package vote_test

import (
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/OpenSlides/openslides-go/datastore/dsfetch"
	"github.com/OpenSlides/openslides-go/datastore/dskey"
	"github.com/OpenSlides/openslides-go/datastore/dsmock"
	"github.com/OpenSlides/openslides-go/datastore/dsmodels"
	"github.com/OpenSlides/openslides-go/datastore/flow"
	"github.com/OpenSlides/openslides-go/datastore/pgtest"
	"github.com/OpenSlides/openslides-go/environment"
	"github.com/OpenSlides/openslides-vote-service/vote"
	"github.com/OpenSlides/openslides-vote-service/vote/method"
)

func TestMain(m *testing.M) {
	os.Exit(pgtest.RunTests(m))
}

func TestAll(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("Postgres Test")
	}

	ctx := t.Context()

	pg, err := pgtest.NewPostgresTest(t)
	if err != nil {
		t.Fatalf("Error starting postgres: %v", err)
	}

	data := `---
	organization/1/enable_electronic_voting: true
	motion/5:
		meeting_id: 1
		sequential_number: 1
		title: my motion
		state_id: 1

	list_of_speakers/7:
		content_object_id: motion/5
		sequential_number: 1
		meeting_id: 1

	meeting/1:
		present_user_ids: [30]

	user:
		5:
			username: admin
			organization_management_level: superadmin
		30:
			username: tom
	meeting_user/300:
		group_ids: [40]
		user_id: 30
		meeting_id: 1

	group/40:
		name: delegate
		meeting_id: 1

	group/41:
		name: wrong group
		meeting_id: 1
	`

	withData(
		t,
		pg,
		data,
		func(service *vote.Vote, flow flow.Flow) {
			t.Run("Create", func(t *testing.T) {
				body := `{
					"title": "my pol",
					"content_object_id": "motion/5",
					"method": "approval",
					"method_config": {
						"onehundred_percent_base": "valid"
					},
					"visibility": "open",
					"meeting_id": 1,
					"entitled_group_ids": [41]
				}`

				id, err := service.Create(ctx, 5, strings.NewReader(body))
				if err != nil {
					t.Fatalf("Error creating poll: %v", err)
				}

				if id != 1 {
					t.Errorf("Expected id 1, got %d", id)
				}

				key := dskey.MustKey("poll/1/title")
				result, err := flow.Get(ctx, key)
				if err != nil {
					t.Fatalf("Error getting title from created poll: %v", err)
				}

				if string(result[key]) != `"my pol"` {
					t.Errorf("Expected title 'my poll', got %s", result[key])
				}
			})

			t.Run("Update", func(t *testing.T) {
				body := `{
					"title": "my poll",
					"entitled_group_ids": [40]
				}`

				err := service.Update(ctx, 1, 5, strings.NewReader(body))
				if err != nil {
					t.Fatalf("Error creating poll: %v", err)
				}

				poll, err := dsmodels.New(flow).Poll(1).First(ctx)
				if err != nil {
					t.Fatalf("fetch poll: %v", err)
				}

				if poll.Title != `my poll` {
					t.Errorf("Expected title 'my poll', got %s", poll.Title)
				}

				if len(poll.EntitledGroupIDs) != 1 && poll.EntitledGroupIDs[0] != 40 {
					t.Errorf("Expected entitled_group_ids [40], got %v", poll.EntitledGroupIDs)
				}
			})

			t.Run("Start", func(t *testing.T) {
				if err := service.Start(ctx, 1, 5); err != nil {
					t.Fatalf("Error starting poll: %v", err)
				}

				key := dskey.MustKey("poll/1/state")
				values, err := flow.Get(ctx, key)
				if err != nil {
					t.Fatalf("Error getting state from poll: %v", err)
				}

				if string(values[key]) != `"started"` {
					t.Errorf("Expected state to be started, got %s", values[key])
				}
			})

			t.Run("Vote", func(t *testing.T) {
				body := `{"value":"Yes"}`
				if err := service.Vote(ctx, 1, 30, strings.NewReader(body)); err != nil {
					t.Fatalf("Error voting poll: %v", err)
				}

				ds := dsmodels.New(flow)
				vote, err := ds.PollBallot(1).First(t.Context())
				if err != nil {
					t.Fatalf("Error: Getting vote: %v", err)
				}

				if id, _ := vote.ActingMeetingUserID.Value(); id != 300 {
					t.Errorf("Expected acting meeting_user ID to be 300, got %d", id)
				}

				if vote.Value != `"Yes"` {
					t.Errorf("Expected vote value to be '\"Yes\"', got '%s'", vote.Value)
				}
			})

			t.Run("Stop", func(t *testing.T) {
				if err := service.Finalize(ctx, 1, 5, false, false); err != nil {
					t.Fatalf("Error stopping poll: %v", err)
				}

				keyState := dskey.MustKey("poll/1/state")
				keyResult := dskey.MustKey("poll/1/result")
				values, err := flow.Get(ctx, keyState, keyResult)
				if err != nil {
					t.Fatalf("Error getting state from poll: %v", err)
				}

				if string(values[keyState]) != `"finished"` {
					t.Errorf("Expected state to be finished, got %s", values[keyState])
				}

				if string(values[keyResult]) == `` {
					t.Errorf("Expected result to be set")
				}
			})

			t.Run("Publish", func(t *testing.T) {
				if err := service.Finalize(ctx, 1, 5, true, false); err != nil {
					t.Fatalf("Error publishing poll: %v", err)
				}

				key := dskey.MustKey("poll/1/published")
				values, err := flow.Get(ctx, key)
				if err != nil {
					t.Fatalf("Error getting state from poll: %v", err)
				}

				if string(values[key]) != `true` {
					t.Errorf("Expected published to be true, got %s", values[key])
				}
			})

			t.Run("Anonymize", func(t *testing.T) {
				if err := service.Finalize(ctx, 1, 5, true, true); err != nil {
					t.Fatalf("Error anonymizing poll: %v", err)
				}

				ds := dsmodels.New(flow)
				vote, err := ds.PollBallot(1).First(t.Context())
				if err != nil {
					t.Fatalf("Error: Getting vote: %v", err)
				}

				if id, set := vote.ActingMeetingUserID.Value(); set {
					t.Errorf("Expected acting meeting_user ID not to be set, but is is %d", id)
				}

				anonymized, err := ds.Poll_Anonymized(1).Value(t.Context())
				if err != nil {
					t.Fatalf("Error: %v", err)
				}
				if !anonymized {
					t.Errorf("Expected poll to be anonymized")
				}
			})

			t.Run("Reset", func(t *testing.T) {
				if err := service.Reset(ctx, 1, 5); err != nil {
					t.Fatalf("Error resetting poll: %v", err)
				}

				key := dskey.MustKey("poll/1/state")
				values, err := flow.Get(ctx, key)
				if err != nil {
					t.Fatalf("Error getting state from poll: %v", err)
				}

				if string(values[key]) != `"created"` {
					t.Errorf("Expected state to be created, got %s", values[key])
				}

				ds := dsmodels.New(flow)
				anonymized, err := ds.Poll_Anonymized(1).Value(t.Context())
				if err != nil {
					t.Fatalf("Error: %v", err)
				}
				if anonymized {
					t.Errorf("Expected poll not to be anonymized")
				}
			})

			t.Run("Delete", func(t *testing.T) {
				if err := service.Delete(ctx, 1, 5); err != nil {
					t.Fatalf("Error deleting poll: %v", err)
				}

				key := dskey.MustKey("poll/1/title")
				_, err := flow.Get(ctx, key)
				if err != nil {
					t.Fatalf("Error getting title from created poll: %v", err)
				}
			})
		},
	)
}

func TestCreateSelection(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("Postgres Test")
	}

	ctx := t.Context()

	pg, err := pgtest.NewPostgresTest(t)
	if err != nil {
		t.Fatalf("Error starting postgres: %v", err)
	}

	data := `---
	organization/1/enable_electronic_voting: true
	user/5:
		username: admin
		organization_management_level: superadmin

	assignment/5:
		meeting_id: 1
		title: my assignment

	list_of_speakers/7:
		content_object_id: assignment/5
		sequential_number: 1
		meeting_id: 1

	meeting/1/welcome_title: hello world
	`

	withData(t, pg, data, func(service *vote.Vote, flow flow.Flow) {
		body := `{
			"title": "my poll",
			"content_object_id": "assignment/5",
			"method": "selection",
			"method_config": {
				"onehundred_percent_base": "valid",
				"min_options_amount": 1,
				"max_options_amount": 2
			},
			"visibility": "open",
			"meeting_id": 1,
			"options": ["Hubert", "Max"],
			"option_type": "text"
		}`

		id, err := service.Create(ctx, 5, strings.NewReader(body))
		if err != nil {
			t.Fatalf("Error creating poll: %v", err)
		}

		poll, err := dsmodels.New(flow).Poll(id).First(ctx)
		if err != nil {
			t.Fatalf("Fetch poll: %v", err)
		}

		_, configIDRaw, ok := strings.Cut(poll.ConfigID, "/")
		if !ok {
			t.Fatalf("invalid configID: %s", poll.ConfigID)
		}

		configID, err := strconv.Atoi(configIDRaw)
		if err != nil {
			t.Fatalf("invalid configID: %s", poll.ConfigID)
		}

		config, err := dsmodels.New(flow).PollConfigSelection(configID).First(ctx)
		if err != nil {
			t.Fatalf("Fetch poll config: %v", err)
		}

		if config.MaxOptionsAmount != 2 {
			t.Errorf("got max_options_amount %d, expected 2", config.MaxOptionsAmount)
		}

		if config.MinOptionsAmount != 1 {
			t.Errorf("got min_options_amount %d, expected 1", config.MinOptionsAmount)
		}

	})
}

func TestUpdate(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("Postgres Test")
	}

	pg, err := pgtest.NewPostgresTest(t)
	if err != nil {
		t.Fatalf("Error starting postgres: %v", err)
	}

	data := `---
	organization/1/enable_electronic_voting: true
	user/5:
		username: admin
		organization_management_level: superadmin
	group/40:
		name: delegate
		meeting_id: 1
	motion/5:
		meeting_id: 1
		sequential_number: 1
		title: my motion
		state_id: 1
	list_of_speakers/7:
		content_object_id: motion/5
		sequential_number: 1
		meeting_id: 1
	meeting/1/welcome_title: hello world

	poll_config_approval/77:
		allow_abstain: true
		onehundred_percent_base: valid
	poll/3:
		title: my poll
		config_id: poll_config_approval/77
		visibility: open
		sequential_number: 1
		content_object_id: motion/5
		meeting_id: 1
		state: created
		entitled_group_ids: [40]
	`

	withData(t, pg, data, func(service *vote.Vote, flow flow.Flow) {
		t.Run("Update Title", func(t *testing.T) {
			body := `{
				"title": "updated title"
			}`

			if err := service.Update(t.Context(), 3, 5, strings.NewReader(body)); err != nil {
				t.Fatalf("Error updating poll: %v", err)
			}

			poll, err := dsmodels.New(flow).Poll(3).First(t.Context())
			if err != nil {
				t.Fatalf("Error getting poll: %v", err)
			}
			if poll.Title != "updated title" {
				t.Fatalf("Expected updated title, got %s", poll.Title)
			}
		})

		t.Run("Update method", func(t *testing.T) {
			body := `{
				"method": "selection",
				"method_config": {
					"onehundred_percent_base": "valid"
				}
			}`

			if err := service.Update(t.Context(), 3, 5, strings.NewReader(body)); err != nil {
				t.Fatalf("Error updating poll: %v", err)
			}

			poll, err := dsmodels.New(flow).Poll(3).First(t.Context())
			if err != nil {
				t.Fatalf("Error getting poll: %v", err)
			}
			if !strings.HasPrefix(poll.ConfigID, "poll_config_selection") {
				t.Fatalf("Expected method selection, got %s", poll.ConfigID)
			}
		})

		t.Run("Update options", func(t *testing.T) {
			body := `{
				"option_type": "text",
				"options": ["option1", "option2"]
			}`

			if err := service.Update(t.Context(), 3, 5, strings.NewReader(body)); err != nil {
				t.Fatalf("Error updating poll: %v", err)
			}

			ds := dsmodels.New(flow)

			q := ds.Poll(3)
			q = q.Preload(q.OptionList())
			poll, err := q.First(t.Context())
			if err != nil {
				t.Fatalf("Error getting poll: %v", err)
			}
			if len(poll.OptionList) != 2 {
				t.Fatalf("Expected two options, got %d", len(poll.OptionList))
			}
		})
	})
}

func TestManually(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("Postgres Test")
	}

	ctx := t.Context()

	pg, err := pgtest.NewPostgresTest(t)
	if err != nil {
		t.Fatalf("Error starting postgres: %v", err)
	}

	data := `---
	user/5:
		username: admin
		organization_management_level: superadmin

	motion/5:
		meeting_id: 1
		sequential_number: 1
		title: my motion
		state_id: 1

	list_of_speakers/7:
		content_object_id: motion/5
		sequential_number: 1
		meeting_id: 1

	meeting/1/welcome_title: hello world
	`

	withData(t, pg, data, func(service *vote.Vote, flow flow.Flow) {
		t.Run("Create", func(t *testing.T) {
			body := `{
				"title": "my poll",
				"content_object_id": "motion/5",
				"method": "approval",
				"method_config": {
					"onehundred_percent_base": "valid"
				},
				"visibility": "manually",
				"meeting_id": 1,
				"result": {"no":"23","yes":"42"}
			}`

			id, err := service.Create(ctx, 5, strings.NewReader(body))
			if err != nil {
				t.Fatalf("Error creating poll: %v", err)
			}

			if id != 1 {
				t.Errorf("Expected id 1, got %d", id)
			}

			poll, err := dsmodels.New(flow).Poll(1).First(ctx)
			if err != nil {
				t.Fatalf("Fetch poll: %v", err)
			}

			if poll.State != "finished" {
				t.Errorf("Poll is in state %s, expected state finished", poll.State)
			}

			if poll.Result != `{"no":"23","yes":"42"}` {
				t.Errorf("Result does not match")
			}
		})

		t.Run("Reset", func(t *testing.T) {
			err := service.Reset(ctx, 1, 5)
			if err != nil {
				t.Fatalf("Error creating poll: %v", err)
			}

			poll, err := dsmodels.New(flow).Poll(1).First(ctx)
			if err != nil {
				t.Fatalf("Fetch poll: %v", err)
			}

			if poll.State != "finished" {
				t.Errorf("State == %s. A manually poll has to be in state finished after a reset", poll.State)
			}
		})
	})
}

func TestVote(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("Postgres Test")
	}

	ctx := t.Context()

	pg, err := pgtest.NewPostgresTest(t)
	if err != nil {
		t.Fatalf("Error starting postgres: %v", err)
	}

	data := `---
	motion/5:
		meeting_id: 1
		sequential_number: 1
		title: my motion
		state_id: 1

	list_of_speakers/7:
		content_object_id: motion/5
		sequential_number: 1
		meeting_id: 1

	meeting/1:
		present_user_ids: [30]

	user/30:
		username: tom
	meeting_user/300:
		group_ids: [40]
		user_id: 30
		meeting_id: 1

	group/40:
		name: delegate
		meeting_id: 1

	poll_config_approval/77:
		allow_abstain: true
		onehundred_percent_base: valid

	poll/5:
		title: my poll
		config_id: poll_config_approval/77
		visibility: open
		sequential_number: 1
		content_object_id: motion/5
		meeting_id: 1
		state: started
		entitled_group_ids: [40]
	`

	withData(
		t,
		pg,
		data,
		func(service *vote.Vote, flow flow.Flow) {
			t.Run("Simple Vote", func(t *testing.T) {
				body := `{"value":"Yes"}`
				if err := service.Vote(ctx, 5, 30, strings.NewReader(body)); err != nil {
					t.Fatalf("Error processing poll: %v", err)
				}

				ds := dsmodels.New(flow)
				vote, err := ds.PollBallot(1).First(t.Context())
				if err != nil {
					t.Fatalf("Error: Getting vote: %v", err)
				}

				if id, _ := vote.ActingMeetingUserID.Value(); id != 300 {
					t.Errorf("Expected acting_meeting_user ID to be 300, got %d", id)
				}

				if vote.Value != `"Yes"` {
					t.Errorf("Expected vote value to be 'Yes', got '%s'", vote.Value)
				}
			})
		},
	)
}

func TestVoteWeight(t *testing.T) {
	for _, tt := range []struct {
		name string
		data string

		expectWeight string
	}{
		{
			"No weight",
			`
			poll/1:
				meeting_id: 1
				entitled_group_ids: [1]
				config_id: poll_config_approval/77
				visibility: open
				content_object_id: some_field/1
				sequential_number: 1
				title: myPoll

			poll_config_approval/77:
				allow_abstain: true
				onehundred_percent_base: valid

			meeting/1/id: 1

			user/1:
				is_present_in_meeting_ids: [1]
				meeting_user_ids: [10]
			meeting_user/10:
				user_id: 1
				group_ids: [1]
				meeting_id: 1
			`,
			"1",
		},
		{
			"Weight enabled, user has no weight",
			`
			poll/1:
				meeting_id: 1
				entitled_group_ids: [1]
				config_id: poll_config_approval/77
				visibility: open
				content_object_id: some_field/1
				sequential_number: 1
				title: myPoll

			poll_config_approval/77:
				allow_abstain: true
				onehundred_percent_base: valid

			meeting/1/users_enable_vote_weight: true

			user/1:
				is_present_in_meeting_ids: [1]
				meeting_user_ids: [10]
			meeting_user/10:
				user_id: 1
				group_ids: [1]
				meeting_id: 1
			`,
			"1",
		},
		{
			"Weight enabled, user has default weight",
			`
			poll/1:
				meeting_id: 1
				entitled_group_ids: [1]
				config_id: poll_config_approval/77
				visibility: open
				content_object_id: some_field/1
				sequential_number: 1
				title: myPoll

			poll_config_approval/77:
				allow_abstain: true
				onehundred_percent_base: valid

			meeting/1/users_enable_vote_weight: true

			user/1:
				is_present_in_meeting_ids: [1]
				meeting_user_ids: [10]
				default_vote_weight: "2.000000"
			meeting_user/10:
				user_id: 1
				group_ids: [1]
				meeting_id: 1
			`,
			"2",
		},
		{
			"Weight enabled, user has default weight and meeting weight",
			`
			poll/1:
				meeting_id: 1
				entitled_group_ids: [1]
				config_id: poll_config_approval/77
				visibility: open
				content_object_id: some_field/1
				sequential_number: 1
				title: myPoll

			poll_config_approval/77:
				allow_abstain: true
				onehundred_percent_base: valid

			meeting/1/users_enable_vote_weight: true

			user/1:
				is_present_in_meeting_ids: [1]
				meeting_user_ids: [10]
				default_vote_weight: "2.000000"
			meeting_user/10:
				user_id: 1
				group_ids: [1]
				meeting_id: 1
				vote_weight: "3.000000"
			`,
			"3",
		},
		{
			"Weight enabled, user has default weight and meeting weight in other meeting",
			`
			poll/1:
				meeting_id: 1
				entitled_group_ids: [1]
				config_id: poll_config_approval/77
				visibility: open
				content_object_id: some_field/1
				sequential_number: 1
				title: myPoll

			poll_config_approval/77:
				allow_abstain: true
				onehundred_percent_base: valid

			meeting/1/users_enable_vote_weight: true

			user/1:
				is_present_in_meeting_ids: [1]
				meeting_user_ids: [10,11]
				default_vote_weight: "2.000000"
			meeting_user/10:
				user_id: 1
				group_ids: [1]
				meeting_id: 1
			meeting_user/11:
				user_id: 1
				group_ids: [1]
				meeting_id: 2
				vote_weight: "3.000000"
			`,
			"2",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ds := dsfetch.New(dsmock.Stub(dsmock.YAMLData(tt.data)))
			weight, err := vote.CalcVoteWeight(t.Context(), ds, 10)
			if err != nil {
				t.Fatalf("CalcVote: %v", err)
			}

			if weight.String() != tt.expectWeight {
				t.Errorf("got weight %q, expected %q", weight, tt.expectWeight)
			}
		})
	}
}

func TestVoteStart(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("Postgres Test")
	}

	ctx := t.Context()

	pg, err := pgtest.NewPostgresTest(t)
	if err != nil {
		t.Fatalf("Error starting postgres: %v", err)
	}

	data := `---
	motion/5:
		meeting_id: 1
		sequential_number: 1
		title: my motion
		state_id: 1

	list_of_speakers/7:
		content_object_id: motion/5
		sequential_number: 1
		meeting_id: 1

	meeting/1:
		present_user_ids: [30]

	user:
		30:
			username: tom
		5:
			username: admin
			organization_management_level: superadmin

	meeting_user/300:
		group_ids: [40]
		user_id: 30
		meeting_id: 1

	group/40:
		name: delegate
		meeting_id: 1

	poll/5:
		title: normal poll
		config_id: poll_config_approval/77
		visibility: open
		sequential_number: 1
		content_object_id: motion/5
		meeting_id: 1
		state: created
		entitled_group_ids: [40]


	poll_config_approval/77:
		allow_abstain: true
		onehundred_percent_base: valid
	`

	withData(
		t,
		pg,
		data,
		func(service *vote.Vote, flow flow.Flow) {
			t.Run("Unknown poll", func(t *testing.T) {
				err := service.Start(ctx, 404, 5)
				if !errors.Is(err, vote.ErrNotExists) {
					t.Errorf("Start returned unexpected error: %v", err)
				}
			})

			t.Run("Not started poll", func(t *testing.T) {
				if err := service.Start(ctx, 5, 5); err != nil {
					t.Errorf("Start returned unexpected error: %v", err)
				}
			})

			t.Run("Start poll a second time", func(t *testing.T) {
				if err := service.Start(ctx, 5, 5); err != nil {
					t.Errorf("Start returned unexpected error: %v", err)
				}
			})

			t.Run("Start a finished poll", func(t *testing.T) {
				if err := service.Start(ctx, 5, 5); err != nil {
					t.Fatalf("Start returned unexpected error: %v", err)
				}

				if err := service.Finalize(ctx, 5, 5, false, false); err != nil {
					t.Fatalf("finish poll: %v", err)
				}

				err := service.Start(ctx, 5, 5)
				if !errors.Is(err, vote.ErrInvalid) {
					t.Errorf("Start returned unexpected error: %v", err)
				}
			})
		},
	)
}

func TestVoteFinalize(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("Postgres Test")
	}

	ctx := t.Context()

	pg, err := pgtest.NewPostgresTest(t)
	if err != nil {
		t.Fatalf("Error starting postgres: %v", err)
	}

	data := `---
	motion/5:
		meeting_id: 1
		sequential_number: 1
		title: my motion
		state_id: 1

	list_of_speakers/7:
		content_object_id: motion/5
		sequential_number: 1
		meeting_id: 1

	meeting/1:
		present_user_ids: [30]

	user:
		30:
			username: tom
		5:
			username: admin
			organization_management_level: superadmin

	meeting_user/300:
		group_ids: [40]
		user_id: 30
		meeting_id: 1

	meeting_user/500:
		group_ids: [40]
		user_id: 5
		meeting_id: 1

	group/40:
		name: delegate
		meeting_id: 1

	poll/5:
		title: poll with votes
		config_id: poll_config_approval/77
		visibility: open
		sequential_number: 1
		content_object_id: motion/5
		meeting_id: 1
		state: started
		entitled_group_ids: [40]

	poll_config_approval/77:
		allow_abstain: true
		onehundred_percent_base: valid

	poll_ballot/1:
		poll_id: 5
		value: '"yes"'
		represented_meeting_user_id: 300
	poll_ballot/2:
		poll_id: 5
		value: '"no"'
		represented_meeting_user_id: 500
	`

	withData(
		t,
		pg,
		data,
		func(service *vote.Vote, flow flow.Flow) {
			t.Run("Unknown poll", func(t *testing.T) {
				err := service.Finalize(ctx, 404, 5, false, false)
				if !errors.Is(err, vote.ErrNotExists) {
					t.Errorf("Stopping an unknown poll has to return an ErrNotExists, got: %v", err)
				}
			})

			t.Run("Poll with votes", func(t *testing.T) {
				if err := service.Finalize(ctx, 5, 5, false, false); err != nil {
					t.Fatalf("Stop returned unexpected error: %v", err)
				}

				poll, err := dsmodels.New(flow).Poll(5).First(ctx)
				if err != nil {
					t.Fatalf("load poll after finalize: %v", err)
				}

				if poll.Result != `{"no":"1","total_ballots":2,"yes":"1"}` {
					t.Errorf("Got result %s, expected %s", poll.Result, `{"no":"1","yes":"1"}`)
				}

				if poll.State != "finished" {
					t.Errorf("Poll state is %s, expected finished", poll.State)
				}

				if slices.Compare(poll.VotedIDs, []int{30, 5}) == 0 {
					t.Errorf("VotedIDs are %v, expected %v", poll.VotedIDs, []int{30, 5})
				}
			})

			t.Run("finish poll a second time", func(t *testing.T) {
				if err := service.Finalize(ctx, 5, 5, false, false); err != nil {
					t.Fatalf("Stop returned unexpected error: %v", err)
				}

				poll, err := dsmodels.New(flow).Poll(5).First(ctx)
				if err != nil {
					t.Fatalf("load poll after finalize: %v", err)
				}

				if poll.State != "finished" {
					t.Errorf("Poll state is %s, expected finished", poll.State)
				}
			})
		},
	)
}

func TestSecretPoll(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("Postgres Test")
	}

	ctx := t.Context()

	pg, err := pgtest.NewPostgresTest(t)
	if err != nil {
		t.Fatalf("Error starting postgres: %v", err)
	}

	data := `---
	motion/5:
		meeting_id: 1
		sequential_number: 1
		title: my motion
		state_id: 1

	list_of_speakers/7:
		content_object_id: motion/5
		sequential_number: 1
		meeting_id: 1

	meeting/1:
		present_user_ids: [30,31]

	user:
		30:
			username: tom
		31:
			username: hans
		5:
			username: admin
			organization_management_level: superadmin

	meeting_user/300:
		group_ids: [40]
		user_id: 30
		meeting_id: 1

	meeting_user/310:
		group_ids: [40]
		user_id: 31
		meeting_id: 1

	meeting_user/500:
		group_ids: [40]
		user_id: 5
		meeting_id: 1

	group/40:
		name: delegate
		meeting_id: 1

	poll/5:
		title: poll with votes
		config_id: poll_config_approval/77
		visibility: secret
		sequential_number: 1
		content_object_id: motion/5
		meeting_id: 1
		state: started
		entitled_group_ids: [40]

	poll_config_approval/77:
		allow_abstain: true
		onehundred_percent_base: valid
	`

	withData(
		t,
		pg,
		data,
		func(service *vote.Vote, flow flow.Flow) {
			t.Run("Vote1", func(t *testing.T) {
				body := `{"value":"Yes"}`
				if err := service.Vote(ctx, 5, 30, strings.NewReader(body)); err != nil {
					t.Fatalf("Error voting for poll: %v", err)
				}

				ds := dsmodels.New(flow)
				ballot, err := ds.PollBallot(1).First(t.Context())
				if err != nil {
					t.Fatalf("Error: Getting ballot: %v", err)
				}

				if ballot.Value == `"Yes"` {
					t.Errorf("ballot value was not encrypted")
				}
			})

			t.Run("Vote2", func(t *testing.T) {
				body := `{"value":"Yes"}`
				if err := service.Vote(ctx, 5, 31, strings.NewReader(body)); err != nil {
					t.Fatalf("Error voting for poll: %v", err)
				}

				ds := dsmodels.New(flow)
				ballotList, err := ds.PollBallot(1, 2).Get(t.Context())
				if err != nil {
					t.Fatalf("Error: Getting ballot: %v", err)
				}

				if len(ballotList) != 2 {
					t.Fatalf("Got %d ballots, expted 2", len(ballotList))
				}

				if ballotList[0].Value == ballotList[1].Value {
					t.Errorf("Two ballots with the same value where identical in db")
				}
			})

			t.Run("Finalize", func(t *testing.T) {
				err := service.Finalize(ctx, 5, 5, false, false)
				if err != nil {
					t.Fatalf("Error finalizing poll: %v", err)
				}

				ds := dsmodels.New(flow)
				q := ds.Poll(5)
				q = q.Preload(q.BallotList())
				poll, err := q.First(ctx)
				if err != nil {
					t.Fatalf("Error: Getting poll: %v", err)
				}

				expectResult := `{"total_ballots":2,"yes":"2"}`
				if poll.Result != expectResult {
					t.Errorf("Got result %s, expected %s", poll.Result, expectResult)
				}

				for _, ballot := range poll.BallotList {
					if ballot.Value != `"Yes"` {
						t.Errorf("value of ballot %d was not decrypted", ballot.ID)
					}
				}
			})
		},
	)
}

func TestVoteVote(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("Postgres Test")
	}

	ctx := t.Context()

	pg, err := pgtest.NewPostgresTest(t)
	if err != nil {
		t.Fatalf("Error starting postgres: %v", err)
	}

	data := `---
	motion/5:
		meeting_id: 1
		sequential_number: 1
		title: my motion
		state_id: 1

	list_of_speakers/7:
		content_object_id: motion/5
		sequential_number: 1
		meeting_id: 1

	meeting/1:
		present_user_ids: [30]

	user:
		30:
			username: tom
		5:
			username: admin
			organization_management_level: superadmin

	meeting_user/300:
		group_ids: [40]
		user_id: 30
		meeting_id: 1

	group/40:
		name: delegate
		meeting_id: 1

	poll/5:
		title: poll with votes
		config_id: poll_config_approval/77
		visibility: open
		sequential_number: 1
		content_object_id: motion/5
		meeting_id: 1
		state: started
		entitled_group_ids: [40]

	poll_config_approval/77:
		allow_abstain: true
		onehundred_percent_base: valid
	`

	withData(
		t,
		pg,
		data,
		func(service *vote.Vote, flow flow.Flow) {
			t.Run("Poll does not exist in DS", func(t *testing.T) {
				err := service.Vote(ctx, 404, 1, strings.NewReader(`{"value":"Y"}`))
				if !errors.Is(err, vote.ErrNotExists) {
					t.Errorf("Expected ErrNotExists, got: %v", err)
				}
			})

			t.Run("Invalid json", func(t *testing.T) {
				err := service.Vote(ctx, 5, 30, strings.NewReader(`{123`))

				var errTyped vote.TypeError
				if !errors.As(err, &errTyped) {
					t.Fatalf("Vote() did not return an TypeError, got: %v", err)
				}

				if errTyped != vote.ErrInvalid {
					t.Errorf("Got error type `%s`, expected `%s`", errTyped.Type(), vote.ErrInvalid.Type())
				}
			})

			t.Run("Invalid format", func(t *testing.T) {
				err := service.Vote(ctx, 5, 30, strings.NewReader(`{}`))

				if _, ok := errors.AsType[method.InvalidBallotError](err); !ok {
					t.Fatalf("Vote() did not return an TypeError, got: %v", err)
				}
			})

			t.Run("Valid data", func(t *testing.T) {
				err := service.Vote(ctx, 5, 30, strings.NewReader(`{"value":"Yes"}`))
				if err != nil {
					t.Fatalf("Vote returned unexpected error: %v", err)
				}
			})

			t.Run("User has voted", func(t *testing.T) {
				err := service.Vote(ctx, 5, 30, strings.NewReader(`{"value":"Yes"}`))
				if err == nil {
					t.Fatalf("Vote returned no error")
				}

				var errTyped vote.TypeError
				if !errors.As(err, &errTyped) {
					t.Fatalf("Vote() did not return an TypeError, got: %v", err)
				}

				if errTyped != vote.ErrDoubleVote {
					t.Errorf("Got error type `%s`, expected `%s`", errTyped.Type(), vote.ErrDoubleVote.Type())
				}
			})

			t.Run("Poll is stopped", func(t *testing.T) {
				if err := service.Finalize(ctx, 5, 5, false, false); err != nil {
					t.Fatalf("Finalize poll: %v", err)
				}

				err := service.Vote(ctx, 5, 30, strings.NewReader(`{"value":"Yes"}`))
				if err == nil {
					t.Fatalf("Vote returned no error")
				}

				var errTyped vote.TypeError
				if !errors.As(err, &errTyped) {
					t.Fatalf("Vote() did not return an TypeError, got: %v", err)
				}

				if errTyped != vote.ErrNotStarted {
					t.Errorf("Got error type `%s`, expected `%s`", errTyped.Type(), vote.ErrNotStarted.Type())
				}
			})
		},
	)
}

func TestVoteDelegationAndGroup(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("Postgres Test")
	}

	baseData := `
	meeting/1/users_enable_vote_delegations: true

	motion/5:
		meeting_id: 1
		sequential_number: 1
		title: my motion
		state_id: 1

	list_of_speakers/7:
		content_object_id: motion/5
		sequential_number: 1
		meeting_id: 1

	group/40:
		name: delegates
		meeting_id: 1

	group/41:
		name: some_group
		meeting_id: 1

	user:
		5:
			username: admin
			organization_management_level: superadmin
		30:
			username: tom

		40:
			username: georg

	meeting_user:
		31:
			user_id: 30
			meeting_id: 1
			group_ids: [41]

		41:
			user_id: 40
			meeting_id: 1
			group_ids: [41]

	poll_config_approval/77:
		allow_abstain: true
		onehundred_percent_base: valid

	poll/5:
		title: normal poll
		config_id: poll_config_approval/77
		visibility: open
		sequential_number: 1
		content_object_id: motion/5
		meeting_id: 1
		state: started
		entitled_group_ids: [40]
	`

	for _, tt := range []struct {
		name string
		data string
		vote string

		expectRepresentedMeetingUserID int
	}{
		{
			"Not delegated",
			`
			user/30:
				is_present_in_meeting_ids: [1]

			meeting_user/31:
				group_ids: [40]
			`,
			`{"value":"Yes"}`,

			31,
		},

		{
			"Not delegated not present",
			`
			meeting_user/31:
				group_ids: [40]
			`,
			`{"value":"Yes"}`,

			0,
		},

		{
			"Not delegated not in group",
			`
			user/30:
				is_present_in_meeting_ids: [1]
			`,
			`{"value":"Yes"}`,

			0,
		},

		{
			"Vote for self",
			`
			user/30:
				is_present_in_meeting_ids: [1]

			meeting_user/31:
				group_ids: [40]
			`,
			`{"meeting_user_id": 31, "value":"Yes"}`,

			31,
		},

		{
			"Vote for self not activated",
			`
			meeting/1/users_enable_vote_delegations: false
			user/30:
				is_present_in_meeting_ids: [1]

			meeting_user/31:
				group_ids: [40]
			`,
			`{"meeting_user_id": 31, "value":"Yes"}`,

			31,
		},

		{
			"Vote for anonymous",
			`
			user/30:
				is_present_in_meeting_ids: [1]

			meeting_user/31:
				group_ids: [40]
			`,
			`{"meeting_user_id": 0, "value":"Yes"}`,

			0,
		},

		{
			"Vote for other without delegation",
			`
			user/30:
				is_present_in_meeting_ids: [1]

			meeting_user/31:
				group_ids: [40]
			`,
			`{"meeting_user_id": 41, "value":"Yes"}`,

			0,
		},

		{
			"Vote for other with delegation",
			`
			user/30:
				is_present_in_meeting_ids: [1]

			meeting_user:
				41:
					group_ids: [40]
					vote_delegated_to_id: 31
			`,
			`{"meeting_user_id": 41, "value":"Yes"}`,

			41,
		},

		{
			"Vote for other with delegation not activated",
			`
			meeting/1/users_enable_vote_delegations: false
			user/30:
				is_present_in_meeting_ids: [1]

			meeting_user:
				41:
					group_ids: [40]
					vote_delegated_to_id: 31
			`,
			`{"meeting_user_id": 41, "value":"Yes"}`,

			0,
		},

		{
			"Vote for other with delegation not in group",
			`
			user/30:
				is_present_in_meeting_ids: [1]

			meeting_user:
				41:
					vote_delegated_to_id: 31
			`,
			`{"meeting_user_id": 41, "value":"Yes"}`,

			0,
		},

		{
			"Vote for self when delegation is activated users_forbid_delegator_to_vote==false",
			`
			meeting/1/users_forbid_delegator_to_vote: false
			user/30:
				is_present_in_meeting_ids: [1]

			meeting_user/31:
				group_ids: [40]
				vote_delegated_to_id: 41
			`,
			`{"meeting_user_id": 31, "value":"Yes"}`,

			31,
		},

		{
			"Vote for self when delegation is activated users_forbid_delegator_to_vote==true",
			`
			meeting/1/users_forbid_delegator_to_vote: true
			user/30:
				is_present_in_meeting_ids: [1]

			meeting_user/31:
				group_ids: [40]
				vote_delegated_to_id: 41
			`,
			`{"meeting_user_id": 31, "value":"Yes"}`,

			0,
		},

		{
			"Vote for self when delegation is deactivated users_forbid_delegator_to_vote==true",
			`
			meeting/1:
				users_forbid_delegator_to_vote: true
				users_enable_vote_delegations: false

			user/30:
				is_present_in_meeting_ids: [1]

			meeting_user/31:
				group_ids: [40]
				vote_delegated_to_id: 41
			`,
			`{"meeting_user_id": 31, "value":"Yes"}`,

			31,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			pg, err := pgtest.NewPostgresTest(t)
			if err != nil {
				t.Fatalf("Error starting postgres: %v", err)
			}

			if err := pg.AddData(ctx, baseData); err != nil {
				t.Fatalf("Insert base data: %v", err)
			}

			withData(
				t,
				pg,
				tt.data,
				func(service *vote.Vote, flow flow.Flow) {
					err := service.Vote(ctx, 5, 30, strings.NewReader(tt.vote))

					if tt.expectRepresentedMeetingUserID != 0 {
						if err != nil {
							t.Fatalf("Vote returned unexpected error: %v", err)
						}

						ds := dsmodels.New(flow)
						q := ds.Poll(5)
						q = q.Preload(q.BallotList())
						poll, err := q.First(ctx)
						if err != nil {
							t.Fatalf("Error: Getting votes from poll: %v", err)
						}
						found := slices.ContainsFunc(poll.BallotList, func(vote dsmodels.PollBallot) bool {
							userID, _ := vote.RepresentedMeetingUserID.Value()
							return userID == tt.expectRepresentedMeetingUserID
						})

						if !found {
							t.Errorf("user %d has not voted", tt.expectRepresentedMeetingUserID)
						}

						return
					}

					if !errors.Is(err, vote.ErrNotAllowed) {
						t.Fatalf("Expected NotAllowedError, got: %v", err)
					}
				},
			)
		})
	}
}

func TestDeleteWithOptionsAndBallots(t *testing.T) {
	t.Parallel()

	if testing.Short() {
		t.Skip("Postgres Test")
	}

	ctx := t.Context()

	pg, err := pgtest.NewPostgresTest(t)
	if err != nil {
		t.Fatalf("Error starting postgres: %v", err)
	}

	data := `---
	assignment/5:
		meeting_id: 1
		sequential_number: 1
		title: my assignment

	list_of_speakers/7:
		content_object_id: assignment/5
		sequential_number: 1
		meeting_id: 1

	meeting/1:
		present_user_ids: [30]

	user:
		30:
			username: tom
		5:
			username: admin
			organization_management_level: superadmin

	meeting_user/300:
		group_ids: [40]
		user_id: 30
		meeting_id: 1

	group/40:
		name: delegate
		meeting_id: 1

	poll/5:
		title: poll with votes and options
		config_id: poll_config_selection/77
		visibility: open
		sequential_number: 1
		content_object_id: assignment/5
		meeting_id: 1
		state: started
		entitled_group_ids: [40]

	poll_config_selection/77:
		onehundred_percent_base: valid

	poll_option/31:
		poll_id: 5
		meeting_user_id: 300

	poll_ballot:
		501:
			poll_id: 5
			value: Yes
		502:
			poll_id: 5
			value: No
	`

	withData(
		t,
		pg,
		data,
		func(service *vote.Vote, flow flow.Flow) {
			err := service.Delete(ctx, 5, 5)
			if err != nil {
				t.Errorf("Error: %v", err)
			}
		},
	)
}

func withData(t *testing.T, pg *pgtest.PostgresTest, data string, fn func(service *vote.Vote, flow flow.Flow)) {
	t.Helper()

	ctx := t.Context()

	if err := pg.AddData(ctx, data); err != nil {
		t.Fatalf("Error: inserting data: %v", err)
	}

	flow, err := pg.Flow()
	if err != nil {
		t.Fatalf("Error getting flow: %v", err)
	}
	defer flow.Close()

	conn, err := pg.Conn(ctx)
	if err != nil {
		t.Fatalf("Error getting connection: %v", err)
	}
	defer conn.Close(ctx)

	service, _, err := vote.New(environment.ForTests{}, flow, conn)
	if err != nil {
		t.Fatalf("Error creating vote: %v", err)
	}

	fn(service, flow)
}
