package method_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/OpenSlides/openslides-go/datastore/dsmodels"
	"github.com/OpenSlides/openslides-vote-service/vote/method"
	"github.com/shopspring/decimal"
)

func TestRatingApprovalValidateVote(t *testing.T) {
	for _, tt := range []struct {
		name        string
		config      string
		options     []int
		vote        string
		expectValid bool
	}{
		{
			name:        "Normal",
			config:      `{}`,
			options:     []int{1, 2},
			vote:        `{"1":"Yes", "2":"No"}`,
			expectValid: true,
		},
		{
			name:        "Invalid key",
			config:      `{}`,
			options:     []int{1, 2},
			vote:        `{"0":"Yes", "2":"No"}`,
			expectValid: false,
		},
		{
			name:        "Disallow abstain",
			config:      `{"allow_abstain":false}`,
			options:     []int{1, 2},
			vote:        `{"1":"Yes", "2":"Abstain"}`,
			expectValid: false,
		},
		{
			name:        "Invalid value",
			config:      `{"allow_abstain":false}`,
			options:     []int{1, 2},
			vote:        `{"1":"Yes", "2":"invalid"}`,
			expectValid: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, err := method.RatingApprovalFromRequest([]byte(tt.config))
			if err != nil {
				t.Fatalf("Error: %v", err)
			}

			a.Options = tt.options

			err = a.ValidateBallot(json.RawMessage(tt.vote))

			if err != nil {
				if _, ok := errors.AsType[method.InvalidBallotError](err); !ok {
					t.Errorf("Got unexpected error: %v", err)
				}
			}

			if tt.expectValid {
				if err != nil {
					t.Fatalf("Validate returned unexpected error: %v", err)
				}
				return
			}

			if err == nil {
				t.Fatalf("Got no validation error")
			}
		})
	}
}

func TestRatingApprovalCreateResult(t *testing.T) {
	for _, tt := range []struct {
		name         string
		config       string
		options      []int
		ballots      []dsmodels.PollBallot
		expectResult string
	}{
		{
			name:    "Normal",
			config:  `{}`,
			options: []int{1, 2, 3},
			ballots: []dsmodels.PollBallot{
				{Value: `{"1":"yes","2":"no"}`},
				{Value: `{"2":"yes","3":"no"}`},
				{Value: `{"3":"yes"}`, Weight: decimal.NewFromInt(5)},
			},
			expectResult: `{"1":{"yes":"1"},"2":{"no":"1","yes":"1"},"3":{"no":"1","yes":"5"}}`,
		},
		{
			name:    "With out abstain but with invalid",
			config:  `{"allow_abstain":false}`,
			options: []int{1, 2, 3},
			ballots: []dsmodels.PollBallot{
				{Value: `{"1":"yes","2":"abstain"}`},
				{Value: `{"1":"yes","2":"no"}`},
			},
			expectResult: `{"1":{"yes":"1"},"2":{"no":"1"},"invalid":1}`,
		},
		{
			name:    "General abstain",
			config:  `{}`,
			options: []int{1, 2, 3},
			ballots: []dsmodels.PollBallot{
				{Value: `{"1":"yes","2":"no"}`},
				{Value: `{}`},
			},
			expectResult: `{"1":{"yes":"1"},"2":{"no":"1"},"abstain":"1"}`,
		},
		{
			name: "General abstain but abstain not allowed",
			// At the moment, to abstain and not vote for a option, is something different.
			config:  `{"allow_abstain":false}`,
			options: []int{1, 2, 3},
			ballots: []dsmodels.PollBallot{
				{Value: `{"1":"yes","2":"no"}`},
				{Value: `{"1":"yes","2":"abstain"}`},
				{Value: `{}`},
			},
			expectResult: `{"1":{"yes":"1"},"2":{"no":"1"},"abstain":"1","invalid":1}`,
		},
		{
			name: "Not Voting does not count as abstain",
			// At the moment, to abstain and not vote for a option, is something different.
			config:  `{}`,
			options: []int{1, 2, 3},
			ballots: []dsmodels.PollBallot{
				{Value: `{"1":"yes","2":"abstain"}`},
				{Value: `{"1":"yes"}`},
			},
			expectResult: `{"1":{"yes":"2"},"2":{"abstain":"1"}}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, err := method.RatingApprovalFromRequest([]byte(tt.config))
			if err != nil {
				t.Fatalf("Error: %v", err)
			}
			a.Options = tt.options

			result, err := a.Result(tt.ballots)
			if err != nil {
				t.Fatalf("CreateResult: %v", err)
			}

			if string(result) != tt.expectResult {
				t.Errorf("Got: %s, expected %s", result, tt.expectResult)
			}
		})
	}
}
