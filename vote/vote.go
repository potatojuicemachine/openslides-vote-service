package vote

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/OpenSlides/openslides-go/datastore/dsfetch"
	"github.com/OpenSlides/openslides-go/datastore/dskey"
	"github.com/OpenSlides/openslides-go/datastore/dsmodels"
	"github.com/OpenSlides/openslides-go/datastore/flow"
	"github.com/OpenSlides/openslides-go/environment"
	"github.com/OpenSlides/openslides-go/perm"
	"github.com/OpenSlides/openslides-vote-service/vote/method"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/shopspring/decimal"
)

var envVoteSecretKeyFile = environment.NewVariable("VOTE_SECRET_KEY_FILE", "/run/secrets/vote_secret_key", "Path to the secret key for secret polls.")

// Vote holds the state of the service.
//
// Vote has to be initializes with vote.New().
type Vote struct {
	flow              flow.Flow
	querier           DBQuerier
	gcmForSecretPolls cipher.AEAD
}

// New creates an initializes vote service.
func New(lookup environment.Environmenter, flow flow.Flow, querier DBQuerier) (*Vote, func(context.Context, func(error)), error) {
	key, err := environment.ReadSecret(lookup, envVoteSecretKeyFile)
	if err != nil {
		return nil, nil, fmt.Errorf("read secret key: %w", err)
	}

	hashedKey := sha256.Sum256([]byte(key))
	block, err := aes.NewCipher(hashedKey[:])
	if err != nil {
		return nil, nil, fmt.Errorf("create cipher for secret polls: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("create GCM for secret polls: %w", err)
	}

	v := &Vote{
		flow:              flow,
		querier:           querier,
		gcmForSecretPolls: gcm,
	}

	bg := func(ctx context.Context, errorHandler func(error)) {
		v.flow.Update(ctx, func(changedData map[dskey.Key][]byte, err error) {
			if err != nil {
				errorHandler(err)
			}

			// This listens on the message bus to see, if a poll got started. If
			// so, it preloads its data. This is only relevant, if a poll gets
			// started on another instance.
			for key, value := range changedData {
				if key.CollectionField() == "poll/state" && string(value) == `"started"` {
					poll, err := dsmodels.New(v.flow).Poll(key.ID()).First(ctx)
					if err != nil {
						errorHandler(fmt.Errorf("Error fetching poll for preload: %w", err))
						continue
					}
					if err := Preload(ctx, dsfetch.New(v.flow), poll.ID, poll.MeetingID); err != nil {
						errorHandler(fmt.Errorf("Error preloading poll: %w", err))
						continue
					}
				}
			}
		})
	}

	return v, bg, nil
}

// Create create a poll, returning the poll id.
func (v *Vote) Create(ctx context.Context, requestUserID int, r io.Reader) (int, error) {
	electronicVotingEnabled, err := dsfetch.New(v.flow).Organization_EnableElectronicVoting(1).Value(ctx)
	if err != nil {
		return 0, fmt.Errorf("fetch organization/1/enable_electronic_voting: %w", err)
	}

	ci, err := parseCreateInput(r, electronicVotingEnabled)
	if err != nil {
		return 0, fmt.Errorf("parsing input: %w", err)
	}

	if err := canManagePoll(ctx, v.flow, ci.MeetingID, ci.ContentObjectID, requestUserID); err != nil {
		return 0, fmt.Errorf("check permissions: %w", err)
	}

	tx, err := v.querier.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	configID, err := method.SaveConfig(ctx, tx, ci.Method, ci.MethodConfig)
	if err != nil {
		return 0, fmt.Errorf("save poll config: %w", err)
	}

	state := "created"
	if ci.Visibility == "manually" {
		state = "finished"
	}

	sql := `INSERT INTO poll
		(title, config_id, visibility, state, content_object_id, meeting_id, result, live_voting_enabled, allow_vote_split)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id;`

	var newID int
	if err := tx.QueryRow(
		ctx,
		sql,
		ci.Title,
		configID,
		ci.Visibility,
		state,
		ci.ContentObjectID,
		ci.MeetingID,
		string(ci.Result),
		ci.LiveVotingEnabled,
		ci.AllowVoteSplit,
	).Scan(&newID); err != nil {
		return 0, fmt.Errorf("save poll: %w", err)
	}

	if len(ci.Options) > 0 {
		if err := saveOptions(ctx, tx, newID, ci.OptionType, ci.Options); err != nil {
			return 0, fmt.Errorf("save options: %w", err)
		}
	}

	if len(ci.EntitledGroupIDs) > 0 {
		placeholders := make([]string, len(ci.EntitledGroupIDs))
		args := make([]any, len(ci.EntitledGroupIDs)*2)

		for i, groupID := range ci.EntitledGroupIDs {
			placeholders[i] = fmt.Sprintf("($%d, $%d)", i*2+1, i*2+2)
			args[i*2] = groupID
			args[i*2+1] = newID
		}

		groupSQL := fmt.Sprintf(
			"INSERT INTO nm_group_poll_ids_poll_t (group_id, poll_id) VALUES %s",
			strings.Join(placeholders, ", "),
		)

		if _, err := tx.Exec(ctx, groupSQL, args...); err != nil {
			return 0, fmt.Errorf("insert group-poll relations: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit transaction: %w", err)
	}

	return newID, nil
}

func saveOptions(ctx context.Context, tx pgx.Tx, pollID int, oType string, optionList []json.RawMessage) error {
	for i, option := range optionList {
		switch oType {
		case "text":
			var textOption string
			if err := json.Unmarshal(option, &textOption); err != nil {
				return errors.Join(fmt.Errorf("decode text option: %w", err), MessageError(ErrInvalid, "Invalid option"))
			}

			sql := `INSERT INTO poll_option
				(poll_id, weight, text)
				VALUES ($1, $2, $3);`

			if _, err := tx.Exec(ctx, sql, pollID, i+1, textOption); err != nil {
				return fmt.Errorf("insert options: %w", err)
			}
		case "meeting_user":
			var meetingUserOption int
			if err := json.Unmarshal(option, &meetingUserOption); err != nil {
				return errors.Join(fmt.Errorf("decode meeting_user option: %w", err), MessageError(ErrInvalid, "Invalid option"))
			}

			sql := `INSERT INTO poll_option
			(poll_id, weight, meeting_user_id)
			VALUES ($1, $2, $3);`

			if _, err := tx.Exec(ctx, sql, pollID, i+1, meetingUserOption); err != nil {
				return fmt.Errorf("insert options: %w", err)
			}
		default:
			return MessageErrorf(ErrInvalid, "Invalid option_type %s", oType)
		}
	}
	return nil
}

type createInput struct {
	Title             string            `json:"title"`
	ContentObjectID   string            `json:"content_object_id"`
	MeetingID         int               `json:"meeting_id"`
	Method            string            `json:"method"`
	MethodConfig      json.RawMessage   `json:"method_config"`
	OptionType        string            `json:"option_type"`
	Options           []json.RawMessage `json:"options"`
	Visibility        string            `json:"visibility"`
	EntitledGroupIDs  []int             `json:"entitled_group_ids"`
	LiveVotingEnabled bool              `json:"live_voting_enabled"`
	Result            json.RawMessage   `json:"result"`
	AllowVoteSplit    bool              `json:"allow_vote_split"`
}

func parseCreateInput(r io.Reader, electronicVotingEnabled bool) (createInput, error) {
	var ci createInput
	if err := json.NewDecoder(r).Decode(&ci); err != nil {
		return createInput{}, MessageError(ErrInvalid, "Request body is not a valid json object.")
	}

	if ci.Title == "" {
		return createInput{}, MessageError(ErrInvalid, "Field 'title' can not be empty")
	}

	if ci.ContentObjectID == "" {
		return createInput{}, MessageError(ErrInvalid, "Field 'content_object_id' can not be empty")
	}

	if ci.MeetingID == 0 {
		return createInput{}, MessageError(ErrInvalid, "Field 'meeting_id' can not be empty")
	}

	if ci.Method == "" {
		return createInput{}, MessageError(ErrInvalid, "Field 'method' can not be empty")
	}

	requireOptions, err := method.RequireOptions(ci.Method)
	if err != nil {
		return createInput{}, fmt.Errorf("getting method: %w", err)
	}

	if requireOptions && len(ci.Options) == 0 {
		return createInput{}, MessageErrorf(ErrInvalid, "Method %s needs at least one option", ci.Method)
	}

	if ci.MethodConfig == nil {
		return createInput{}, MessageError(ErrInvalid, "Field 'config' can not be empty")
	}

	if ci.OptionType == "" && len(ci.Options) != 0 {
		return createInput{}, MessageError(ErrInvalid, "Field `option_type` has to be set, if options are given")
	}

	if ci.Visibility == "" {
		return createInput{}, MessageError(ErrInvalid, "Field 'visibility' can not be empty")
	}

	if ci.Visibility == "secret" && ci.AllowVoteSplit {
		return createInput{}, MessageError(ErrInvalid, "Vote splitting is not allowed for secret polls")
	}

	switch ci.Visibility {
	case "manually":
		if len(ci.EntitledGroupIDs) > 0 {
			return createInput{}, MessageError(ErrInvalid, "Entitled Group IDs can not be set when visibility is set to manually")
		}

	default:
		if !electronicVotingEnabled {
			return createInput{}, MessageError(ErrNotAllowed, "Electronic voting is not enabled. Only polls with visibility set to manually are allowed.")
		}

		if ci.Result != nil {
			return createInput{}, MessageError(ErrInvalid, "Result can only be set when visibility is set to manually")
		}
	}

	return ci, nil
}

// Update changes a poll.
func (v *Vote) Update(ctx context.Context, pollID int, requestUserID int, r io.Reader) error {
	poll, err := fetchPoll(ctx, v.flow, pollID)
	if err != nil {
		return fmt.Errorf("fetching poll: %w", err)
	}

	if err := canManagePoll(ctx, v.flow, poll.MeetingID, poll.ContentObjectID, requestUserID); err != nil {
		return fmt.Errorf("check permissions: %w", err)
	}

	electronicVotingEnabled, err := dsfetch.New(v.flow).Organization_EnableElectronicVoting(1).Value(ctx)
	if err != nil {
		return fmt.Errorf("fetch organization/1/enable_electronic_voting: %w", err)
	}

	ui, err := parseUpdateInput(r, poll, electronicVotingEnabled)
	if err != nil {
		return fmt.Errorf("parse update body: %w", err)
	}

	tx, err := v.querier.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	sql, values := ui.query(pollID)
	if len(values) > 0 {
		if _, err := tx.Exec(ctx, sql, values...); err != nil {
			return fmt.Errorf("update poll: %w", err)
		}
	}

	if ui.Method != "" || ui.MethodConfig != nil {
		m, err := pollMethod(poll)
		if err != nil {
			return fmt.Errorf("getting poll method for poll %d: %w", poll.ID, err)
		}

		if ui.Method != "" {
			m = ui.Method
		}

		if err := method.DeleteConfig(ctx, tx, pollID, poll.ConfigID); err != nil {
			return fmt.Errorf("delete old config: %w", err)
		}

		newConfigID, err := method.SaveConfig(ctx, tx, m, ui.MethodConfig)
		if err != nil {
			return fmt.Errorf("save poll config: %w", err)
		}

		sql := `UPDATE poll_t SET config_id = $1 WHERE id = $2`
		if _, err := tx.Exec(ctx, sql, newConfigID, pollID); err != nil {
			return fmt.Errorf("update poll.config_id: %w", err)
		}
	}

	if ui.OptionType != "" && len(ui.Options) > 0 {
		sql := "DELETE FROM poll_option WHERE poll_id = $1"
		if _, err := tx.Exec(ctx, sql, pollID); err != nil {
			return fmt.Errorf("deleting existing poll options: %w", err)
		}

		if err := saveOptions(ctx, tx, pollID, ui.OptionType, ui.Options); err != nil {
			return fmt.Errorf("save options: %w", err)
		}
	}

	if len(ui.EntitledGroupIDs) > 0 {
		sql := "DELETE FROM nm_group_poll_ids_poll_t WHERE poll_id = $1"
		if _, err := tx.Exec(ctx, sql, pollID); err != nil {
			return fmt.Errorf("deleting existing group associations: %w", err)
		}

		placeholders := make([]string, len(ui.EntitledGroupIDs))
		args := make([]any, len(ui.EntitledGroupIDs)*2)

		for i, groupID := range ui.EntitledGroupIDs {
			placeholders[i] = fmt.Sprintf("($%d, $%d)", i*2+1, i*2+2)
			args[i*2] = groupID
			args[i*2+1] = poll.ID
		}

		groupSQL := fmt.Sprintf(
			"INSERT INTO nm_group_poll_ids_poll_t (group_id, poll_id) VALUES %s",
			strings.Join(placeholders, ", "),
		)

		if _, err := tx.Exec(ctx, groupSQL, args...); err != nil {
			return fmt.Errorf("insert group-poll relations: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

type updateInput struct {
	Title             string              `json:"title"`
	Method            string              `json:"method"`
	MethodConfig      json.RawMessage     `json:"method_config"`
	OptionType        string              `json:"option_type"`
	Options           []json.RawMessage   `json:"options"`
	Visibility        string              `json:"visibility"`
	EntitledGroupIDs  []int               `json:"entitled_group_ids"`
	LiveVotingEnabled dsfetch.Maybe[bool] `json:"live_voting_enabled"`
	Result            json.RawMessage     `json:"result"`
	AllowVoteSplit    dsfetch.Maybe[bool] `json:"allow_vote_split"`
}

func parseUpdateInput(r io.Reader, poll dsmodels.Poll, electronicVotingEnabled bool) (updateInput, error) {
	var ui updateInput
	if err := json.NewDecoder(r).Decode(&ui); err != nil {
		return updateInput{}, fmt.Errorf("decoding update input: %w", err)
	}

	if poll.Visibility == "manually" {
		if len(ui.EntitledGroupIDs) > 0 {
			return updateInput{}, MessageError(ErrNotAllowed, "Entitled Group IDs can not be set when visibility is set to manually")
		}
		return ui, nil
	}

	if ui.Visibility == "manually" {
		return updateInput{}, MessageError(ErrNotAllowed, "A poll can not be changed manually")
	}

	if poll.State != "created" {
		if ui.Method != "" {
			return updateInput{}, MessageError(ErrNotAllowed, "method can only be changed before the poll has started")
		}

		if ui.MethodConfig != nil {
			return updateInput{}, MessageError(ErrNotAllowed, "config can only be changed before the poll has started")
		}

		if ui.OptionType != "" {
			return updateInput{}, MessageError(ErrNotAllowed, "option type can only be changed before the poll has started")
		}

		if len(ui.Options) != 0 {
			return updateInput{}, MessageErrorf(ErrInvalid, "options can only be changed before the poll has started")
		}

		if ui.Visibility != "" {
			return updateInput{}, MessageError(ErrNotAllowed, "visibility can only be changed before the poll has started")
		}

		if ui.EntitledGroupIDs != nil {
			return updateInput{}, MessageError(ErrNotAllowed, "entitled group ids can only be changed before the poll has started")
		}

		if !ui.AllowVoteSplit.Null() {
			return updateInput{}, MessageError(ErrNotAllowed, "allow vote split can only be changed before the poll has started")
		}
	}

	if !electronicVotingEnabled {
		return updateInput{}, MessageError(ErrNotAllowed, "Electronic voting is not enabled. Only polls with visibility set to manually are allowed.")
	}

	if ui.Result != nil {
		return updateInput{}, MessageError(ErrNotAllowed, "Result can only be set when visibility is set to manually")
	}

	return ui, nil
}

func (ui updateInput) query(pollID int) (string, []any) {
	var setParts []string
	var args []any
	argIndex := 1

	if ui.Title != "" {
		setParts = append(setParts, fmt.Sprintf("title = $%d", argIndex))
		args = append(args, ui.Title)
		argIndex++
	}

	if ui.Visibility != "" {
		setParts = append(setParts, fmt.Sprintf("visibility = $%d", argIndex))
		args = append(args, ui.Visibility)
		argIndex++
	}

	if liveVoting, hasValue := ui.LiveVotingEnabled.Value(); hasValue {
		setParts = append(setParts, fmt.Sprintf("live_voting_enabled = $%d", argIndex))
		args = append(args, liveVoting)
		argIndex++
	}

	if ui.Result != nil {
		setParts = append(setParts, fmt.Sprintf("result = $%d", argIndex))
		args = append(args, string(ui.Result))
		argIndex++
	}

	if allowVoteSplit, hasValue := ui.AllowVoteSplit.Value(); hasValue {
		setParts = append(setParts, fmt.Sprintf("allow_vote_split = $%d", argIndex))
		args = append(args, allowVoteSplit)
		argIndex++
	}

	if len(setParts) == 0 {
		return "", nil
	}

	query := fmt.Sprintf("UPDATE poll SET %s WHERE id = $%d",
		strings.Join(setParts, ", "),
		argIndex)

	args = append(args, pollID)

	return query, args
}

// Delete removes a poll.
func (v *Vote) Delete(ctx context.Context, pollID int, requestUserID int) error {
	poll, err := fetchPoll(ctx, v.flow, pollID)
	if err != nil {
		return fmt.Errorf("fetching poll: %w", err)
	}

	if err := canManagePoll(ctx, v.flow, poll.MeetingID, poll.ContentObjectID, requestUserID); err != nil {
		return fmt.Errorf("check permissions: %w", err)
	}

	tx, err := v.querier.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	deleteStatements := []string{
		`DELETE FROM poll_ballot WHERE poll_id = $1`,
		`DELETE FROM poll_option WHERE poll_id = $1`,
	}
	for _, sql := range deleteStatements {
		if _, err := tx.Exec(ctx, sql, pollID); err != nil {
			return fmt.Errorf("remove old config entries for poll %d: %w", pollID, err)
		}
	}

	if err := method.DeleteConfig(ctx, tx, pollID, poll.ConfigID); err != nil {
		return fmt.Errorf("delete method: %w", err)
	}

	sql := `DELETE FROM poll WHERE id = $1;`
	if _, err := tx.Exec(ctx, sql, pollID); err != nil {
		return fmt.Errorf("delete poll: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

// Start validates a poll and set its state to started.
func (v *Vote) Start(ctx context.Context, pollID int, requestUserID int) error {
	poll, err := fetchPoll(ctx, v.flow, pollID)
	if err != nil {
		return fmt.Errorf("fetching poll: %w", err)
	}

	if err := canManagePoll(ctx, v.flow, poll.MeetingID, poll.ContentObjectID, requestUserID); err != nil {
		return fmt.Errorf("check permissions: %w", err)
	}

	if poll.State == "finished" {
		return MessageErrorf(ErrInvalid, "Poll %d is already finished", pollID)
	}

	if err := Preload(ctx, dsfetch.New(v.flow), poll.ID, poll.MeetingID); err != nil {
		return fmt.Errorf("preloading poll: %w", err)
	}

	sql := `UPDATE poll SET state = 'started', published = $2 WHERE id = $1 AND state != 'finished';`
	commandTag, err := v.querier.Exec(ctx, sql, pollID, poll.LiveVotingEnabled)
	if err != nil {
		return fmt.Errorf("set poll %d to started: %w", pollID, err)
	}

	if commandTag.RowsAffected() != 1 {
		return fmt.Errorf("poll %d not found or not in 'created' state", pollID)
	}

	return nil
}

// Finalize ends a poll.
//
// - If in the started state, it creates poll/result.
// - Sets the state to `finished`.
// - Sets the `published` flag.
// - With the flag `anonymize`, clears all user_ids from the coresponding votes.
func (v *Vote) Finalize(ctx context.Context, pollID int, requestUserID int, publish bool, anonymize bool) error {
	poll, err := fetchPoll(ctx, v.flow, pollID)
	if err != nil {
		return fmt.Errorf("fetching poll: %w", err)
	}

	if err := canManagePoll(ctx, v.flow, poll.MeetingID, poll.ContentObjectID, requestUserID); err != nil {
		return fmt.Errorf("check permissions: %w", err)
	}

	if poll.State == "created" {
		return MessageErrorf(ErrInvalid, "Poll %d has not started yet.", pollID)
	}

	tx, err := v.querier.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	if poll.State == `started` {
		ds := dsmodels.New(v.flow)

		ballots, err := ds.PollBallot(poll.BallotIDs...).Get(ctx)
		if err != nil {
			return fmt.Errorf("fetch votes of poll %d: %w", poll.ID, err)
		}

		if poll.Visibility == "secret" {
			for i := range ballots {
				ballots[i].Value, err = v.decryptBallot(ballots[i].Value)
				if err != nil {
					return fmt.Errorf("decrypting ballot: %w", err)
				}
			}

			// Change the order of the ballots so the new values can not be guessed.
			sort.Slice(ballots, func(i, j int) bool {
				return ballots[i].Value < ballots[j].Value
			})

			// Delete and reinsert old ballots.
			_, err = tx.Exec(ctx, "DELETE FROM poll_ballot WHERE poll_id = $1", poll.ID)
			if err != nil {
				return fmt.Errorf("deleting old ballots: %w", err)
			}

			_, err = tx.CopyFrom(
				ctx,
				pgx.Identifier{"poll_ballot_t"},
				[]string{"weight", "split", "value", "poll_id"},
				pgx.CopyFromSlice(len(ballots), func(i int) ([]any, error) {
					return []any{
						ballots[i].Weight,
						ballots[i].Split,
						ballots[i].Value,
						poll.ID,
					}, nil
				}),
			)
			if err != nil {
				return fmt.Errorf("bulk inserting anonymized ballots: %w", err)
			}
		}

		pm, err := v.resolveMethod(ctx, poll)
		if err != nil {
			return fmt.Errorf("resolve poll method: %w", err)
		}

		result, err := CreateResult(pm, poll.AllowVoteSplit, ballots)
		if err != nil {
			return fmt.Errorf("create poll result: %w", err)
		}

		votedMeetingUserIDs := make([]int, len(ballots))
		for i, vote := range ballots {
			meetingUserID, set := vote.RepresentedMeetingUserID.Value()
			if !set {
				return fmt.Errorf("vote %d has no representedMeetingUserID", vote.ID)
			}
			votedMeetingUserIDs[i] = meetingUserID
		}

		sql := `UPDATE poll SET result = $1 WHERE id = $2;`
		if _, err := tx.Exec(ctx, sql, result, pollID); err != nil {
			return fmt.Errorf("set result of poll %d: %w", pollID, err)
		}

		if len(votedMeetingUserIDs) > 0 {
			placeholders := make([]string, len(votedMeetingUserIDs))
			args := make([]any, len(votedMeetingUserIDs)*2)

			for i, votedUserID := range votedMeetingUserIDs {
				placeholders[i] = fmt.Sprintf("($%d, $%d)", i*2+1, i*2+2)
				args[i*2] = votedUserID
				args[i*2+1] = pollID
			}

			votedSQL := fmt.Sprintf(
				"INSERT INTO nm_meeting_user_poll_voted_ids_poll_t (meeting_user_id, poll_id) VALUES %s",
				strings.Join(placeholders, ", "),
			)

			if _, err := tx.Exec(ctx, votedSQL, args...); err != nil {
				return fmt.Errorf("insert voted_user_ids to meeting_user relations: %w", err)
			}
		}
	}

	sql := `UPDATE poll SET state = 'finished', published = $1 WHERE id = $2;`
	if _, err := tx.Exec(ctx, sql, publish, pollID); err != nil {
		return fmt.Errorf("set poll %d to finished and publish to %v: %w", pollID, publish, err)
	}

	if anonymize {
		if poll.Visibility == "named" {
			return MessageError(ErrNotAllowed, "A named-poll can not be anonymized.")
		}

		sqlBallots := `
		UPDATE poll_ballot
		SET acting_meeting_user_id = NULL, represented_meeting_user_id = NULL
		WHERE poll_id = $1`
		if _, err := tx.Exec(ctx, sqlBallots, pollID); err != nil {
			return fmt.Errorf("anonymize ballots: %w", err)
		}

		sqlPoll := `
        UPDATE poll
        SET anonymized = TRUE
        WHERE id = $1`
		if _, err := tx.Exec(ctx, sqlPoll, pollID); err != nil {
			return fmt.Errorf("set anonymize on poll: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

// Reset removes all votes from a poll and sets its state to created.
func (v *Vote) Reset(ctx context.Context, pollID int, requestUserID int) error {
	poll, err := fetchPoll(ctx, v.flow, pollID)
	if err != nil {
		return fmt.Errorf("fetching poll: %w", err)
	}

	if err := canManagePoll(ctx, v.flow, poll.MeetingID, poll.ContentObjectID, requestUserID); err != nil {
		return fmt.Errorf("check permissions: %w", err)
	}

	tx, err := v.querier.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var exists bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM poll WHERE id = $1)`, pollID).Scan(&exists)
	if err != nil {
		return fmt.Errorf("check poll existence: %w", err)
	}

	if !exists {
		return MessageErrorf(ErrInvalid, "Poll with id %d not found", pollID)
	}

	deleteVoteQuery := `DELETE FROM poll_ballot WHERE poll_id = $1`
	if _, err := tx.Exec(ctx, deleteVoteQuery, pollID); err != nil {
		return fmt.Errorf("delete ballots: %w", err)
	}

	state := "created"
	if poll.Visibility == "manually" {
		state = "finished"
	}

	updateQuery := `UPDATE poll SET state = $1, published = false, result = '', anonymized = FALSE WHERE id = $2`
	if _, err := tx.Exec(ctx, updateQuery, state, pollID); err != nil {
		return fmt.Errorf("reset poll state: %w", err)
	}

	deleteVotedQuery := `DELETE FROM nm_meeting_user_poll_voted_ids_poll_t WHERE poll_id = $1`
	if _, err := tx.Exec(ctx, deleteVotedQuery, pollID); err != nil {
		return fmt.Errorf("delete poll votes: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

// Vote validates and saves the vote.
func (v *Vote) Vote(ctx context.Context, pollID, requestUserID int, r io.Reader) error {
	if requestUserID == 0 {
		return MessageErrorf(ErrInvalid, "Anonymous can not vote")
	}

	poll, err := fetchPoll(ctx, v.flow, pollID)
	if err != nil {
		return fmt.Errorf("fetching poll: %w", err)
	}

	var body struct {
		MeetingUserID dsfetch.Maybe[int] `json:"meeting_user_id"`
		Value         json.RawMessage    `json:"value"`
		Split         bool               `json:"split"`
	}

	if err := json.NewDecoder(r).Decode(&body); err != nil {
		return MessageError(ErrInvalid, "Invalid body")
	}

	if body.Split && !poll.AllowVoteSplit {
		return MessageErrorf(ErrInvalid, "Vote split is not allowed for poll %d", poll.ID)
	}

	fetch := dsfetch.New(v.flow)
	actingMeetingUserID, found, err := getMeetingUser(ctx, fetch, requestUserID, poll.MeetingID)
	if err != nil {
		return fmt.Errorf("getting meeting user of request user: %w", err)
	}
	if !found {
		return MessageErrorf(ErrInvalid, "You have to be in the meeting to vote")
	}

	representedMeetingUserID := actingMeetingUserID
	if meetingUserID, set := body.MeetingUserID.Value(); set {
		representedMeetingUserID = meetingUserID
	}

	if err := allowedToVote(ctx, fetch, poll, representedMeetingUserID, actingMeetingUserID); err != nil {
		return fmt.Errorf("allowedToVote: %w", err)
	}

	ballotValue := string(body.Value)
	if poll.Visibility == "secret" {
		ballotValue, err = v.encryptBallot(ballotValue)
		if err != nil {
			return fmt.Errorf("encrypting ballot value: %w", err)
		}
	}

	weight, err := CalcVoteWeight(ctx, fetch, representedMeetingUserID)
	if err != nil {
		return fmt.Errorf("calc vote weight: %w", err)
	}

	if !poll.AllowInvalid {
		splitted := map[decimal.Decimal]json.RawMessage{decimal.Zero: body.Value}

		if body.Split {
			splitted, err = split(weight, body.Value)
			if err != nil {
				return fmt.Errorf("split vote: %w", err)
			}
		}

		pm, err := v.resolveMethod(ctx, poll)
		if err != nil {
			return fmt.Errorf("resolve poll method: %w", err)
		}

		for _, value := range splitted {
			if err := pm.ValidateBallot(value); err != nil {
				return fmt.Errorf("validate ballot: %w", err)
			}
		}
	}

	sql := `WITH
		poll_check AS (
			SELECT
				id,
				state,
				CASE
					WHEN id IS NULL THEN 'POLL_NOT_FOUND'
					WHEN state != 'started' THEN 'POLL_NOT_STARTED'
					ELSE 'POLL_VALID'
				END as poll_status
			FROM poll
			WHERE id = $1
		),
		ballot_check AS (
			SELECT
				COUNT(*) as existing_ballots,
				CASE
					WHEN COUNT(*) > 0 THEN 'USER_HAS_VOTED_BEFORE'
					ELSE 'BALLOT_OK'
				END as ballot_status
			FROM poll_ballot
			WHERE poll_id = $1 AND represented_meeting_user_id = $5
		),
		inserted AS (
			INSERT INTO poll_ballot
			(poll_id, value, weight, acting_meeting_user_id, represented_meeting_user_id)
			SELECT $1, $2, $3, $4, $5
			FROM poll_check p, ballot_check b
			WHERE p.poll_status = 'POLL_VALID' AND b.ballot_status = 'BALLOT_OK'
			RETURNING id
		)
		SELECT
			CASE
				WHEN i.id IS NOT NULL THEN 'VALID'
				WHEN p.poll_status != 'POLL_VALID' THEN p.poll_status
				WHEN b.ballot_status != 'BALLOT_OK' THEN b.ballot_status
				ELSE 'UNKNOWN_ERROR'
			END as status
		FROM poll_check p, ballot_check b
		LEFT JOIN inserted i ON true;`

	var status string
	err = v.querier.QueryRow(ctx, sql, pollID, ballotValue, weight, actingMeetingUserID, representedMeetingUserID).Scan(
		&status,
	)
	if err != nil {
		return fmt.Errorf("insert ballot: %w", err)
	}

	switch status {
	case "VALID":
		return nil
	case "POLL_NOT_FOUND":
		return MessageErrorf(ErrNotExists, "Poll %d does not exist", pollID)
	case "POLL_NOT_STARTED":
		return MessageErrorf(ErrNotStarted, "Poll %d is not started", pollID)
	case "USER_HAS_VOTED_BEFORE":
		return MessageErrorf(ErrDoubleVote, "You can not vote again on poll %d", pollID)
	default:
		return fmt.Errorf("unknown vote sql status %s", status)
	}
}

// encryptBallot encrypts the given value with AES using the key for secret polls.
func (v *Vote) encryptBallot(ballotValue string) (string, error) {
	nonce := make([]byte, v.gcmForSecretPolls.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("create nonce: %w", err)
	}

	encryptedValue := v.gcmForSecretPolls.Seal(nonce, nonce, []byte(ballotValue), nil)

	return base64.StdEncoding.EncodeToString(encryptedValue), nil
}

// decryptBallot decrypt the given value with AES using the key for secret polls.
func (v *Vote) decryptBallot(encryptedBallot string) (string, error) {
	encryptedValue, err := base64.StdEncoding.DecodeString(encryptedBallot)
	if err != nil {
		return "", fmt.Errorf("base64 decode encrypted ballot: %w", err)
	}

	nonceSize := v.gcmForSecretPolls.NonceSize()
	if len(encryptedValue) < nonceSize {
		return "", fmt.Errorf("encrypted ballot too short")
	}

	nonce, ciphertext := encryptedValue[:nonceSize], encryptedValue[nonceSize:]

	plaintext, err := v.gcmForSecretPolls.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt ciphertext: %w", err)
	}

	return string(plaintext), nil
}

// resolveMethod returns the poll method for a poll.
func (v *Vote) resolveMethod(ctx context.Context, poll dsmodels.Poll) (method.Method, error) {
	method, err := method.ResolveMethod(ctx, v.flow, poll.ConfigID, poll.OptionIDs)
	if err != nil {
		return nil, fmt.Errorf("method.ResolveMethod: %w", err)
	}
	return method, nil
}

func pollMethod(poll dsmodels.Poll) (string, error) {
	configCollection, _, found := strings.Cut(poll.ConfigID, "/")
	if !found {
		return "", fmt.Errorf("poll %d has an invalid config_id: %s", poll.ID, poll.ConfigID)
	}

	m, found := strings.CutPrefix(configCollection, "poll_config_")
	if !found {
		return "", fmt.Errorf("poll %d has an unknown poll config: %s", poll.ID, poll.ConfigID)
	}
	return m, nil
}

// split split sa vote and valides the weight
func split(maxWeight decimal.Decimal, value json.RawMessage) (map[decimal.Decimal]json.RawMessage, error) {
	var splitVotes map[decimal.Decimal]json.RawMessage
	if err := json.Unmarshal(value, &splitVotes); err != nil {
		return nil, errors.Join(MessageError(ErrInvalid, "Invalid split votes"), err)
	}

	var splitWeightSum decimal.Decimal
	for splitWeight := range splitVotes {
		splitWeightSum = splitWeightSum.Add(splitWeight)
	}

	if splitWeightSum.Cmp(maxWeight) == 1 {
		return nil, MessageError(ErrInvalid, "Split weight exceeds your vote weight.")
	}

	return splitVotes, nil
}

// allowedToVote checks, that the represented user can vote and the acting user
// can vote for him.
func allowedToVote(
	ctx context.Context,
	ds *dsfetch.Fetch,
	poll dsmodels.Poll,
	representedMeetingUserID int,
	actingMeetingUserID int,
) error {
	if representedMeetingUserID == 0 {
		return MessageError(ErrNotAllowed, "You can not vote for anonymous.")
	}

	if err := ensurePresent(ctx, ds, actingMeetingUserID); err != nil {
		return fmt.Errorf("ensure acting user %d is present: %w", actingMeetingUserID, err)
	}

	groupIDs, err := ds.MeetingUser_GroupIDs(representedMeetingUserID).Value(ctx)
	if err != nil {
		return fmt.Errorf("fetching groups of meeting_user %d: %w", representedMeetingUserID, err)
	}

	if !hasCommon(groupIDs, poll.EntitledGroupIDs) {
		return MessageErrorf(ErrNotAllowed, "Meeting User %d is not allowed to vote. He is not in an entitled group", representedMeetingUserID)
	}

	delegationActivated, err := ds.Meeting_UsersEnableVoteDelegations(poll.MeetingID).Value(ctx)
	if err != nil {
		return fmt.Errorf("fetching meeting/user_enable_vote_delegations: %w", err)
	}

	forbitDelegateToVote, err := ds.Meeting_UsersForbidDelegatorToVote(poll.MeetingID).Value(ctx)
	if err != nil {
		return fmt.Errorf("fetching meeting/users_forbid_delegator_to_vote: %w", err)
	}

	delegation, err := ds.MeetingUser_VoteDelegatedToID(representedMeetingUserID).Value(ctx)
	if err != nil {
		return fmt.Errorf("fetching meeting_user/vote_delegated_to_id: %w", err)
	}

	if delegationActivated && forbitDelegateToVote && !delegation.Null() && representedMeetingUserID == actingMeetingUserID {
		return MessageError(ErrNotAllowed, "You have delegated your vote and therefore can not vote for your self")
	}

	if representedMeetingUserID == actingMeetingUserID {
		return nil
	}

	if !delegationActivated {
		return MessageErrorf(ErrNotAllowed, "Vote delegation is not activated in meeting %d", poll.MeetingID)
	}

	if id, ok := delegation.Value(); !ok || id != actingMeetingUserID {
		return MessageErrorf(ErrNotAllowed, "You can not vote for meeting user %d", representedMeetingUserID)
	}

	return nil
}

// CalcVoteWeight calculates the vote weight for a user in a meeting.
//
// voteweight is a DecimalField with 6 zeros.
func CalcVoteWeight(ctx context.Context, fetch *dsfetch.Fetch, meetingUserID int) (decimal.Decimal, error) {
	defaultVoteWeight, _ := decimal.NewFromString("1.000000")
	userID, err := fetch.MeetingUser_UserID(meetingUserID).Value(ctx)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("getting user ID from meeting user: %w", err)
	}

	meetingID, err := fetch.MeetingUser_MeetingID(meetingUserID).Value(ctx)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("getting meeting ID from meeting user: %w", err)
	}

	var voteWeightEnabled bool
	var meetingUserVoteWeight decimal.Decimal
	var userDefaultVoteWeight decimal.Decimal
	fetch.Meeting_UsersEnableVoteWeight(meetingID).Lazy(&voteWeightEnabled)
	fetch.MeetingUser_VoteWeight(meetingUserID).Lazy(&meetingUserVoteWeight)
	fetch.User_DefaultVoteWeight(userID).Lazy(&userDefaultVoteWeight)

	if err := fetch.Execute(ctx); err != nil {
		return decimal.Decimal{}, fmt.Errorf("getting vote weight values from db: %w", err)
	}

	if !voteWeightEnabled {
		return defaultVoteWeight, nil
	}

	if !meetingUserVoteWeight.IsZero() {
		return meetingUserVoteWeight, nil
	}

	if !userDefaultVoteWeight.IsZero() {
		return userDefaultVoteWeight, nil
	}

	return defaultVoteWeight, nil
}

// CreateResult creates the result from a list of votes.
func CreateResult(method method.Method, allowVoteSplit bool, ballots []dsmodels.PollBallot) (string, error) {
	if allowVoteSplit {
		ballots = splitVote(method, ballots)
	}

	return method.Result(ballots)
}

func splitVote(method method.Method, ballots []dsmodels.PollBallot) []dsmodels.PollBallot {
	var splittedBallots []dsmodels.PollBallot
	for _, ballot := range ballots {
		if !ballot.Split {
			splittedBallots = append(splittedBallots, ballot)
			continue
		}

		splitted, err := split(ballot.Weight, json.RawMessage(ballot.Value))
		if err != nil {
			// If the ballot value can not be splitted, just use it as value.
			// It will probably be counted as invalid.
			splittedBallots = append(splittedBallots, ballot)
			continue
		}

		splittedBallots = append(splittedBallots, ballotsFromSplitted(method, ballot, splitted)...)
	}
	return splittedBallots
}

func ballotsFromSplitted(method method.Method, ballot dsmodels.PollBallot, splitted map[decimal.Decimal]json.RawMessage) []dsmodels.PollBallot {
	var fromThisBallot []dsmodels.PollBallot
	for splitWeight, splitValue := range splitted {
		if err := method.ValidateBallot(splitValue); err != nil {
			return []dsmodels.PollBallot{ballot}
		}

		fromThisBallot = append(fromThisBallot, dsmodels.PollBallot{
			PollID:                   ballot.PollID,
			Weight:                   splitWeight,
			Value:                    string(splitValue),
			ActingMeetingUserID:      ballot.ActingMeetingUserID,
			RepresentedMeetingUserID: ballot.RepresentedMeetingUserID,
			Split:                    true,
		})
	}
	return fromThisBallot
}

func fetchPoll(ctx context.Context, getter flow.Getter, pollID int) (dsmodels.Poll, error) {
	ds := dsmodels.New(getter)
	poll, err := ds.Poll(pollID).First(ctx)
	if err != nil {
		var doesNotExist dsfetch.DoesNotExistError
		if errors.As(err, &doesNotExist) {
			return dsmodels.Poll{}, MessageErrorf(ErrNotExists, "Poll %d does not exist", pollID)
		}
		return dsmodels.Poll{}, fmt.Errorf("loading poll %d: %w", pollID, err)
	}

	return poll, nil
}

func getMeetingUser(ctx context.Context, fetch *dsfetch.Fetch, userID, meetingID int) (int, bool, error) {
	meetingUserIDs, err := fetch.User_MeetingUserIDs(userID).Value(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("getting all meeting_user ids: %w", err)
	}

	meetingIDs := make([]int, len(meetingUserIDs))
	for i := range meetingUserIDs {
		fetch.MeetingUser_MeetingID(meetingUserIDs[i]).Lazy(&meetingIDs[i])
	}

	if err := fetch.Execute(ctx); err != nil {
		return 0, false, fmt.Errorf("get all meeting IDs: %w", err)
	}

	idx := slices.Index(meetingIDs, meetingID)
	if idx == -1 {
		return 0, false, nil
	}

	return meetingUserIDs[idx], true, nil
}

func canManagePoll(ctx context.Context, getter flow.Getter, meetingID int, contentObjectID string, userID int) error {
	collection, _, found := strings.Cut(contentObjectID, "/")
	if !found {
		return fmt.Errorf("invalid content object id: %s", contentObjectID)
	}

	var requiredPerm perm.TPermission
	switch collection {
	case "motion":
		requiredPerm = perm.MotionCanManagePolls
	case "assignment":
		requiredPerm = perm.AssignmentCanManagePolls
	case "topic":
		requiredPerm = perm.AgendaItemCanManagePolls
	default:
		return fmt.Errorf(
			"invalid content object id %s, only motion, assignment or topic allowed",
			contentObjectID,
		)
	}

	userPerms, err := perm.New(ctx, dsfetch.New(getter), userID, meetingID)
	if err != nil {
		return fmt.Errorf("calculate user permissions: %w", err)
	}

	if !userPerms.Has(requiredPerm) {
		return MessageError(ErrNotAllowed, "You are not allowed to manage a poll")
	}

	return nil
}

func ensurePresent(ctx context.Context, ds *dsfetch.Fetch, meetingUser int) error {
	meetingID, err := ds.MeetingUser_MeetingID(meetingUser).Value(ctx)
	if err != nil {
		return fmt.Errorf("fetching meeting ID: %w", err)
	}

	userID, err := ds.MeetingUser_UserID(meetingUser).Value(ctx)
	if err != nil {
		return fmt.Errorf("fetching user ID: %w", err)
	}

	presentMeetings, err := ds.User_IsPresentInMeetingIDs(userID).Value(ctx)
	if err != nil {
		return fmt.Errorf("fetching is present in meetings: %w", err)
	}

	if !slices.Contains(presentMeetings, meetingID) {
		return MessageErrorf(ErrNotAllowed, "You have to be present in meeting %d", meetingID)
	}

	return nil
}

func hasCommon(list1, list2 []int) bool {
	return slices.ContainsFunc(list1, func(a int) bool {
		return slices.Contains(list2, a)
	})
}

// Preload loads all data in the cache, that is needed later for the vote
// requests.
func Preload(ctx context.Context, flow flow.Getter, pollID int, meetingID int) error {
	ds := dsmodels.New(flow)
	var dummyBool bool
	ds.Meeting_UsersEnableVoteWeight(meetingID).Lazy(&dummyBool)
	ds.Meeting_UsersEnableVoteDelegations(meetingID).Lazy(&dummyBool)
	ds.Meeting_UsersForbidDelegatorToVote(meetingID).Lazy(&dummyBool)

	q := ds.Poll(pollID)
	q = q.Preload(q.EntitledGroupList().MeetingUserList().User())
	q = q.Preload(q.EntitledGroupList().MeetingUserList().VoteDelegatedTo().User())
	poll, err := q.First(ctx)
	if err != nil {
		return fmt.Errorf("fetch preload data: %w", err)
	}

	configCollection, configIDStr, found := strings.Cut(poll.ConfigID, "/")
	if !found {
		return fmt.Errorf("invalid value in configID: %s", poll.ConfigID)
	}

	configID, err := strconv.Atoi(configIDStr)
	if err != nil {
		return fmt.Errorf("invalid value in configID. Second part has to be an int: %s", poll.ConfigID)
	}

	switch configCollection {
	case "poll_config_approval":
		_, err := ds.PollConfigApproval(configID).First(ctx)
		if err != nil {
			return fmt.Errorf("fetch poll config approval: %w", err)
		}

	case "poll_config_selection":
		_, err := ds.PollConfigSelection(configID).First(ctx)
		if err != nil {
			return fmt.Errorf("fetch poll config selection: %w", err)
		}

	case "poll_config_rating_score":
		_, err := ds.PollConfigRatingScore(configID).First(ctx)
		if err != nil {
			return fmt.Errorf("fetch poll config rating score: %w", err)
		}

	case "poll_config_rating_approval":
		_, err := ds.PollConfigRatingApproval(configID).First(ctx)
		if err != nil {
			return fmt.Errorf("fetch poll config rating approval: %w", err)
		}

	default:
		return fmt.Errorf("invalid config collection. Unknown method: %s", configCollection)

	}

	return nil
}

// DBQuerier is either a pgx-connection or a pgx-pool.
type DBQuerier interface {
	Begin(ctx context.Context) (pgx.Tx, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func valueOrZero(n dsfetch.Maybe[int]) int {
	value, set := n.Value()
	if set {
		return value
	}
	return 0
}
