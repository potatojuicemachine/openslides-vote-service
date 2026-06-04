package method

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/OpenSlides/openslides-go/datastore/dsfetch"
	"github.com/OpenSlides/openslides-go/datastore/dsmodels"
	"github.com/OpenSlides/openslides-go/datastore/flow"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

// Method is an interface to handle the method of a poll.
type Method interface {
	Name() string
	ValidateBallot(ballot json.RawMessage) error
	Result(votes []dsmodels.PollBallot) (string, error)
	RequireOptions() bool
}

// ResolveMethod returns the method object for an poll.
func ResolveMethod(ctx context.Context, getter flow.Getter, configStr string, optionIDs []int) (Method, error) {
	configCollection, configIDStr, found := strings.Cut(configStr, "/")
	if !found {
		return nil, fmt.Errorf("invalid config_id: %s", configStr)
	}

	configID, err := strconv.Atoi(configIDStr)
	if err != nil {
		return nil, fmt.Errorf("invalid config_id. Second part is not a number: %s", configStr)
	}

	dsm := dsmodels.New(getter)

	switch configCollection {
	case "poll_config_approval":
		configDB, err := dsm.PollConfigApproval(configID).First(ctx)
		if err != nil {
			return nil, fmt.Errorf("fetching poll_config_approval: %w", err)
		}

		return ApprovalFromDB(configDB), nil

	case "poll_config_selection":
		configDB, err := dsm.PollConfigSelection(configID).First(ctx)
		if err != nil {
			return nil, fmt.Errorf("fetching poll_config_selection: %w", err)
		}

		return SelectionFromDB(configDB, optionIDs), nil

	case "poll_config_rating_score":
		configDB, err := dsm.PollConfigRatingScore(configID).First(ctx)
		if err != nil {
			return nil, fmt.Errorf("fetching poll_config_rating_score: %w", err)
		}

		return RatingScoreFromDB(configDB, optionIDs), nil

	case "poll_config_rating_approval":
		configDB, err := dsm.PollConfigRatingApproval(configID).First(ctx)
		if err != nil {
			return nil, fmt.Errorf("fetching poll_config_rating_approval: %w", err)
		}

		return RatingApprovalFromDB(configDB, optionIDs), nil

	default:
		return nil, fmt.Errorf("unknown poll config: %s", configStr)
	}
}

// SaveConfig saves the configuration for a given vote method.
func SaveConfig(ctx context.Context, tx pgx.Tx, method string, config json.RawMessage) (string, error) {
	switch method {
	case Approval{}.Name():
		return approvalSaveConfig(ctx, tx, config)
	case Selection{}.Name():
		return selectionSaveConfig(ctx, tx, config)
	case RatingScore{}.Name():
		return ratingScoreSaveConfig(ctx, tx, config)
	case RatingApproval{}.Name():
		return ratingApprovalSaveConfig(ctx, tx, config)
	default:
		return "", fmt.Errorf("unknown method: %s", method)
	}
}

// DeleteConfig deletes the configuration for a given vote method.
func DeleteConfig(ctx context.Context, tx pgx.Tx, pollID int, configID string) error {
	parts := strings.SplitN(configID, "/", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid config_id: %s", configID)
	}
	configType, configIDStr := parts[0], parts[1]

	var configTable string
	switch configType {
	case "poll_config_approval":
		configTable = "poll_config_approval_t"
	case "poll_config_selection":
		configTable = "poll_config_selection_t"
	case "poll_config_rating_score":
		configTable = "poll_config_rating_score_t"
	case "poll_config_rating_approval":
		configTable = "poll_config_rating_approval_t"
	default:
		return fmt.Errorf("unknown config type: %s", configType)
	}

	if _, err := tx.Exec(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE id = $1`, configTable),
		configIDStr,
	); err != nil {
		return fmt.Errorf("delete table %s from postgres: %w", configTable, err)
	}

	return nil
}

func RequireOptions(methodStr string) (bool, error) {
	method, err := methodFromString(methodStr)
	if err != nil {
		return false, fmt.Errorf("getting method from string %s: %w", methodStr, err)
	}

	return method.RequireOptions(), nil
}

func methodFromString(methodStr string) (Method, error) {
	switch methodStr {
	case Approval{}.Name():
		return &Approval{}, nil
	case Selection{}.Name():
		return &Selection{}, nil
	case RatingScore{}.Name():
		return &RatingScore{}, nil
	case RatingApproval{}.Name():
		return &RatingApproval{}, nil
	default:
		return nil, fmt.Errorf("unknown method: %s", methodStr)
	}
}

const (
	keyAbstain      = "abstain"
	keyNota         = "nota"
	keyInvalid      = "invalid"
	keyTotalBallots = "total_ballots"
)

var reservedOptionNames = []string{keyAbstain, keyNota, keyInvalid, keyTotalBallots}

func addInvalidAndTotalBallots(result []byte, totalBallots, invalid int) ([]byte, error) {
	var data map[string]any
	if err := json.Unmarshal(result, &data); err != nil {
		return nil, err
	}

	if invalid != 0 {
		data[keyInvalid] = invalid
	}
	data[keyTotalBallots] = totalBallots

	return json.Marshal(data)
}

func iterateValues(
	m Method,
	votes []dsmodels.PollBallot,
	fn func(value string, weight decimal.Decimal, result map[string]decimal.Decimal) error,
) (string, error) {
	result := make(map[string]decimal.Decimal)
	invalid := 0
	for _, vote := range votes {
		if err := m.ValidateBallot(json.RawMessage(vote.Value)); err != nil {
			if _, ok := errors.AsType[InvalidBallotError](err); ok {
				invalid++
				continue
			}
			return "", fmt.Errorf("validating vote: %w", err)
		}

		factor := vote.Weight
		if factor.IsZero() {
			factor = decimal.NewFromInt(1)
		}

		if err := fn(vote.Value, factor, result); err != nil {
			return "", fmt.Errorf("prcess: %w", err)
		}
	}

	encodedResult, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("encode result: %w", err)
	}

	withInvalidAndTotalBallots, err := addInvalidAndTotalBallots(encodedResult, len(votes), invalid)
	if err != nil {
		return "", fmt.Errorf("add invalid: %w", err)
	}

	return string(withInvalidAndTotalBallots), nil
}

func hasDuplicates[T comparable](slice []T) bool {
	seen := make(map[T]struct{}, len(slice))
	for _, v := range slice {
		if _, ok := seen[v]; ok {
			return true
		}
		seen[v] = struct{}{}
	}
	return false
}

func maybeZeroIsNull(n int) dsfetch.Maybe[int] {
	if n == 0 {
		return dsfetch.Maybe[int]{}
	}

	return dsfetch.MaybeValue(n)
}

// TODO: Maybe find a way to directly implement this in the maybe type, so pgx
// can understand it.
func maybeNullIsNil(n dsfetch.Maybe[int]) any {
	v, hasValue := n.Value()
	if !hasValue {
		return nil
	}
	return v
}

type invalidConfigError struct {
	msg string
}

func invalidConfig(msg string, a ...any) invalidConfigError {
	return invalidConfigError{msg: fmt.Sprintf(msg, a...)}
}

func (invalidConfigError) Type() string {
	return "invalid_config"
}

func (err invalidConfigError) Error() string {
	if err.msg == "" {
		return "Invalid value for field 'config'"
	}
	return err.msg
}

// InvalidBallotError is returned when the ballot has an invalid format.
type InvalidBallotError struct {
	msg string
}

func (InvalidBallotError) Type() string {
	return "invalid_ballot"
}

func (err InvalidBallotError) Error() string {
	return err.msg
}

func invalidVote(msg string, a ...any) InvalidBallotError {
	return InvalidBallotError{msg: fmt.Sprintf(msg, a...)}
}
