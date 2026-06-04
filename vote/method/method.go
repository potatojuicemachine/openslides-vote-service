package method

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/OpenSlides/openslides-go/datastore/dsfetch"
	"github.com/OpenSlides/openslides-go/datastore/dsmodels"
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
