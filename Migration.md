# Migration to the new Vote-Service

The old system and the new one differ significantly. It is not possible to map
the old and new fields on a one-to-one basis.

## Previous system

In the current system, each poll has several options. These are linked via
`poll/option_ids` and `poll/global_option_id`. A global option was also created
for motions, even though it should never be used there.

Each option has the values `yes`, `no` and `abstain`. When voting, each user
selects one of these three options for each poll. There are therefore several
`vote` objects per user. These are always stored as yes-no-abstain, even if it
is actually a selection. The `option` objects contain the result. They store
the sum of all vote objects relating to them in the `yes`-`no`-`abstain`
fields. The global options are counted separately. Hence "General approval",
"General rejection" or "General abstain".

The `vote` objects serve solely to display who voted and how. The `user_token`
field helps to group together different votes from a single user if the user ID
has been removed. In non-anonymised polls, `vote/user_id` is the user for whom
the vote is to be counted, and `vote/delegated_user_id` is the user who cast
the vote. If multiple votes are permitted per user, these votes are aggregated
in the vote objects via the vote-weight feature.

The data actually sent by the user is not stored, but is interpreted and
distributed across the vote objects.

## New system

The old `option` collection and the new `poll_option` are two different things
that just happen to have similar names. In the new system, options no longer
contain the result. The new collection `poll_option` is only used in the
elections for storing the possible options.

Such system makes old collections `poll_candidate_list` and `poll_candidate`
redundant. Relations between the poll and the candidates are now defined
directly in the `poll_option` via `meeting_user_id` field.

The votes (collection is now called `poll_ballot`) contain exactly the data
that a user has submitted. There is therefore only one ballot object per poll
and user.

## Permissions

In the old system along with the separate `can_manage_polls` for assinments and
motions there was also a general setting `poll.can manage`. In the new system
this general permission was replaced with 3 distinct permissions depending on
the value in the `content_object_id` field:

* motions: `motion.can_manage_polls`
* assignments: `assignment.can_manage_polls`
* topic: `agenda_item.can_manage_polls`  (new)

New system also introduces a new child permission `can_see_polls` for each of
these content object collections. This permissions allows to see the
unpublished results of the polls.

## Migrating the polls

NOTE: there parts are still in dicsussion and may change:

* Migration of the assignment polls
* Possible onehundred_percent_base values based on the old logic

### General rules

The following information is relevant for all the poll types.

#### poll, poll_config_X and poll_option

Regardless of the type, for each old poll 2 models have to be created: a new
`poll` and a related `poll_config_X`. Additionally for some poll types
`poll_option` models should be generated.

How the data should be migrated depends on whether it is a motion, assignment
or a topic poll. When it comes to assignement polls, there are 3 possibilities
based on content_object_id and whether the global yes or no option is used.

#### poll_ballot objects and poll/result

If old_poll.type is "analog" (corresponds to the new visibility "manually"),
no ballots should be created for the poll. Othervise `poll_ballot` models
should be generated from the old votes.

If poll.state is "created" or "started", then poll/result is empty. Otherwise
it should be generated from the old votes and options.

Additionally to the calculated part, poll/result can include 2 extra values
carried over from the old poll:

* total_ballots (old_poll.votescast): it's the total number of votes that
  includes both valid and invalid votes. Should be always included into the
  result.
* invalid (old_poll.votesinvalid): should be included into the result only if
  new_poll.visibility == "manually"

"total_ballots" and "invalid" should be saved as integers.

Old polls used to have a separate field for the valid votes: `votesvalid`. It
should be omitted in the migration.

### motion

#### poll_config_approval

```
{
  poll_id: new_poll.id,
  allow_abstain: if old.method == "YNA" then "true" else "false",
  onehundred_percent_base: old_poll.onehundred_percent_base. Map (old_poll -> new):
      - YN -> yes_no
      - YNA -> valid
      - valid: (remains unchanged).
      - cast: (remains unchanged).
      - entitled: (remains unchanged).
      - entitled_present: (remains unchanged).
      - disabled: (remains unchanged).
      - Y -> @panic(not allowed for this config type)
      - N -> @panic(not allowed for this config type)
}
```

#### poll

```
{
  title: old.title,
  visibility: old.type. Map (old -> new):
      - analog -> manually
      - named -> open
      - pseudoanonymous -> secret
      - cryptographic -> @panic(immpossible)
  state: if old.state == "published" then "finished" else old.state,
  result: see below,
  published: old.state == "published",
  anonymized: old.is_pseudoanonymized,
  allow_invalid: false,
  allow_vote_split: false,
  live_voting_enabled: old.live_voting_enabled,
  sequential_number: old.sequential_number,
  content_object_id: old.content_object_id,
  voted_ids: old.voted_ids -> replace each user_id with meeting_user_id in poll.meeting_id,
  entitled_group_ids: old.entitled_group_ids,
  meeting_id: old.meeting_id,
}
```


#### poll/result

In the old system, there is one option per poll. There is also a global option,
but this can be ignored. The new `poll/result` essentially corresponds to this
single option. If there is more than one option, then @panic.

Values for "invalid" and "total_ballots" should be stored as integers. The
other values are strings as they are decimal.

Example: `{"yes":"32","no":"20","abstain":"10","invalid":2,"total_ballots":64}`

Calculated from `old_poll.option_ids[0].vote_ids`:

```
{
  yes: option.yes -> string,  (skip if 0)
  no: option.no -> string,  (skip if 0)
  abstain: option.abstain -> string,  (skip if 0)
  invalid: old_poll.votesinvalid if new_poll.visibility == "manually" -> number,  (skip if 0)
  total_ballots: count(option.vote_ids) -> number
}
```

#### poll_ballot

In the old system only one vote should exist per user. The votes can be found
via `old_poll.option_ids[0].vote_ids`. For each old vote a new
`poll_ballot` object sould be created:

```
{
  poll_id: new_poll.id,
  weight: old.weight,
  split: false,
  value: Map (old -> new):
      - Y -> yes
      - N -> no
      - A -> abstain
      - else @panic(impossible value)
  acting_meeting_user_id: meeting_user_id from old.delegated_user_id and poll.meeting,
  represented_meeting_user_id: meeting_user_id from old.user_id and poll.meeting
}
```


### assignment with poll_candidate_list

If in the old system the collection of `old_poll.content_object_id` is
`poll_candidate_list`, the following collections should be created the same way
as for the [Motion poll](#motion):

* poll_config_approval
* poll (including calculation of poll/result)
* poll_ballot

#### poll_option

Additionally, each `poll_candidate` ("old") in
`old_poll.option_ids[0].content_object_id.poll_candidate_ids` should be saved as
a `poll_option`:

```
{
  poll_id: new_poll.id,
  weight: old.weight,
  text: NULL,
  meeting_user_id: meeting_user_id from old.user_id and old.meeting_id
}
```

### topic

`poll` is being created the same way as for [Motion poll](#motion). The other
collections are being migrated differently.

#### poll_config_selection

```
{
  poll_id: new_poll.id,
  max_options_amount: old_poll.max_votes_amount,
  min_options_amount: old_poll.min_votes_amount,
  allow_nota: old_poll.global_option_id exists,
  strike_out: old_poll.pollmethod == N,
  display_chart: pie,
  onehundred_percent_base: no_general
}
```

#### poll_option

A new `poll_option` should be created for each old `option` ("old"). They can
be found via `old_poll.option_ids`.

```
{
  poll_id: new_poll.id,
  weight: old.weight,
  text: old.text,
  meeting_user_id: None
}
```

#### poll/result

For each old option there is one entry in the result dict. The key is the
poll_option.text. The value is being calculated from the old votes.

`global_yes` and `global_no` in polls with `poll_config_selection` are being
calculated separately into the value "nota".

Example: `{"Option 1":"40","Option 2":"23","nota":"6","abstain":"7","invalid":3,"total_ballots":79}`

Calculation:
```
{
  for each option (if not option.used_as_global_option_in_poll_id == old_poll.id):
    poll_option.text: option.yes -> string,  (skip if value is 0)

  abstain: sum(all_options.abstain) -> string,  (skip if 0)
  nota: old_poll.global_option_id -> (option.yes + option.no) -> string,  (skip if 0)
  invalid: old_poll.votesinvalid if new_poll.visibility == "manually" -> number,  (skip if 0)
  total_ballots: old_poll.votescast -> number
}
```

#### poll_ballot

```
{
  poll_id: new_poll.id,
  weight: old.weight,
  split: false,
  value: old.value -> Replace old options ids with corresponding new poll_options ids,
  acting_meeting_user_id: meeting_user_id from old.delegated_user_id and poll.meeting,
  represented_meeting_user_id: meeting_user_id from old.user_id and poll.meeting
}
```

### assignment with global_yes or global_no

Assignment polls with `global_yes` or `global_no` in the new voting system will
function almost like the topic polls: with `poll_config_selection` but with
`meeting_user_id`s instead of the `text` in `poll_option`.

Migrate poll this way if:

* Collection of the old poll's content_object_id is `assignment`
* Old poll has `global_option_id`
* `global_yes` and/or `global_no` for the poll is true

The following collections should be created the same way
as for the [Topic poll](#topic):

* poll
* poll_option
* poll_ballot

Other collection have minor differences.

#### poll_config_selection

`poll_config_selection` for the assignment poll is similar to the topic poll
but it always has `allow_nota: true` and should not have `display_chart`:

```
{
  poll_id: new_poll.id,
  max_options_amount: old_poll.max_votes_amount,
  min_options_amount: old_poll.min_votes_amount,
  allow_nota: true,
  strike_out: old_poll.pollmethod == N,
  display_chart: null,
  onehundred_percent_base: old_poll.onehundred_percent_base. Map (old_poll -> new):
      - YNA -> valid
      - Y -> no_general
      - N -> no_general
      - valid: (remains unchanged).
      - cast: (remains unchanged).
      - entitled: (remains unchanged).
      - entitled_present: (remains unchanged).
      - disabled: (remains unchanged).
      - YN -> @panic(not allowed for this config type)
}
```

#### poll/result

Calculated similarly to the topic polls, but ids of the poll_options created
above are used as the keys instead of the poll_option/text.

Example: `{"1":"40","2":"23","nota":"6","abstain":"7","invalid":3,"total_ballots":79}`

Calculation:
```
{
  for each option (if not option.used_as_global_option_in_poll_id == old_poll.id):
    poll_option.id: option.yes -> string,  (skip if value is 0)

  abstain: sum(all_options.abstain) -> string,  (skip if 0)
  nota: old_poll.global_option_id -> (option.yes + option.no) -> string,  (skip if 0)
  invalid: old_poll.votesinvalid if new_poll.visibility == "manually" -> number,  (skip if 0)
  total_ballots: old_poll.votescast -> number
}
```

### assignment: other cases

#### poll_config_rating_approval

```
{
  poll_id: new_poll.id,
  max_options_amount: old.max_votes_amount,
  min_options_amount: old.min_votes_amount,
  allow_abstain: if old.method == "YNA" then "true" else "false",
  onehundred_percent_base: old.onehundred_percent_base. Map (old -> new):
      - YN -> yes_no
      - YNA -> valid
      - valid: (remains unchanged).
      - cast: (remains unchanged).
      - entitled: (remains unchanged).
      - entitled_present: (remains unchanged).
      - disabled: (remains unchanged).
      - Y -> @panic(not allowed for this config type)
      - N -> @panic(not allowed for this config type)
}
```

#### poll

```
{
  title: old.title,
  visibility: old.type. Map (old -> new):
      - analog -> manually
      - named -> open
      - pseudoanonymous -> secret
      - cryptographic -> @panic(immpossible)
  state: if old.state == "published" then "finished" else old.state,
  result: see below,
  published: old.state == "published",
  anonymized: old.is_pseudoanonymized,
  allow_invalid: false,
  allow_vote_split: false,
  live_voting_enabled: old.live_voting_enabled,
  sequential_number: old.sequential_number,
  content_object_id: old.content_object_id,
  voted_ids: for each user in old.voted_ids -> meeting_user_id in old.meeting_id,
  entitled_group_ids: old.entitled_group_ids,
  meeting_id: old.meeting_id
}
```

#### poll_option

For each old option ("old"), the option.content_object_id value has to be a
`user` collection. Otherwise, @panic. The user_id has to be retrieved from this
field, and the meeting_user_id associated with it is then looked up in the
corresponding meeting.

```
{
  poll_id: new_poll.id,
  weight: old.weight,
  text: NULL,
  meeting_user_id: meeting_user_id from old.user_id and old.meeting_id
}
```

#### poll/result

Poll/result is a dict. There is one entry for each old option. The key is
the poll_option-id created above. The values "yes", "no" and "abstain" are
adopted as objects.

Example: `{"1":{"yes":"5","no":"1"},"2":{"yes":"1","abstain":"6"},"invalid":1,"total_ballots":7}`

Calculation:
```
{
  for each option:
    option.id: {
      yes: option.yes -> string,  (skip if 0)
      no: option.no -> string,  (skip if 0)
      abstain: option.abstain -> string   (skip if 0)
    },

  invalid: old_poll.votesinvalid if new_poll.visibility == "manually" -> number,  (skip if 0)
  total_ballots: old_poll.votescast -> number
}
```

#### poll_ballot

In the old system for each pair user-option a sepatate `vote` was created. A single
`poll_ballot` should be generated for each group of the votes with the same
`user_token`. All of these votes must have the same `weight` - else @panic.

Old votes that can be found via `old_poll.option_ids.vote_ids`.

Calculation (one `poll_ballot` per each `user_token`):

```
{
  poll_id: new_poll.id,
  weight: old.weight (must be same for all),
  split: false,
  value: see below,
  acting_meeting_user_id: meeting_user_id from old.delegated_user_id and poll.meeting,
  represented_meeting_user_id: meeting_user_id from old.user_id and poll.meeting
}
```

Example of `poll_ballot.value`: `{"1":"yes","2":"abstain"}`.

It's a dictionary where each key-value pair represents an old `vote`:

* key: vote.option_id -> should be replaced with the id of the new `poll_option`
  instances created above
* value: transformed vote.value:
      - Y -> yes
      - N -> no
      - A -> abstain
      - else @panic(impossible value)


## Information that will be lost:

* poll/backend: long or short
* poll/description: was not used
* poll/entitled_users_at_stop: only sata about the actual poll results is
  being migrated, but not who was entitled to vote
* For cumulative polls: poll.max_votes_per_option
* Global options are no longer listed separately, but are included in the result.
* poll/valid was previously counted separately. In future, it should be
  calculated by subtracting result.invalid from the total number of votes.

## Changes in old fields by collection

### Group

* group/permissions: poll.can_manage -> agenda_item.can_manage_polls (migration
  is needed)

### Meeting

* Fields were removed. No migration necessary:
  * meeting/motion_poll_default_method
  * meeting/assignment_poll_default_backend
  * meeting/poll_default_backend
  * meeting/poll_candidate_list_ids
  * meeting/poll_candidate_ids
  * meeting/option_ids
  * meeting/vote_ids
  * meeting/assignment_poll_ballot_paper_selection
  * meeting/assignment_poll_ballot_paper_number
  * meeting/motion_poll_ballot_paper_selection
  * meeting/motion_poll_ballot_paper_number
  * meeting/poll_ballot_paper_selection
  * meeting/poll_ballot_paper_number
* Fields that were renamed:
  * meeting/motion_poll_projection_name_order_first -> poll_projection_name_order_first
  * meeting/motion_poll_projection_max_columns -> poll_projection_max_columns
  * meeting/assignment_poll_enable_max_votes_per_option -> poll_enable_max_votes_per_option
  * meeting/default_projector_poll_ids -> default_projector_topic_poll_ids
* Fields that need to be moved to meeting_poll_default:
  * meeting/*_poll_default_group_ids -> meeting_poll_default/group_ids
  * meeting/*_poll_sort_poll_result_by_votes -> meeting_poll_default/sort_result_by_votes
* Field should be renamed and moved to meeting_poll_default, values should be changed similarly to poll/type:
  * meeting/*_poll_default_type -> meeting_poll_default/visibility
* Field should be moved to meeting_poll_default and values should be changed similarly to poll/onehundred_percent_base:
  * meeting/*_poll_default_onehundred_percent_base -> meeting_poll_default/onehundred_percent_base
* For topic polls:
  * meeting_poll_default/display_chart: pie
* meeting/assignment_poll_default_method
  * Y
    *  meeting_poll_default/method -> selection
  * N
    *  meeting_poll_default/method -> selection
    *  meeting_poll_default/strike_out -> true
  * YN
    * meeting_poll_default/method -> rating_approval
  * YNA
    * meeting_poll_default/method -> rating_approval
    * meeting_poll_default/allow_abstain -> true

### Motion

* Field was removed. No migration necessary:
  * motion/option_ids

### Poll

* Fields were removed. No migration necessary:
  * poll/description
  * poll/backend
  * poll/live_votes
  * poll/entitled_users_at_stop
  * poll/votesvalid
* poll/is_pseudoanonymized -> poll/anonymized.
* poll/type -> poll/visibility and the values have changed:
  * analog -> manually
  * named -> open
  * pseudoanonymous -> secret
  * cryptographic There should be no case. If so, "secret" can be used.
* poll/onehundred_percent_base -> poll_config_X/onehundred_percent_base and
  some values have changed:
  * YN -> yes_no
  * YNA -> valid
  * Y -> no_general
  * N -> no_general (+ poll_config_selection/strike_out: true)
  * valid: no changes.
  * cast: no changes.
  * entitled: no changes.
  * entitled_present: no changes.
  * disabled: no changes.
* poll/state: The value `published` was removed. Published polls have to be migrated:
  * poll/state -> `finished`.
  * poll/published -> `true`.
* Fields were moved to poll_config_X collections, data has to be migrated.
  Refer to [Migrating the polls](#migrating-the-polls) for more details:
  * poll/pollmethod
  * poll/min_votes_amount
  * poll/max_votes_amount
  * poll/max_votes_per_option
  * poll/option_ids
* Fields were removed, `poll/result` has to be generated from them. Refer to
  [Migrating the polls](#migrating-the-polls) for more details:
  * poll/global_yes
  * poll/global_no
  * poll/global_abstain
  * poll/global_option_id
  * poll/votescast
  * poll/votesinvalid

### Poll_candidate_list

* The `poll_candidate_list` collection was removed. No migration needed.

### Poll_candidate, option -> poll_option

* Parts `poll_candidate` and `option` were absorbed by the new collection `poll_option`:
  * poll_option/weight:
    * option/weight
    * poll_candidate/weight
  * poll_option/poll_id:
    * option/poll_id
    * poll_candidate_list_id.option_id.poll_id
  * poll_option/text:
    * option/text
    * None
  * poll_option/meeting_user_id:
    * if collection of content_object_id == "user" -> meeting_user from
      option/content_object_id and option/meeting_id
    * meeting_user from poll_candidate/user_id and
      poll_candidate.option_id.meeting_id

### Option -> other collections

* The following fields were removed but have to be used in the migration for
  generating values for generating poll/result:
  * yes, no and abstain
  * vote_ids

### Projection

* projection/content was removed. No migration necessary.

### Vote

* The `vote` collection was renamed into `poll_ballot`.
* Field was removed. No migration necessary:
  * vote/meeting_id
* vote/user_token: is used to merge old votes into new ballots
* vote/user_id -> poll_ballot/represented_meeting_user_id (needs to be
  generated from user_id and meeting_id).
* vote/delegated_user_id -> poll_ballot/acting_meeting_user_id (needs to be
  generated from user_id and meeting_id).
* vote/option_id: replaced with the direct relation to the poll. Needs
  migration: vote.option_id.poll_id -> poll_ballot.poll_id.

### User

* Direct relation between `user` and `poll`, `option` and `vote` was replaced
with relation through the `meeting_user`:
  * user/poll_voted_ids -> meeting_user/poll_voted_ids
  * user/option_ids + user/poll_candidate_ids -> meeting_user/poll_option_ids
  * user/vote_ids -> meeting_user/represented_ballot_ids
  * user/delegated_vote_ids -> meeting_user/acting_ballot_ids
