package vote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"sync"
	"time"

	"github.com/OpenSlides/openslides-go/datastore/dsfetch"
	"github.com/OpenSlides/openslides-go/datastore/dsmodels"
	"github.com/OpenSlides/openslides-go/datastore/dsrecorder"
	"github.com/OpenSlides/openslides-go/datastore/flow"
	"github.com/OpenSlides/openslides-vote-service/log"
	"github.com/shopspring/decimal"
)

// Vote holds the state of the service.
//
// Vote has to be initializes with vote.New().
type Vote struct {
	fastBackend Backend
	longBackend Backend
	flow        flow.Flow

	liveVotesMu sync.Mutex
	liveVotes   map[int]map[int][]byte // voted holds for all running polls, the votes of a user
}

// New creates an initializes vote service.
func New(ctx context.Context, fast, long Backend, flow flow.Flow, singleInstance bool) (*Vote, func(context.Context, func(error)), error) {
	v := &Vote{
		fastBackend: fast,
		longBackend: long,
		flow:        flow,
	}

	if err := v.loadVoted(ctx); err != nil {
		return nil, nil, fmt.Errorf("loading voted: %w", err)
	}

	bg := func(ctx context.Context, errorHandler func(error)) {
		go v.flow.Update(ctx, nil)

		if singleInstance {
			return
		}

		go func() {
			for {
				if err := v.loadVoted(ctx); err != nil {
					errorHandler(err)
				}
				time.Sleep(time.Second)
			}
		}()
	}

	return v, bg, nil
}

// backend returns the poll backend for a pollConfig object.
func (v *Vote) backend(p dsmodels.Poll) Backend {
	backend := v.longBackend
	if p.Backend == "fast" {
		backend = v.fastBackend
	}
	log.Debug("Used backend: %v", backend)
	return backend
}

// Start an electronic vote.
//
// This function is idempotence. If you call it with the same input, you will
// get the same output. This means, that when a poll is stopped, Start() will
// not throw an error.
func (v *Vote) Start(ctx context.Context, pollID int) error {
	recorder := dsrecorder.New(v.flow)
	ds := dsmodels.New(recorder)

	poll, err := ds.Poll(pollID).First(ctx)
	if err != nil {
		var doesNotExist dsfetch.DoesNotExistError
		if errors.As(err, &doesNotExist) {
			return MessageErrorf(ErrNotExists, "Poll %d does not exist", pollID)
		}
		return fmt.Errorf("loading poll: %w", err)
	}

	if poll.Type == "analog" {
		return MessageError(ErrInvalid, "Analog poll can not be started")
	}

	if err := preload(ctx, &ds.Fetch, poll); err != nil {
		return fmt.Errorf("preloading data: %w", err)
	}
	log.Debug("Preload cache. Received keys: %v", recorder.Keys())

	backend := v.backend(poll)
	if err := backend.Start(ctx, pollID); err != nil {
		return fmt.Errorf("starting poll in the backend: %w", err)
	}

	return nil
}

// StopResult is the return value from vote.Stop.
type StopResult struct {
	Votes   [][]byte
	UserIDs []int
}

// Stop ends a poll.
//
// This method is idempotence. Many requests with the same pollID will return
// the same data. Calling vote.Clear will stop this behavior.
func (v *Vote) Stop(ctx context.Context, pollID int) (StopResult, error) {
	ds := dsmodels.New(v.flow)
	poll, err := ds.Poll(pollID).First(ctx)
	if err != nil {
		var doesNotExist dsfetch.DoesNotExistError
		if errors.As(err, &doesNotExist) {
			return StopResult{}, MessageErrorf(ErrNotExists, "Poll %d does not exist", pollID)
		}
		return StopResult{}, fmt.Errorf("loading poll: %w", err)
	}

	backend := v.backend(poll)
	ballots, userIDs, err := backend.Stop(ctx, pollID)
	if err != nil {
		var errNotExist interface{ DoesNotExist() }
		if errors.As(err, &errNotExist) {
			return StopResult{}, MessageErrorf(ErrNotExists, "Poll %d does not exist in the backend", pollID)
		}

		return StopResult{}, fmt.Errorf("fetching vote objects: %w", err)
	}

	return StopResult{ballots, userIDs}, nil
}

// Clear removes all knowlage of a poll.
func (v *Vote) Clear(ctx context.Context, pollID int) error {
	if err := v.fastBackend.Clear(ctx, pollID); err != nil {
		return fmt.Errorf("clearing fastBackend: %w", err)
	}

	if err := v.longBackend.Clear(ctx, pollID); err != nil {
		return fmt.Errorf("clearing longBackend: %w", err)
	}

	v.liveVotesMu.Lock()
	v.liveVotes[pollID] = nil
	v.liveVotesMu.Unlock()

	return nil
}

// ClearAll removes all knowlage of all polls and the datastore-cache.
func (v *Vote) ClearAll(ctx context.Context) error {
	// Reset the cache if it has the ResetCach() method.
	type ResetCacher interface {
		Reset()
	}
	if r, ok := v.flow.(ResetCacher); ok {
		r.Reset()
	}

	if err := v.fastBackend.ClearAll(ctx); err != nil {
		return fmt.Errorf("clearing fastBackend: %w", err)
	}

	if err := v.longBackend.ClearAll(ctx); err != nil {
		return fmt.Errorf("clearing long Backend: %w", err)
	}

	v.liveVotesMu.Lock()
	v.liveVotes = make(map[int]map[int][]byte)
	v.liveVotesMu.Unlock()

	return nil
}

// Vote validates and saves the vote.
func (v *Vote) Vote(ctx context.Context, pollID, requestUser int, r io.Reader) error {
	ds := dsmodels.New(v.flow)
	poll, err := ds.Poll(pollID).First(ctx)
	if err != nil {
		var doesNotExist dsfetch.DoesNotExistError
		if errors.As(err, &doesNotExist) {
			return MessageErrorf(ErrNotExists, "Poll %d does not exist", pollID)
		}
		return fmt.Errorf("loading poll: %w", err)
	}
	log.Debug("Poll config: %v", poll)

	if err := ensurePresent(ctx, &ds.Fetch, poll.MeetingID, requestUser); err != nil {
		return err
	}

	var vote ballot
	if err := json.NewDecoder(r).Decode(&vote); err != nil {
		return MessageErrorf(ErrInvalid, "decoding payload: %v", err)
	}

	voteUser, exist := vote.UserID.Value()
	if !exist {
		voteUser = requestUser
	}

	if voteUser == 0 {
		return MessageError(ErrNotAllowed, "Votes for anonymous user are not allowed")
	}

	voteMeetingUserID, found, err := getMeetingUser(ctx, &ds.Fetch, voteUser, poll.MeetingID)
	if err != nil {
		return fmt.Errorf("get meeting user for vote user: %w", err)
	}

	if !found {
		return MessageError(ErrNotAllowed, "You are not in the right meeting")
	}

	if err := ensureVoteUser(ctx, &ds.Fetch, poll, voteUser, voteMeetingUserID, requestUser); err != nil {
		return err
	}

	if validation := validate(poll, vote.Value); validation != "" {
		return MessageError(ErrInvalid, validation)
	}

	// voteData.Weight is a DecimalField with 6 zeros.
	var voteWeightEnabled bool
	var meetingUserVoteWeight decimal.Decimal
	var userDefaultVoteWeight decimal.Decimal
	ds.Meeting_UsersEnableVoteWeight(poll.MeetingID).Lazy(&voteWeightEnabled)
	ds.MeetingUser_VoteWeight(voteMeetingUserID).Lazy(&meetingUserVoteWeight)
	ds.User_DefaultVoteWeight(voteUser).Lazy(&userDefaultVoteWeight)

	if err := ds.Execute(ctx); err != nil {
		return fmt.Errorf("getting vote weight: %w", err)
	}

	var voteWeight decimal.Decimal
	if voteWeightEnabled {
		voteWeight = meetingUserVoteWeight
		if voteWeight.IsZero() {
			voteWeight = userDefaultVoteWeight
		}
	}

	if voteWeight.IsZero() {
		voteWeight = decimal.NewFromInt(1)
	}

	log.Debug("Using voteWeight %s", voteWeight.String())

	voteData := struct {
		RequestUser int             `json:"request_user_id,omitempty"`
		VoteUser    int             `json:"vote_user_id,omitempty"`
		Value       json.RawMessage `json:"value"`
		Weight      string          `json:"weight"`
	}{
		requestUser,
		voteUser,
		vote.Value.original,
		voteWeight.StringFixed(6),
	}

	if poll.Type != "named" {
		voteData.RequestUser = 0
		voteData.VoteUser = 0
	}

	bs, err := json.Marshal(voteData)
	if err != nil {
		return fmt.Errorf("decoding vote data: %w", err)
	}

	if err := v.backend(poll).Vote(ctx, pollID, voteUser, bs); err != nil {
		var errNotExist interface{ DoesNotExist() }
		if errors.As(err, &errNotExist) {
			return ErrNotExists
		}

		var errDoubleVote interface{ DoubleVote() }
		if errors.As(err, &errDoubleVote) {
			return ErrDoubleVote
		}

		var errNotOpen interface{ Stopped() }
		if errors.As(err, &errNotOpen) {
			return ErrStopped
		}

		return fmt.Errorf("save vote: %w", err)
	}

	var liveVote []byte
	if poll.Type == "named" {
		liveVote = bs
	}

	v.liveVotesMu.Lock()
	if v.liveVotes[pollID] == nil {
		v.liveVotes[pollID] = make(map[int][]byte)
	}
	v.liveVotes[pollID][voteUser] = liveVote
	v.liveVotesMu.Unlock()

	return nil
}

// getMeetingUser returns the meeting_user id between a userID and a meetingID.
func getMeetingUser(ctx context.Context, fetch *dsfetch.Fetch, userID, meetingID int) (int, bool, error) {
	meetingUserIDs, err := fetch.User_MeetingUserIDs(userID).Value(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("getting all meeting_user ids: %w", err)
	}

	meetingIDs := make([]int, len(meetingUserIDs))
	for i := 0; i < len(meetingUserIDs); i++ {
		fetch.MeetingUser_MeetingID(meetingUserIDs[i]).Lazy(&meetingIDs[i])
	}

	if err := fetch.Execute(ctx); err != nil {
		return 0, false, fmt.Errorf("get all meeting IDs: %w", err)
	}

	for i, mid := range meetingIDs {
		if mid == meetingID {
			return meetingUserIDs[i], true, nil
		}
	}

	return 0, false, nil
}

// ensurePresent makes sure that the user sending the vote request is present.
func ensurePresent(ctx context.Context, ds *dsfetch.Fetch, meetingID, user int) error {
	presentMeetings, err := ds.User_IsPresentInMeetingIDs(user).Value(ctx)
	if err != nil {
		return fmt.Errorf("fetching is present in meetings: %w", err)
	}

	for _, present := range presentMeetings {
		if present == meetingID {
			return nil
		}
	}
	return MessageErrorf(ErrNotAllowed, "You have to be present in meeting %d", meetingID)
}

// ensureVoteUser makes sure the user from the vote:
// * the delegation is correct and
// * is in the correct group
func ensureVoteUser(ctx context.Context, ds *dsfetch.Fetch, poll dsmodels.Poll, voteUser, voteMeetingUserID, requestUser int) error {
	groupIDs, err := ds.MeetingUser_GroupIDs(voteMeetingUserID).Value(ctx)
	if err != nil {
		return fmt.Errorf("fetching groups of user %d in meeting %d: %w", voteUser, poll.MeetingID, err)
	}

	if !equalElement(groupIDs, poll.EntitledGroupIDs) {
		return MessageErrorf(ErrNotAllowed, "User %d is not allowed to vote. He is not in an entitled group", voteUser)
	}

	delegationActivated, err := ds.Meeting_UsersEnableVoteDelegations(poll.MeetingID).Value(ctx)
	if err != nil {
		return fmt.Errorf("fetching user enable vote delegation: %w", err)
	}

	forbitDelegateToVote, err := ds.Meeting_UsersForbidDelegatorToVote(poll.MeetingID).Value(ctx)
	if err != nil {
		return fmt.Errorf("getting users_forbid_delegator_to_vote: %w", err)
	}

	delegation, err := ds.MeetingUser_VoteDelegatedToID(voteMeetingUserID).Value(ctx)
	if err != nil {
		return fmt.Errorf("fetching delegation : %w", err)
	}

	if delegationActivated && forbitDelegateToVote && !delegation.Null() && voteUser == requestUser {
		return MessageError(ErrNotAllowed, "You have delegated your vote and therefore can not vote for your self")
	}

	if voteUser == requestUser {
		return nil
	}

	log.Debug("Vote delegation")

	if !delegationActivated {
		return MessageErrorf(ErrNotAllowed, "Vote delegation is not activated in meeting %d", poll.MeetingID)
	}

	requestMeetingUserID, found, err := getMeetingUser(ctx, ds, requestUser, poll.MeetingID)
	if err != nil {
		return fmt.Errorf("getting meeting_user for request user: %w", err)
	}

	if !found {
		return MessageError(ErrNotAllowed, "You are not in the right meeting")
	}

	if id, ok := delegation.Value(); !ok || id != requestMeetingUserID {
		return MessageErrorf(ErrNotAllowed, "You can not vote for user %d", voteUser)
	}

	return nil
}

// delegatedUserIDs returns all user ids for which the user can vote.
func delegatedUserIDs(ctx context.Context, fetch *dsfetch.Fetch, userID int) ([]int, error) {
	meetingUserIDs, err := fetch.User_MeetingUserIDs(userID).Value(ctx)
	if err != nil {
		return nil, fmt.Errorf("fetching meeting user: %w", err)
	}

	meetingUserDelegationsIDs := make([][]int, len(meetingUserIDs))
	for i, muid := range meetingUserIDs {
		fetch.MeetingUser_VoteDelegationsFromIDs(muid).Lazy(&meetingUserDelegationsIDs[i])
	}

	if err := fetch.Execute(ctx); err != nil {
		return nil, fmt.Errorf("getting vote_delegation_from values: %w", err)
	}

	var delegatedMeetingUserIDs []int
	for i := range meetingUserDelegationsIDs {
		delegatedMeetingUserIDs = append(delegatedMeetingUserIDs, meetingUserDelegationsIDs[i]...)
	}

	userIDs := make([]int, len(delegatedMeetingUserIDs))
	for i := range delegatedMeetingUserIDs {
		fetch.MeetingUser_UserID(delegatedMeetingUserIDs[i]).Lazy(&userIDs[i])
	}

	if err := fetch.Execute(ctx); err != nil {
		return nil, fmt.Errorf("getting user_ids from meeting_user_ids: %w", err)
	}

	return userIDs, nil
}

// Voted tells, on which the requestUser has already voted.
func (v *Vote) Voted(ctx context.Context, pollIDs []int, requestUser int) (map[int][]int, error) {
	ds := dsfetch.New(v.flow)
	userIDs, err := delegatedUserIDs(ctx, ds, requestUser)
	if err != nil {
		return nil, fmt.Errorf("getting all delegated users: %w", err)
	}

	requestedUserIDs := make(map[int]struct{}, len(userIDs)+1)
	requestedUserIDs[requestUser] = struct{}{}
	for _, uid := range userIDs {
		requestedUserIDs[uid] = struct{}{}
	}

	requestedPollIDs := make(map[int]struct{}, len(pollIDs))
	for _, pid := range pollIDs {
		requestedPollIDs[pid] = struct{}{}
	}

	v.liveVotesMu.Lock()
	defer v.liveVotesMu.Unlock()

	out := make(map[int][]int, len(pollIDs))
	for pid, userID2Vote := range v.liveVotes {
		if _, ok := requestedPollIDs[pid]; !ok {
			continue
		}

		for uid := range maps.Keys(userID2Vote) {
			if _, ok := requestedUserIDs[uid]; ok {
				out[pid] = append(out[pid], uid)
			}
		}
	}

	for _, pid := range pollIDs {
		if _, ok := out[pid]; !ok {
			out[pid] = nil
		}
	}

	return out, nil
}

// AllLiveVotes returns for all running polls the vote from each user.
func (v *Vote) AllLiveVotes(ctx context.Context) map[int]map[int]*string {
	v.liveVotesMu.Lock()
	defer v.liveVotesMu.Unlock()

	ds := dsmodels.New(v.flow)

	out := make(map[int]map[int]*string, len(v.liveVotes))
	for pollID, userID2Vote := range v.liveVotes {
		poll, err := ds.Poll(pollID).First(ctx)
		if err != nil {
			continue
		}

		out[pollID] = make(map[int]*string, len(userID2Vote))

		if poll.LiveVotingEnabled && poll.Type == "named" {
			// Only send votes an votes, where live voting is enabled and its a
			// named vote. Remove the votes for all other votes.
			for userID, vote := range userID2Vote {
				if vote == nil {
					out[pollID] = nil
					continue
				}
				str := string(vote)
				out[pollID][userID] = &str
			}
			continue
		}

		for userID := range userID2Vote {
			out[pollID][userID] = nil
		}
	}
	return out
}

// loadVoted creates the value for v.voted by the backends.
func (v *Vote) loadVoted(ctx context.Context) error {
	combinedData, err := v.fastBackend.LiveVotes(ctx)
	if err != nil {
		return fmt.Errorf("fetching data from fast backend: %w", err)
	}

	longData, err := v.longBackend.LiveVotes(ctx)
	if err != nil {
		return fmt.Errorf("fetching data from long backend: %w", err)
	}

	maps.Copy(combinedData, longData)

	v.liveVotesMu.Lock()
	v.liveVotes = combinedData
	v.liveVotesMu.Unlock()
	return nil
}

// Backend is a storage for the poll options.
type Backend interface {
	// Start opens the poll for votes. To start a poll that is already started
	// is ok. To start an stopped poll is also ok, but it has to be a noop (the
	// stop-state does not change).
	Start(ctx context.Context, pollID int) error

	// Vote saves vote data into the backend. The backend has to check that the
	// poll is started and the userID has not voted before.
	//
	// If the user has already voted, an Error with method `DoubleVote()` has to
	// be returned. If the poll has not started, an error with the method
	// `DoesNotExist()` is required. An a stopped vote, it has to be `Stopped()`.
	//
	// The return value is the number of already voted objects.
	Vote(ctx context.Context, pollID int, userID int, object []byte) error

	// Stop ends a poll and returns all poll objects and all userIDs from users
	// that have voted. It is ok to call Stop() on a stopped poll. On a unknown
	// poll `DoesNotExist()` has to be returned.
	Stop(ctx context.Context, pollID int) ([][]byte, []int, error)

	// Clear has to remove all data. It can be called on a started or stopped or
	// non existing poll.
	Clear(ctx context.Context, pollID int) error

	// ClearAll removes all data from the backend.
	ClearAll(ctx context.Context) error

	// LiveVotes returns all votes from each user.
	LiveVotes(ctx context.Context) (map[int]map[int][]byte, error)

	fmt.Stringer
}

// preload loads all data in the cache, that is needed later for the vote
// requests.
func preload(ctx context.Context, ds *dsfetch.Fetch, poll dsmodels.Poll) error {
	var dummyBool bool
	var dummyIntSlice []int
	var dummyDecimal decimal.Decimal
	var dummyManybeInt dsfetch.Maybe[int]
	var dummyInt int
	ds.Meeting_UsersEnableVoteWeight(poll.MeetingID).Lazy(&dummyBool)
	ds.Meeting_UsersEnableVoteDelegations(poll.MeetingID).Lazy(&dummyBool)
	ds.Meeting_UsersForbidDelegatorToVote(poll.MeetingID).Lazy(&dummyBool)

	meetingUserIDsList := make([][]int, len(poll.EntitledGroupIDs))
	for i, groupID := range poll.EntitledGroupIDs {
		ds.Group_MeetingUserIDs(groupID).Lazy(&meetingUserIDsList[i])
	}

	// First database request to get meeting/enable_vote_weight and all
	// meeting_users from all entitled groups.
	if err := ds.Execute(ctx); err != nil {
		return fmt.Errorf("fetching users: %w", err)
	}

	var userIDs []*int
	for _, meetingUserIDs := range meetingUserIDsList {
		for _, muID := range meetingUserIDs {
			var uid int
			userIDs = append(userIDs, &uid)
			ds.MeetingUser_UserID(muID).Lazy(&uid)
			ds.MeetingUser_GroupIDs(muID).Lazy(&dummyIntSlice)
			ds.MeetingUser_VoteWeight(muID).Lazy(&dummyDecimal)
			ds.MeetingUser_VoteDelegatedToID(muID).Lazy(&dummyManybeInt)
			ds.MeetingUser_MeetingID(muID).Lazy(&dummyInt)
		}
	}

	// Second database request to get all user ids and meeting_user_data.
	if err := ds.Execute(ctx); err != nil {
		return fmt.Errorf("preload meeting user data: %w", err)
	}

	var delegatedMeetingUserIDs []int
	for _, muIDs := range meetingUserIDsList {
		for _, muID := range muIDs {
			// This does not send a db request, since the value was fetched in
			// the block above.
			mID, err := ds.MeetingUser_VoteDelegatedToID(muID).Value(ctx)
			if err != nil {
				return fmt.Errorf("getting vote delegated to for meeting user %d: %w", muID, err)
			}
			if id, ok := mID.Value(); ok {
				delegatedMeetingUserIDs = append(delegatedMeetingUserIDs, id)
			}
		}
	}

	delegatedUserIDs := make([]int, len(delegatedMeetingUserIDs))
	for i, muID := range delegatedMeetingUserIDs {
		ds.MeetingUser_UserID(muID).Lazy(&delegatedUserIDs[i])
		ds.MeetingUser_MeetingID(muID).Lazy(&dummyInt)
	}

	// Third database request to get all delegated user ids. Only fetches data
	// if there are delegates.
	if err := ds.Execute(ctx); err != nil {
		return fmt.Errorf("preloading delegate user ids: %w", err)
	}

	for _, uID := range userIDs {
		ds.User_DefaultVoteWeight(*uID).Lazy(&dummyDecimal)
		ds.User_MeetingUserIDs(*uID).Lazy(&dummyIntSlice)
		ds.User_IsPresentInMeetingIDs(*uID).Lazy(&dummyIntSlice)
	}
	for _, uID := range delegatedUserIDs {
		ds.User_IsPresentInMeetingIDs(uID).Lazy(&dummyIntSlice)
		ds.User_MeetingUserIDs(uID).Lazy(&dummyIntSlice)
	}

	// Thrid or forth database request to get is present_in_meeting for all users and delegates.
	if err := ds.Execute(ctx); err != nil {
		return fmt.Errorf("preloading user data: %w", err)
	}

	return nil
}

type maybeInt struct {
	unmarshalled bool
	value        int
}

func (m *maybeInt) UnmarshalJSON(b []byte) error {
	if err := json.Unmarshal(b, &m.value); err != nil {
		return fmt.Errorf("decoding value as int: %w", err)
	}
	m.unmarshalled = true
	return nil
}

func (m *maybeInt) Value() (int, bool) {
	return m.value, m.unmarshalled
}

type ballot struct {
	UserID maybeInt    `json:"user_id"`
	Value  ballotValue `json:"value"`
}

func (v ballot) String() string {
	bs, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("Error decoding ballot: %v", err)
	}
	return string(bs)
}

func validate(poll dsmodels.Poll, v ballotValue) string {
	if poll.MinVotesAmount == 0 {
		poll.MinVotesAmount = 1
	}

	if poll.MaxVotesPerOption == 0 {
		poll.MaxVotesPerOption = 1
	}

	allowedOptions := make(map[int]bool, len(poll.OptionIDs))
	for _, o := range poll.OptionIDs {
		allowedOptions[o] = true
	}

	allowedGlobal := map[string]bool{
		"Y": poll.GlobalYes,
		"N": poll.GlobalNo,
		"A": poll.GlobalAbstain,
	}

	var voteIsValid string

	switch poll.Pollmethod {
	case "Y", "N":
		switch v.Type() {
		case ballotValueString:
			// The user answered with Y, N or A (or another invalid string).
			if !allowedGlobal[v.str] {
				return fmt.Sprintf("Global vote %s is not enabled", v.str)
			}
			return voteIsValid

		case ballotValueOptionAmount:
			if poll.MaxVotesAmount == 0 {
				poll.MaxVotesAmount = 1
			}

			var sumAmount int
			for optionID, amount := range v.optionAmount {
				if amount < 0 {
					return fmt.Sprintf("Your vote for option %d has to be >= 0", optionID)
				}

				if amount > poll.MaxVotesPerOption {
					return fmt.Sprintf("Your vote for option %d has to be <= %d", optionID, poll.MaxVotesPerOption)
				}

				if !allowedOptions[optionID] {
					return fmt.Sprintf("Option_id %d does not belong to the poll", optionID)
				}

				sumAmount += amount
			}

			if sumAmount < poll.MinVotesAmount || sumAmount > poll.MaxVotesAmount {
				return fmt.Sprintf("The sum of your answers has to be between %d and %d", poll.MinVotesAmount, poll.MaxVotesAmount)
			}

			return voteIsValid

		default:
			return "Your vote has a wrong format"
		}

	case "YN", "YNA":
		if poll.MaxVotesAmount == 0 {
			poll.MaxVotesAmount = len(poll.OptionIDs)
		}
		if poll.MaxYesVotesAmount == 0 {
			poll.MaxYesVotesAmount = len(poll.OptionIDs)
		}
		switch v.Type() {
		case ballotValueString:
			// The user answered with Y, N or A (or another invalid string).
			if !allowedGlobal[v.str] {
				return fmt.Sprintf("Global vote %s is not enabled", v.str)
			}
			return voteIsValid

		case ballotValueOptionString:
			if len(v.optionYNA) < poll.MinVotesAmount || len(v.optionYNA) > poll.MaxVotesAmount {
				return fmt.Sprintf("You have to select between %d and %d options", poll.MinVotesAmount, poll.MaxVotesAmount)
			}

			for optionID, yna := range v.optionYNA {
				if !allowedOptions[optionID] {
					return fmt.Sprintf("Option_id %d does not belong to the poll", optionID)
				}

				if yna != "Y" && yna != "N" && (yna != "A" || poll.Pollmethod != "YNA") {
					// Valid that given data matches poll method.
					return fmt.Sprintf("Data for option %d does not fit the poll method.", optionID)
				}
			}

			var yes_votes = countYesVotes(v)
			if yes_votes > poll.MaxYesVotesAmount {
				return fmt.Sprintf("The amount of yes votes must not exceed %d", poll.MaxYesVotesAmount)
			}

			return voteIsValid

		default:
			return "Your vote has a wrong format"
		}

	default:
		return "Your vote has a wrong format"
	}
}

// voteData is the data a user sends as his vote.
type ballotValue struct {
	str          string
	optionAmount map[int]int
	optionYNA    map[int]string

	original json.RawMessage
}

func (v ballotValue) MarshalJSON() ([]byte, error) {
	return v.original, nil
}

func (v *ballotValue) UnmarshalJSON(b []byte) error {
	v.original = b

	if err := json.Unmarshal(b, &v.str); err == nil {
		// voteData is a string
		return nil
	}

	if err := json.Unmarshal(b, &v.optionAmount); err == nil {
		// voteData is option_id to amount
		return nil
	}
	v.optionAmount = nil

	if err := json.Unmarshal(b, &v.optionYNA); err == nil {
		// voteData is option_id to string
		return nil
	}

	return fmt.Errorf("unknown vote value: `%s`", b)
}

const (
	ballotValueUnknown = iota
	ballotValueString
	ballotValueOptionAmount
	ballotValueOptionString
)

func (v *ballotValue) Type() int {
	if v.str != "" {
		return ballotValueString
	}

	if v.optionAmount != nil {
		return ballotValueOptionAmount
	}

	if v.optionYNA != nil {
		return ballotValueOptionString
	}

	return ballotValueUnknown
}

// equalElement returns true, if g1 and g2 have at lease one equal element.
func equalElement(g1, g2 []int) bool {
	set := make(map[int]bool, len(g1))
	for _, e := range g1 {
		set[e] = true
	}
	for _, e := range g2 {
		if set[e] {
			return true
		}
	}
	return false
}

// counts the amount of yes votes in a ballot
func countYesVotes(vote ballotValue) int {
	var amount int
	for _, yna := range vote.optionYNA {
		if yna == "Y" {
			amount++
		}
	}
	return amount
}
