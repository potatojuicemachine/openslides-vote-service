package method

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/OpenSlides/openslides-go/datastore/dsfetch"
	"github.com/OpenSlides/openslides-go/datastore/dsmodels"
	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"
)

type RatingApproval struct {
	Options          []int              `json:"options"`
	MaxOptionsAmount dsfetch.Maybe[int] `json:"max_options_amount"`
	MinOptionsAmount dsfetch.Maybe[int] `json:"min_options_amount"`
	AllowAbstain     bool               `json:"allow_abstain"`
}

func (RatingApproval) RequireOptions() bool {
	return true
}

func RatingApprovalFromDB(configDB dsmodels.PollConfigRatingApproval, optionIDs []int) *RatingApproval {
	return &RatingApproval{
		Options:          optionIDs,
		MaxOptionsAmount: maybeZeroIsNull(configDB.MaxOptionsAmount),
		MinOptionsAmount: maybeZeroIsNull(configDB.MinOptionsAmount),
		AllowAbstain:     configDB.AllowAbstain,
	}
}

func RatingApprovalFromRequest(config json.RawMessage) (*RatingApproval, error) {
	var cfg struct {
		MaxOptionsAmount dsfetch.Maybe[int]  `json:"max_options_amount"`
		MinOptionsAmount dsfetch.Maybe[int]  `json:"min_options_amount"`
		AllowAbstain     dsfetch.Maybe[bool] `json:"allow_abstain"`
	}
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, invalidConfig("method_config has to be valid json")
	}

	valueMaxOption, setMaxOption := cfg.MaxOptionsAmount.Value()
	valueMinOption, setMinOption := cfg.MinOptionsAmount.Value()
	if setMaxOption && setMinOption && valueMinOption > valueMaxOption {
		return nil, invalidConfig("value of min_options_amount has to be lower then max_options_amount")
	}

	allowAbstain, set := cfg.AllowAbstain.Value()
	if !set {
		allowAbstain = true
	}

	return &RatingApproval{
		MaxOptionsAmount: cfg.MaxOptionsAmount,
		MinOptionsAmount: cfg.MinOptionsAmount,
		AllowAbstain:     allowAbstain,
	}, nil
}

func (ra RatingApproval) Name() string {
	return "rating_approval"
}

func ratingApprovalSaveConfig(ctx context.Context, tx pgx.Tx, config json.RawMessage) (string, error) {
	ra, err := RatingApprovalFromRequest(config)
	if err != nil {
		return "", fmt.Errorf("load config: %w", err)
	}

	var cfg struct {
		OneHundredPercentBase string `json:"onehundred_percent_base"`
	}
	if err := json.Unmarshal(config, &cfg); err != nil {
		return "", fmt.Errorf("load additional config: %w", err)
	}

	if cfg.OneHundredPercentBase == "" {
		return "", invalidConfig("field onehundred_percent_base is required.")
	}

	var configID int
	sql := `INSERT INTO poll_config_rating_approval
	(max_options_amount, min_options_amount, allow_abstain, onehundred_percent_base)
	VALUES ($1, $2, $3, $4)
	RETURNING id;`
	if err := tx.QueryRow(
		ctx,
		sql,
		maybeNullIsNil(ra.MaxOptionsAmount),
		maybeNullIsNil(ra.MinOptionsAmount),
		ra.AllowAbstain,
		cfg.OneHundredPercentBase,
	).Scan(&configID); err != nil {
		return "", fmt.Errorf("save rating_approval config: %w", err)
	}

	return fmt.Sprintf("poll_config_rating_approval/%d", configID), nil
}

func (ra RatingApproval) ValidateBallot(vote json.RawMessage) error {
	var choice map[int]json.RawMessage
	if err := json.Unmarshal(vote, &choice); err != nil {
		return errors.Join(invalidVote("Vote has invalid format"), fmt.Errorf("decoding vote: %w", err))
	}

	if value, set := ra.MaxOptionsAmount.Value(); set && len(choice) > value {
		return invalidVote("too many options")
	}

	if value, set := ra.MinOptionsAmount.Value(); set && len(choice) < value {
		return invalidVote("too few options")
	}

	for option, choice := range choice {
		if !slices.Contains(ra.Options, option) {
			return invalidVote("unknown option id %d", option)
		}

		approval := &Approval{AllowAbstain: ra.AllowAbstain}
		if err := approval.ValidateBallot(choice); err != nil {
			return fmt.Errorf("validating option id %d: %w", option, err)
		}
	}

	return nil
}

func (ra RatingApproval) Result(votes []dsmodels.PollBallot) (string, error) {
	result := make(map[string]map[string]decimal.Decimal)
	invalid := 0

	for _, vote := range votes {
		if err := ra.ValidateBallot(json.RawMessage(vote.Value)); err != nil {
			if _, ok := errors.AsType[InvalidBallotError](err); ok {
				invalid++
				continue
			}
			return "", fmt.Errorf("validating vote: %w", err)
		}

		weight := vote.Weight
		if vote.Weight.IsZero() {
			weight = decimal.NewFromInt(1)
		}

		var votedOptions map[string]json.RawMessage
		if err := json.Unmarshal([]byte(vote.Value), &votedOptions); err != nil {
			return "", fmt.Errorf("invalid options `%s`: %w", vote.Value, err)
		}

		for option, value := range votedOptions {
			if _, ok := result[option]; !ok {
				result[option] = make(map[string]decimal.Decimal)
			}

			switch strings.ToLower(string(value)) {
			case `"yes"`:
				result[option]["yes"] = result[option]["yes"].Add(weight)
			case `"no"`:
				result[option]["no"] = result[option]["no"].Add(weight)
			case `"abstain"`:
				result[option]["abstain"] = result[option]["abstain"].Add(weight)
			}
		}
	}

	encodedResult, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("encode result: %w", err)
	}
	withInvalid, err := addInvalid(encodedResult, invalid)
	if err != nil {
		return "", fmt.Errorf("add invalid: %w", err)
	}
	return string(withInvalid), nil
}
