package method_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/OpenSlides/openslides-go/datastore/dsmodels"
	"github.com/OpenSlides/openslides-vote-service/vote/method"
	"github.com/shopspring/decimal"
)

func TestRatingScoreValidateVote(t *testing.T) {
	for _, tt := range []struct {
		name        string
		method      string
		config      string
		options     []int
		vote        string
		expectValid bool
	}{
		{
			name:        "Rating Score",
			method:      "rating_score",
			config:      `{}`,
			options:     []int{1, 2},
			vote:        `{"1":3}`,
			expectValid: true,
		},
		{
			name:        "Rating Score invalid key",
			method:      "rating_score",
			config:      `{}`,
			options:     []int{1, 2},
			vote:        `{"0":3}`,
			expectValid: false,
		},
		{
			name:        "Rating Score with negative value",
			method:      "rating_score",
			config:      `{}`,
			options:     []int{1, 2},
			vote:        `{"1":-3}`,
			expectValid: false,
		},
		{
			name:        "Rating Score max_options_amount",
			method:      "rating_score",
			config:      `{"max_options_amount":1}`,
			options:     []int{1, 2},
			vote:        `{"1":3}`,
			expectValid: true,
		},
		{
			name:        "Rating Score max_options_amount too many",
			method:      "rating_score",
			config:      `{"max_options_amount":1}`,
			options:     []int{1, 2},
			vote:        `{"1":3, "2":1}`,
			expectValid: false,
		},
		{
			name:        "Rating Score min_options_amount",
			method:      "rating_score",
			config:      `{"min_options_amount":1}`,
			options:     []int{1, 2},
			vote:        `{"1":3}`,
			expectValid: true,
		},
		{
			name:        "Rating Score min_options_amount too few",
			method:      "rating_score",
			config:      `{"min_options_amount":2}`,
			options:     []int{1, 2},
			vote:        `{"1":3}`,
			expectValid: false,
		},
		{
			name:        "Rating Score max_votes_per_option",
			method:      "rating_score",
			config:      `{"max_votes_per_option":2}`,
			options:     []int{1, 2},
			vote:        `{"1":2}`,
			expectValid: true,
		},
		{
			name:        "Rating Score max_votes_per_option too many",
			method:      "rating_score",
			config:      `{"max_votes_per_option":2}`,
			options:     []int{1, 2},
			vote:        `{"1":3}`,
			expectValid: false,
		},
		{
			name:        "Rating Score max_vote_sum",
			method:      "rating_score",
			config:      `{"max_vote_sum":5}`,
			options:     []int{1, 2},
			vote:        `{"1":3}`,
			expectValid: true,
		},
		{
			name:        "Rating Score max_vote_sum too many",
			method:      "rating_score",
			config:      `{"max_vote_sum":5}`,
			options:     []int{1, 2},
			vote:        `{"1":6}`,
			expectValid: false,
		},
		{
			name:        "Rating Score max_vote_sum too many on different options",
			method:      "rating_score",
			config:      `{"max_vote_sum":5}`,
			options:     []int{1, 2},
			vote:        `{"1":3, "2":3}`,
			expectValid: false,
		},
		{
			name:        "Rating Score min_vote_sum on one vote",
			method:      "rating_score",
			config:      `{"min_vote_sum":10}`,
			options:     []int{1, 2},
			vote:        `{"1":5}`,
			expectValid: false,
		},
		{
			name:        "Rating Score min_vote_sum on many votes",
			method:      "rating_score",
			config:      `{"min_vote_sum":10}`,
			options:     []int{1, 2},
			vote:        `{"1":5, "2":4}`,
			expectValid: false,
		},
		{
			name:        "Rating Score min_vote_sum enough",
			method:      "rating_score",
			config:      `{"min_vote_sum":1}`,
			options:     []int{1, 2},
			vote:        `{"1":5, "2":5}`,
			expectValid: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, err := method.RatingScoreFromRequest([]byte(tt.config))
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

func TestRatingScoreCreateResult(t *testing.T) {
	for _, tt := range []struct {
		name         string
		method       string
		config       string
		options      []int
		ballots      []dsmodels.PollBallot
		expectResult string
	}{
		{
			name:    "Rating Score",
			method:  "rating_score",
			config:  `{}`,
			options: []int{1, 2, 3},
			ballots: []dsmodels.PollBallot{
				{Value: `{"1":3,"2":3}`},
				{Value: `{"2":2,"3":3}`},
				{Value: `{"3":5}`, Weight: decimal.NewFromInt(5)},
			},
			expectResult: `{"1":"3","2":"5","3":"28","total_ballots":3}`,
		},
		{
			name:    "Rating Score Abstain",
			method:  "rating_score",
			config:  `{}`,
			options: []int{1, 2, 3},
			ballots: []dsmodels.PollBallot{
				{Value: `{"1":3,"2":3}`},
				{Value: `{}`},
				{Value: `{}`, Weight: decimal.NewFromInt(5)},
			},
			expectResult: `{"1":"3","2":"3","abstain":"6","total_ballots":3}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, err := method.RatingScoreFromRequest([]byte(tt.config))
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
