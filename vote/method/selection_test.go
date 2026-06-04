package method_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/OpenSlides/openslides-go/datastore/dsmodels"
	"github.com/OpenSlides/openslides-vote-service/vote/method"
	"github.com/shopspring/decimal"
)

func TestSelectionValidateVote(t *testing.T) {
	for _, tt := range []struct {
		name        string
		method      string
		config      string
		options     []int
		vote        string
		expectValid bool
	}{
		{
			name:        "Selection invalid json",
			method:      "selection",
			config:      `{}`,
			options:     []int{1, 2},
			vote:        `[0`,
			expectValid: false,
		},
		{
			name:        "Selection",
			method:      "selection",
			config:      `{}`,
			options:     []int{1, 2},
			vote:        `[1]`,
			expectValid: true,
		},
		{
			name:        "Selection same value multiple times",
			method:      "selection",
			config:      `{}`,
			options:     []int{1, 2},
			vote:        `[1,1]`,
			expectValid: false,
		},
		{
			name:        "Selection unknown key",
			method:      "selection",
			config:      `{}`,
			options:     []int{1, 2},
			vote:        `[3]`,
			expectValid: false,
		},
		{
			name:        "Selection max_options_amount",
			method:      "selection",
			config:      `{"max_options_amount":1}`,
			options:     []int{1, 2},
			vote:        `[1]`,
			expectValid: true,
		},
		{
			name:        "Selection max_options_amount too many",
			method:      "selection",
			config:      `{"max_options_amount":1}`,
			options:     []int{1, 2},
			vote:        `[1,2]`,
			expectValid: false,
		},
		{
			name:        "Selection min_options_amount",
			method:      "selection",
			config:      `{"min_options_amount":1}`,
			options:     []int{1, 2},
			vote:        `[1]`,
			expectValid: true,
		},
		{
			name:        "Selection min_options_amount too few",
			method:      "selection",
			config:      `{"min_options_amount":2}`,
			options:     []int{1, 2},
			vote:        `[1]`,
			expectValid: false,
		},
		{
			name:        "Selection nota",
			method:      "selection",
			config:      `{"min_options_amount":2,"allow_nota":true}`,
			options:     []int{1, 2},
			vote:        `"nota"`,
			expectValid: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, err := method.SelectionFromRequest([]byte(tt.config))
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

func TestSelectionCreateResult(t *testing.T) {
	for _, tt := range []struct {
		name         string
		method       string
		config       string
		options      []int
		ballots      []dsmodels.PollBallot
		expectResult string
	}{
		{
			name:    "Selection",
			method:  "selection",
			config:  `{}`,
			options: []int{1, 2, 3},
			ballots: []dsmodels.PollBallot{
				{Value: `[1,2]`},
				{Value: `[2,3]`},
				{Value: `[3]`, Weight: decimal.NewFromInt(5)},
			},
			expectResult: `{"1":"1","2":"2","3":"6","total_ballots":3}`,
		},
		{
			name:    "Selection abstain",
			method:  "selection",
			config:  `{}`,
			options: []int{1, 2, 3},
			ballots: []dsmodels.PollBallot{
				{Value: `[1,2]`},
				{Value: `[]`},
				{Value: `[]`, Weight: decimal.NewFromInt(5)},
			},
			expectResult: `{"1":"1","2":"1","abstain":"6","total_ballots":3}`,
		},
		{
			name:    "Selection nota",
			method:  "selection",
			config:  `{"allow_nota":true}`,
			options: []int{1, 2, 3},
			ballots: []dsmodels.PollBallot{
				{Value: `[1,2]`},
				{Value: `"nota"`},
				{Value: `"nota"`, Weight: decimal.NewFromInt(5)},
			},
			expectResult: `{"1":"1","2":"1","nota":"6","total_ballots":3}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, err := method.SelectionFromRequest([]byte(tt.config))
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
