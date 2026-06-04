package method_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/OpenSlides/openslides-go/datastore/dsmodels"
	"github.com/OpenSlides/openslides-vote-service/vote/method"
)

func TestApprovalValidateVote(t *testing.T) {
	for _, tt := range []struct {
		name        string
		method      string
		config      string
		vote        string
		expectValid bool
	}{
		{
			name:        "Approval: Vote Yes",
			method:      "approval",
			config:      "",
			vote:        `"Yes"`,
			expectValid: true,
		},
		{
			name:        "Approval: unknown string",
			method:      "approval",
			config:      "",
			vote:        `"Y"`,
			expectValid: false,
		},
		{
			name:        "Approval: Abstain",
			method:      "approval",
			config:      "",
			vote:        `"Abstain"`,
			expectValid: true,
		},
		{
			name:        "Approval: Abstain deactivated",
			method:      "approval",
			config:      `{"allow_abstain": false}`,
			vote:        `"Abstain"`,
			expectValid: false,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, err := method.ApprovalFromRequest([]byte(tt.config))
			if err != nil {
				t.Fatalf("Error: %v", err)
			}

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

func TestApprovalCreateResult(t *testing.T) {
	for _, tt := range []struct {
		name         string
		method       string
		config       string
		ballots      []dsmodels.PollBallot
		expectResult string
	}{
		{
			name:   "Approval",
			method: "approval",
			config: "",
			ballots: []dsmodels.PollBallot{
				{Value: `"Yes"`},
				{Value: `"Yes"`},
				{Value: `"No"`},
			},
			expectResult: `{"no":"1","total_ballots":3,"yes":"2"}`,
		},
		{
			name:   "Approval with invalid",
			method: "approval",
			config: "",
			ballots: []dsmodels.PollBallot{
				{Value: `"Yes"`},
				{Value: `"Yes"`},
				{Value: `"No"`},
				{Value: `"ABC"`},
			},
			expectResult: `{"invalid":1,"no":"1","total_ballots":4,"yes":"2"}`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, err := method.ApprovalFromRequest([]byte(tt.config))
			if err != nil {
				t.Fatalf("Error: %v", err)
			}

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
