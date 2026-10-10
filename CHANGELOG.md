# Changelog

The format is [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## Unreleased

### Added

- `Kit.Control(agent)`, a kit's agent as `agentturn.Control` (#87). It
  does around each run what a kit's agent needs and a bare agent does
  not: every run, queued input and head move carries the kit's recorder
  and session (`Control.RunContext`); `Resume` puts the answers through
  the engine's `Release` first, so a call the engine only held beside
  an asked one is released with the batch, and refuses an answer
  `Resume` would refuse before releasing anything; `Queue` writes the
  queued entry before it returns; and, when the kit has no elicitor of
  its own, a question asked while a call runs is an
  `agentturn.Question` event, recorded, answered with `Reply`, whose
  note reaches the model. Outside the contract: `Permissions(end)`, the
  calls a run left for someone with the policy's question, not the
  ones the engine only holds; and `ContinueFrom(ctx, entryID)`, the
  head move with the skill grants moved along, undone on failure. It
  is the composition agentconsole's kitbackend and dax each wrote; the
  manual lists its calls.

### Changed

- Requires agentturn and agentturn/session v0.0.19 (up from v0.0.16),
  and agenttool and agenttool/mcpclient v0.0.22 (up from v0.0.15).

## v0.0.9 - 2026-10-09

### Changed

- Requires agentpolicy v0.0.12. No API of this module changes, but
  agentpolicy's `Subjects` now takes the decision's context,
  `func(ctx context.Context, args json.RawMessage) ([]Subject, error)`,
  so a product's splitter in the `agentpolicy.ToolMatcher`s it passes to
  `WithPolicy` changes from `func(args)` to `func(_ context.Context, args)`.
  The kit's refusal of a call only an ended grant allowed
  (`WithSkillGrantScope`) passes the context of the call's decision to
  the splitter too, so a splitter that asks a remote executor is
  cancelled with the decision. The release also makes a sibling whose
  subjects could not be evaluated hold the batch call beside it rather
  than read as blocked, and not keep the failed reading for the batch.

## v0.0.8 - 2026-10-09

### Added

- `WithAgentsMD` can read the AGENTS.md chain through an `fs.FS`, such
  as a container's or a remote workspace's, by setting
  `agentsmd.Options.FS`: `path` and `Root` are then names in that file
  system, `"."` its root, and `Extra` stays OS paths. The option already
  passed `agentsmd.Options` verbatim, so there is no new option; the
  manual path is `agentsmd.Chain` with the same options. (#84)

### Changed

- Requires agentsmd v0.0.3, for `agentsmd.Options.FS`. No API of this
  module changes. (#84)

## v0.0.7 - 2026-10-02

The round 8 findings (#66 to #76).

### Security

- Under `WithSkillGrantScope` a grant is bound to the user message in
  force when the read made it, the count of user messages in the turn's
  transcript and a digest of the last one, and ends on any turn whose
  transcript holds a later one, wherever it sits. The scope read only
  the transcript's tail, so a message another kit's agent received in a
  handoff, whose hand-back opened the holder's turn on the transfer's
  output, ended nothing, and the holder ran under the first message's
  grant. A steer delivered before an output is one more message too. A
  grant a restart makes again is bound to the message the transcript the
  agent is seeded with ends under, `session.Transcript`, so the restart's
  first turn keeps it, after a fold past the last prompt too, where the
  fold's summary stands in for it: a compaction ends nothing by itself,
  in the run or across a restart. (#66)
- A memory write a policy held and a person approved ran in the Resume
  past #53's refusal when the budget had dropped the block of the run
  that composed it: the Resume has no render, so the drop was not seen.
  The kit's BeforeToolCall hook now keeps the render, manifest and drop,
  for every memory write it decides, `memory_patch` and `memory_forget`
  as well as `memory_save`, and the refusal reads it first; after a
  restart, a manifest folded at the call that shows no entry and lists
  one the block held among the omitted is refused on that. When the
  budget drops the block the kit writes `agentmemory.OmitBlock`,
  agentmemory v0.0.10's reason for an entry left out with the whole
  block, on every omitted entry, those a bounded render had already left
  out under the budget as well, and `droppedRender` tests that reason in
  place of the empty one it read. `Kit.Omitted()` reports those entries
  with the reason `block`, where it reported `budget`. A session a kit
  before this release recorded wrote the drop with no reason, and a
  held write in it is no longer refused after a restart on the manifest
  alone. (#69)

### Fixed

- A restart that passes over a skill read because the skill changed
  since, its served instructions, which agentskill ends with the skill's
  file list and each file's size, or its frontmatter, reports it with
  `Replayed` set, `FrontmatterChanged` saying which digest moved and
  `Err` wrapping the new `ErrSkillGrantChanged`, and the session records
  a verdict saying so. Both cases were a bare `continue`, so a grant in
  force when the process stopped vanished with nothing to say why: an
  `allowed-tools` narrowed, a file in the skill grown by a byte, or an
  edit `Kit.ReloadSkills` picked up with no read after. `WithSkillGrants`
  and `ReloadSkills` say what the digest covers. (#72)
- A read the skill tool refuses with `agentskill.ErrSkillChanged`, the
  skill file gone, renamed or no longer parsing since discovery, is
  reported to `WithSkillGrantReport` with `Err` wrapping that error, by
  the name the call gave. The model was told to discover the skills
  again, which it cannot, and the product, which can through
  `Kit.ReloadSkills`, was told nothing. (#73)
- A front that names each conversation with `session.ContextWithSessionID`
  and no `ContextWithRecorder` had the kit's observer write every grant
  verdict nowhere, so a restart's `RegrantSkills` found the read with no
  recorded rules, granted nothing and said nothing. A live read of a
  skill with rules that the session records no verdict for, granted or
  refused, is reported with `Err` wrapping the new
  `ErrSkillGrantUnrecorded`, and `Kit.RegrantSkills` returns the new
  `ErrSkillGrantRecorder` before binding or granting anything when the
  kit records its engine's verdicts and no recorder, on `ctx` or the
  kit's own, writes the session. (#74)
- Under `WithSkillGrantScope` a call that only a grant the scope ended
  would have allowed is refused with a reason the model reads naming the
  skill and the tool that reads it again, until the skill is read again.
  The engine deferred it to the ask rule and what the model read was a
  reviewer's refusal with no word of the skill; the turn-start note was
  several items up, and a model refused acts on the refusal in front of
  it. The refusal is the first hook the kit folds into the engine it
  builds under `WithPolicy`, ahead of the product's, so the engine's
  fold takes the Block: a sibling in the same batch is decided beside it
  and runs when allowed, where a Block chained ahead of the engine left
  the siblings held for a question nobody was asked, and the engine
  records the refusal as the call's verdict. The kit refuses when
  agentpolicy v0.0.11's `Engine.Would`, the engine's own side-effect-free
  decision for the call without the ended grants, is to ask; a call the
  engine allows or denies is left to it. `Would` reads the policy, the
  grants in force, the tool's confinement, every agentpolicy option and
  the hooks folded into the engine, so the refusal no longer gives up
  under an option of the product's, `WithAliases` among them, and no
  longer refuses a call a specifier or a carve-out leaves allowed. The
  product's hooks are called once more for the question, and with hooks
  of the product's, or agentpolicy options that may add some, the kit
  refuses only when an ask rule is behind the verdict, since a hook's
  question would be asked whatever a grant did. There is no
  refusal under `WithEngine`. An ended grant is kept by the source it was made under,
  and `Kit.ReloadSkills` forgets one whose skill the catalogue no longer
  lists, so a deleted skill does not refuse its tool for the rest of the
  conversation. (#76)
- A kit restarted into a handoff wrote its whole memory manifest on its
  first hand-back, since its own last manifest, the base of the delta,
  lives in the kit object. A kit with no last manifest in memory now
  writes a delta on whichever manifest the session's path holds in force
  gives the smallest record. The record is agentmemory v0.0.10's
  `ManifestFold.Record`, the smallest over the manifests the fold holds,
  and the kit keeps no copy of those manifests of its own. (#68)
- A render of a run the kit's session does not record, an agent built
  from the kit and attached to nothing or to another recorder with no
  recorder on its context, was written into that session with no run
  around it, where `foldAtCall` read it as the render of whichever run
  was open, and a save held across a restart was based on it. Such a
  render is not recorded; the run's own saves are based on it in the
  process that rendered it, and a held save of its own after a restart
  is refused. The `Kit` and `WithSession` docs say every agent built
  from a kit with a session is attached to it or prompted under
  `ContextWithRecorder`. (#70)
- `AddMCP`, `RemoveMCP` and `Close` no longer hold the kit's MCP lock
  across a dial or a close, so `Kit.Tools()`, removing another server
  and quitting answer while a sign-in waits in a browser or a server
  takes its grace to close. `AddMCP` takes its number under the lock and
  dials off it; `RemoveMCP` takes the server out of the lists and closes
  it after; `Close` takes every server under the lock and closes them
  together, so quitting costs one grace and not one per server, and
  cancels a dial `AddMCP` has in flight, whose `AddMCP` returns an
  error. `RemoveMCP`'s and `Close`'s docs say the close ends every call
  in flight to the server with `mcpclient.ErrClosed` and returns within
  a few seconds, as agenttool v0.0.15's does, with or without
  `WithToolElicitor`; they no longer say it waits for the calls. (#71)

### Added

- `Kit.GrantScope(ctx)` names the grant scope the kit decides a run
  under, the conversation's session ID, and `SkillGrant.Scope` says whose
  read a report is. `Kit.LookupToolFor(ctx, name)` reads the union the
  provider last returned to the run the context belongs to; the engine
  the kit builds is given it through `agentpolicy.WithToolsFor`, so two
  runs off one kit whose tool lists differ each read their own, where
  `Kit.LookupTool` read whichever list was offered last. A run the kit
  has no list for reads the kit's own, the one offered outside any run,
  never another run's, and `LookupTool` now answers that list too, for a
  context with no run. (#43 of agentpolicy)

### Changed

- Requires agentturn and agentturn/session v0.0.16, agentsession
  v0.0.20, agenttool and mcpclient v0.0.15, openresponses v0.0.14,
  agentpolicy v0.0.11, agentskill v0.0.11 and agentmemory v0.0.10,
  the releases that carry the sibling APIs the entries here use.
  Under agentturn v0.0.16 a session is written in agentsession format
  0.11, which names an instruction part or an omitted run the path
  already holds rather than repeating it: in a handoff between two
  kits the config entry at each hand-back after the first fell from
  about 80 KB to about 13 KB.

- Skill grants are per conversation. A grant is made under the grant
  scope of the conversation that read the skill,
  `agentpolicy.ContextWithGrantScope` keyed by its session ID, and the
  engine decides only that conversation's calls by it, so one kit serves
  any number of conversations: a skill read in one grants nothing in
  another, and a read in a second is granted there. The kit put the
  engine's one grant set on the first conversation it served and, at the
  first call it decided in a second, revoked every grant and granted
  nothing after; that mechanism is gone, with `ErrSkillGrantConversation`,
  its guard, its verdicts and its report with no skill (#44, #59). A
  front that matched that error matches nothing now, and a front that
  built a kit for each conversation to get this can build one. The
  scope is on the context the kit's engine hook and tool provider call
  the engine with, so a product that calls the engine for a
  conversation, `Engine.Answers` on a `Resume`, `Engine.GrantsFor`,
  `Engine.RevokeScope`, puts `Kit.GrantScope(ctx)` on its context. A
  scope a front puts on the context with `agentpolicy.ContextWithGrantScope`
  is the conversation's scope; a run with no session and no recorder has
  one scope of the kit's own, not the unscoped one, which would decide
  every conversation's calls. Under `WithSkillGrants` a child agent
  `WithChildAgent` offers runs under a scope of its own, under its
  conversation's: its parent's skills grant it nothing, a skill it reads
  is its alone, and its conversation's next message ends the grants of
  the children run under it, also a message the transcript's tail test
  does not see, since a child's grants are bound to its parent's turn's
  user-message mark. The children of a scope are recorded when each
  child's run starts, so a grandchild that reads a skill under a child
  that read none is reached too, and ended exactly, never matched by
  name, so scopes a front
  names "user/4" and "user/42" are two conversations. Without skill
  grants the kit puts no scope on a child's context, and a scope the
  product put on the host's, with grants of its own under it, is the
  child's. `Kit.RevokeSkillGrants(ctx)` ends one conversation's grants
  and its children's, and is what a front calls when a conversation is
  over, since nothing else tells the kit; the kit then forgets what it
  kept of them. The engine consults every set not so ended for every
  decision, so a server whose conversations end for good calls it, or
  runs under `WithSkillGrantScope`. A call with a bare context revokes
  the kit's no-session scope and no conversation's. The scope state is guarded by one lock across
  conversations, held across the engine's `GrantSet` and `Revoke` and the
  journal writes they trigger, so a slow session store in one conversation
  delays a skill read in another; a lock per scope would remove that.
  Each revocation is still a verdict per source in the conversation's
  session, `revoked the rules granted by <source>`, so a restart replays
  it as before, and `RegrantSkills` and `New` grant a session's reads
  again under its scope, or under the scope their context carries when
  the front names its conversations' scopes with
  `agentpolicy.ContextWithGrantScope`, since the session does not record
  it; a replay also reads `Engine.RevokeScope`'s
  verdict as ending every grant of the conversation. `RegrantSkills` no
  longer refuses a second session.
- Under `WithSkillGrantScope` a call that only an ended grant would have
  allowed is now refused by the kit with a reason naming the skill, where
  the engine held it for the ask rule and a reviewer answered (#76). A
  front that showed such a call as pending sees a run that ends `done`
  with the refusal in its output instead, and the model reads the
  skill again before it calls; the other calls of its batch are decided
  as they would have been without it, so an allowed sibling runs rather
  than waiting on the refused call. The kit refuses when the engine's
  `Would` says it would ask, and under `WithEngine` nothing is refused.
- `Kit.RegrantSkills` returns `ErrSkillGrantRecorder` instead of nil
  when the kit records its engine's verdicts and no recorder, on `ctx`
  or the kit's own, writes the session it was given; it granted
  nothing in that case before, and now says so (#74). A front that
  called it on a bare context passes the conversation's recorder with
  `ContextWithRecorder`. Under `WithEngine` nothing changes.
- `WithCompaction` sends the summary request under the agent's
  reasoning only when the agent's model writes the summary. Under
  `WithCompactionModel` or `WithCompactor` it sets none: the product
  chose that model knowing it, and a configuration meant for the agent's
  may be refused by, or wasted on, another. For an agent at effort low
  or above the summary is still asked at that effort, and the doc says a
  thinking model spends part of the summary's cap reasoning, so a
  product whose agent thinks passes a `compact.WithRequest` at effort
  none, or the lowest its provider accepts; the kit does not pick a
  lower effort itself because it does not know that floor, and several
  reasoning models refuse none. (#75)

### Documentation

- The by-hand `AddMCP` block in `docs/manual.md` checked `Connect`'s
  error after it used the server, so a server that failed to start
  panicked; it checks first, and every elided value in the manual's Go
  blocks is spelled out. `TestManualGoBlocksParse` parses every Go block
  of the manual and the README, so a block that stops parsing fails the
  build. (#67)
- `Kit.Engine` says a front calls `Engine.Forget` for a run that ended
  with no pending call, since the kit has no run-end hook (agentturn
  #208).

## v0.0.6 - 2026-10-01

The round 7 findings (#52 to #64), and the siblings' round 7 releases
taken up.

### Security

- A front that names each conversation with
  `session.ContextWithSessionID` and no `ContextWithRecorder`, as
  agentturn/front/a2a's `WithRecorderFor` example does, is several
  conversations to the kit's skill grants, not one. They took every run
  for the kit's own conversation, so one conversation's skill read
  granted every other's calls, and #44's leak was open there. A run's
  conversation is now the recorder on its context, else the session the
  context names, unless that is the kit's own or a child's of a run the
  kit served, else the kit's own session. Under `WithRecorder` every
  session of the recorder is one conversation, as before. (#58)
- A restart never grants a skill whose frontmatter changed since the
  read. The replay compared only the digest of the instructions served,
  which does not cover `allowed-tools`, so a rewrite of `allowed-tools`
  that left the body alone was granted again. A read now records
  agentskill's `FrontmatterSHA256`, and the replay passes over one whose
  skill's frontmatter is no longer that. The default source names the
  digest as its `Hash`, so every verdict about a grant says what it was
  built from, and `WithSkillGrants` says how a product binds a person's
  approval to it. (#63)

### Fixed

- Under `agentpolicy.WithAliases` a restart grants again what a skill's
  read was granted. The engine records a rule as it granted it, after
  the alias expanded it, and the replay looked for the skill's own
  spelling, found nothing, and granted nothing, silently: a task
  approved after a restart had every call its skill allowed asked
  about. When the read recorded the frontmatter's digest and the skill
  still has it, the rules are granted as the engine recorded them; a
  replay that finds none of a read's rules reports it, and one the
  engine takes wider than recorded is revoked. (#57)
- `WithCompaction` sends the summary request under the agent's
  reasoning, `WithReasoning`'s or `WithRequest`'s, through a
  `compact.WithRequest` of its own ahead of the product's options. A
  thinking model left at its server's default reasoned through the
  summary's cap and answered no text. (#60)
- A kit that resumes a session seeds the agent with all of agentturn
  v0.0.15's resume state: `Kit.AgentOptions` is `session.AgentOptions`
  read with the recorder's `ReadOptions`, so the model each reasoning
  item came from is known after a restart and a fork's prefix calls are
  read through its origin; `Kit.Transcript` is `session.Transcript`,
  with the items the filter kept from the model put back; and under
  `WithCompaction` the fold is given `session.CompactOptions`, so it
  backs off from a fold that failed before the restart rather than
  asking for the same summary again. (#64)
- A memory block the instruction budget drops takes `memory_save`,
  `memory_patch` and `memory_forget` out of that request, and the run
  refuses them, so a model shown no block and no word on the tools no
  longer saves over entries it never saw. `memory_search` stays. (#53)
- A held `memory_save` folded from the path after a restart is refused
  when another run's render may be the last before the call: another
  run open at the call, or one started between the call's run and the
  call. Two runs sharing a session based the save on the other run's
  render, and a write made in between was lost with nothing reported.
  (#56)
- Each turn of a kit handed back to reads only the manifest records
  written since the turn before. The kit folded every record from the
  session's root whenever the other kit's was last, which in a handoff
  is every turn: 92 ms at the 100th hand-back, growing. The fold is now
  `agentmemory.ManifestFold`, kept per session. (#55)
- When another conversation ends a kit's skill grants, the owner's
  session records a verdict naming that conversation ahead of the
  revocations, `WithSkillGrantReport` is told once, with no `Skill` and
  an `Err` wrapping `ErrSkillGrantConversation`, and the owner's own
  read after it names the conversation that ended them, where it named
  the owner twice. `Kit.RegrantSkills` writes a later revocation to the
  kit's own recorder when the context carries none and that recorder
  writes the session. (#59)
- Under `WithSkillGrantScope`, a user message that ends a grant in force
  gives the turn a developer note naming the skills and their rules and
  saying that a read restores them. A model with the skill's text above
  went straight to the tool and was refused as an ordinary ask. (#61)

### Added

- `Kit.ReloadSkills` discovers the skills again over the sources `New`
  was given and puts the new catalogue behind the skill tool, the skill
  grants and the skills part, for an agent that writes a skill and uses
  it in the same conversation. A config returned before keeps its
  instructions until its agent is given the new one, unless the kit
  re-renders them each request. (#62)
- `SkillGrant.FrontmatterChanged`: the skill file's frontmatter changed
  since discovery, so the grant is the rules as loaded. (#63)

### Changed

- The memory manifest a kit handed back to records is a delta on its
  own last manifest when that is smaller than one on the manifest in
  force, which after a handoff is the other kit's. agentmemory v0.0.9's
  `ManifestFold` resolves such a delta and `ApplyManifestRecord` refuses
  it, so a reader of a session a handoff wrote folds with
  `ManifestFold`; agentkit v0.0.5 and earlier read such a record as one
  that does not fold, and write their next manifest whole. (#55)
- Requires agentturn and agentturn/session v0.0.15, agentsession
  v0.0.19, agenttool and mcpclient v0.0.14, agentpolicy v0.0.10,
  agentskill v0.0.10 and agentmemory v0.0.9. agentskill v0.0.10 serves
  a skill read by the path `SKILL.md` as its instructions, so such a
  read now grants its `allowed-tools`, and reads the skill file at each
  call.

### Documentation

- `WithFoldObserver` and the README say a failed fold is observed too,
  with `Fold.Err` set and the transcript sent whole, and that a front
  tells the two apart by `Err`. (#52)
- `docs/manual.md` says its blocks need agentturn v0.0.15 or later, and
  what an earlier one does to the fold block; the siblings' releases
  now require it, so a product on them selects it. (#54)
- `docs/ordering.md` no longer says the kit's `WithOnFold` replaces a
  product's, which it has not since v0.0.4.

## v0.0.5 - 2026-10-01

### Added

- `Kit.AddMCP` and `Kit.AddMCPTransport` connect an MCP server after
  `New`, and `Kit.RemoveMCP` closes one. A server added mid-session is
  dialed as `New` dials one, with `WithMCPStderr` and the elicitation
  `WithToolElicitor` asks for, labelled `mcp:#<n> <what>` after the
  servers before it, and offered from the next turn, after `New`'s
  servers and ahead of `WithToolProvider`'s. A name it shares with a
  tool offered now is an error and the server is closed. `Kit.Tools()`
  lists its tools until it is removed, and `Close` closes it. By hand it
  is a provider over a locked list of remotes, which `docs/manual.md`
  writes out. (#50)

### Changed

- Requires agenttool and agenttool/mcpclient v0.0.13. No API of this
  module changes with them.

### Documentation

- `WithMCPTransport` shows how to keep an OAuth-protected server's
  grant with mcpclient's new `StoreTokens`, keyed by the endpoint and
  the user a kit is built for, so a restart does not mean consenting
  again. `WithMCP`, the README and `docs/composition.md` point a front
  that builds a kit per user at it.

## v0.0.4 - 2026-10-01

The round 6 findings (#37 to #47), and the siblings' round 6 releases
taken up.

### Security

- A skill grant no longer reaches another conversation. A grant is a
  rule set on the kit's one engine, which applies it to every decision,
  so a kit served to many conversations under `ContextWithRecorder`
  let every conversation run what one conversation's skill read
  allowed, and a user message in any of them ended the grant under
  `WithSkillGrantScope`. A kit's grants now belong to one conversation:
  the session it opened or was given, or the conversation of its first
  grant. The first call the kit decides in any other conversation
  revokes every grant before it is decided, recorded in the owner's
  session, and from then on the kit grants nothing; a read elsewhere is
  reported with the new `ErrSkillGrantConversation`. A front that grants
  skills to many conversations gives each its own kit. agentpolicy has
  no way to decide one conversation's call without another's grants, so
  this fails closed rather than scoping them. The new
  `Kit.RegrantSkills` restores a session's grants for a front that
  resumes the conversation itself. (#44)
- A restart never widens a skill grant. The replay granted the skill's
  `allowed-tools` as they stood at `New`, so a skill widened between a
  held call and its approval was granted rules the model never read,
  and a read the engine withheld as untrusted was granted once the
  source function trusted it. The replay now grants only the rules the
  path's `granted <rule> by <source>` verdicts say the read was granted
  and that the skill still allows, and passes over a read whose digest
  is not what the catalogue's tool serves for the name now. Under
  `WithEngine`, where the kit records no verdict, it is the catalogue's
  rules as before. (#45)
- A held `memory_save` keeps its base whoever holds it. The base was
  kept by the observer of the engine `WithPolicy` builds, so a save held
  by a `WithEngine` engine or a product `BeforeToolCall` hook, held
  across a restart under `ContextWithRecorder`, or made after 1,024
  other runs rendered, was based on the kit's last render and could
  discard a write made after its model read the block, with nothing
  reported. A `BeforeToolCall` hook of the kit's, ahead of the engine
  and the product's hooks, now keeps it; a call with no kept base is
  based on the manifest in force on its session's path at the call; and
  a call in a run with neither is refused, telling the model to search
  for the entry and save again. Only a call outside any run is based on
  `Kit.MemoryManifest`. `New` no longer folds the pending saves itself,
  since the path is folded when the save runs. (#41)

### Changed

- **Breaking**: requires agentturn and agentturn/session v0.0.14,
  agentsession v0.0.18, agenttool and agenttool/mcpclient v0.0.12,
  agentpolicy v0.0.9, agentskill v0.0.9 and agentmemory v0.0.8.
  Sessions are written as `agentsession/0.10`, and a 0.9 file the
  recorder appends to is raised to 0.10, after which agentsession
  v0.0.12 to v0.0.17 refuse it: upgrade every reader of a store first.
  A `cas` store is migrated to per-session logs on its first writing
  open, after which agentsession v0.0.15 and earlier cannot read it.
- A `compact.WithOnFold` in `WithCompaction`'s or `WithCompactor`'s
  options runs again, after the kit records the fold. agentturn
  v0.0.14's `WithOnFold` adds a callback rather than replacing the one
  before it, so the kit registers its own ahead of the product's, and
  v0.0.3's breaking change, which dropped the product's with no error,
  is undone. (#38)
- The memory manifest is a delta wherever the kit records it. The kit
  read the path only of the session `New` opened, so under a recorder on
  the run's context, the first turn after a restart or a `Rebase`, and a
  handoff, every write was the whole manifest. The kit now reads the
  path of whichever session the record lands in through the recorder's
  store, whose `Open` hands back the live session it holds, and writes
  `RecordSince` the manifest in force there, or nothing when the render
  says what the path already says. Whole is left for a session with
  nothing in force. (#42)
- A kit handed back to after a handoff records its render again even
  when it did not move. A kit under `WithRecorder` took "I wrote to this
  session before" for "my record is last", so the path's last manifest
  was the other agent's memory. It now sees the other kit's record on
  the path, and, for a store that cannot open the session, a record of
  the last manifest any kit in the process wrote there. (#43)
- A restart's replay of skill grants is silent. It recorded every live
  grant's verdicts again and called `WithSkillGrantReport` as if the
  skill were read, so a daemon restarted every few minutes grew its
  session by a verdict per rule per restart. The replay now records
  nothing, the verdicts it repeats being on the path, and reports with
  the new `SkillGrant.Replayed` set. (#46)
- A memory budget too small for the block drops the block. agentmemory
  v0.0.8 refuses such a bound with `ErrBudget`, which `New` would have
  returned as an error.

### Documentation

- `docs/manual.md`'s `guardParts` sets each per-part verdict's
  `Subject` to `instructions/<part id>`, as the kit does, and
  `TestTheManualPathKeepsARedactedSecretOutOfTheRecord` compares the
  two paths' guard verdicts. Written from the manual, a product
  recorded them with no subject. (#39)
- The README's first example takes its skills from
  `WithOptionalSkills(repoRoot/.dax/skills, home/.dax/skills)`, since
  `WithSkills(".dax/skills")` failed `New` in every repository without
  one, relative to the process's directory. The serving paragraph no
  longer says a peer needs no option, and names
  `fronta2a.WithRecorderFor` and `RecordEach`. (#40)
- `WithMCP` and `WithMCPTransport` say that a server's connection is
  dialed once and its identity is the kit's, so an OAuth-authorized
  server acts as whoever authorized it for every conversation, and that
  a front serving several users gives each a kit. Connecting per user
  needs a token store mcpclient does not have yet. (#47)
- `ContextWithRecorder`'s example releases the conversation's session
  when the task ends, and says that skill grants and MCP connections
  stay the kit's.

### Fixed

- `examples/a2a`'s `RecordEach` releases a conversation's session when
  the last task running in it ends, and the next message resumes it. A
  server held every session it had served, a lock and the whole session
  each, so no other process could open one. Its stale "the root is not
  released" comment is gone. (#37)

## v0.0.3 - 2026-09-29

The round 5 findings (#28 to #35), and the siblings' round 5 releases
taken up.

### Security

- A skill's `allowed-tools` grant survives a restart. A grant lives in
  the engine, so a kit rebuilt over `WithResumedSession` between a held
  call and its approval lost it, and the approval's `Resume` held every
  later call the skill had been granted. Under `WithSkillGrants`, `New`
  now replays the session's path forward. An `agentskill:read` record
  of a skill's own instructions grants its source, and a recorded
  revocation ends it: the engine's when it removed a rule, and one the
  kit now records for a set that held none, an untrusted skill's, since
  the engine reports nothing there and a skill trusted by the next
  start would otherwise be granted again. Under `WithSkillGrantScope`
  the replay also starts after the path's last user message. What is
  left is granted under the catalogue's rules as they stand now. A
  record whose name another skill now holds is passed over. Each restart
  records the grants' verdicts again. (#35)
- Two runs off one kit each base `memory_save` on their own render.
  Every run shared the kit's last render, so a save in run A after run
  B rendered was based on B's view, which already held a write a third
  session made in between. The save discarded that write,
  `agentmemory.LostUpdates` reported nothing and the model was not told.
  The kit now keeps each run's last render under
  `agentturn.RunIDFromContext` and builds the save over it. A save held
  for approval runs in the `Resume`, which has not rendered, so the
  kit also keeps a save's render when the engine it built decides the
  call, keyed by the session on the context and the call ID, and after
  a restart `New` folds the path's manifest records up to each pending
  save. Anything else, a save held by a `WithEngine` engine or a
  product hook, falls back to the last render. Both maps keep their
  newest 1,024 keys.
  `agentmemory.WithRenderedContext`, the seam the issue proposed, does
  not exist yet; this is the same thing from exported calls. (#34)
- A kit served per conversation files what it records in that
  conversation's session. The new `ContextWithRecorder` and
  `RecorderFromContext` put a recorder on a run's context, and the
  engine's and guards' verdicts, the memory manifest, a fold, a tool's
  question and a child agent's run go to it rather than to the kit's
  own recorder, where they sat at the root beside no run. The engine
  observer, the guard chain's observer and the fold callback are now
  bound with or without a session. `examples/a2a` gains `RecordEach`,
  a `WithRecorderFor` that opens a session per a2a context ID and uses
  it. (#31)

### Changed

- **Breaking**: requires agenttool and agenttool/mcpclient v0.0.11,
  agentturn and agentturn/session v0.0.12, agentsession v0.0.15,
  agentskill v0.0.8, agentmemory v0.0.7 and agentpolicy v0.0.8.
  Sessions are written as `agentsession/0.9`, which v0.0.11 of
  agentsession refuses. agentturn v0.0.12's `Resume` now puts an
  approval of a call that never started to `BeforeToolCall`, so the
  engine decides it, and agentpolicy v0.0.8 decides again a call it
  would run again. `examples/a2a` requires front/a2a and tools/a2a
  v0.0.12.
- **Breaking**: `WithCompactor(c, budget, opts...)` takes a budget, as
  `WithCompaction` does, and sends the agent's model name ahead of
  `opts`. It folded at `compact.DefaultBudget` and sent every
  `CompactRequest` with an empty `model`, which a provider that
  requires it refuses. `New` refuses `WithCompactor` beside
  `WithCompaction` or `WithCompactionModel`. Add the budget you gave
  `WithCompaction`. (#28)
- **Breaking**: `PartsFrom` takes the kits' variables, `PartsFrom(&triage,
  &billing)`, and reads each one on every request. The recorder is
  opened before the kits exist, so `PartsFrom(triage, billing)` passed
  as the parts function held two nils and recorded neither agent's
  parts, and the handoff still verified. (#30)
- **Breaking**: a `compact.WithOnFold` in `WithCompaction`'s or
  `WithCompactor`'s options is always replaced, since a run may bring
  its recorder on its context after `New`. `WithFoldObserver` is how a
  product hears of a fold.
- A guard that refuses an instructions part names it,
  `instructions/<id>: ...`, in `New`'s error and a turn's alike, where
  `New`'s said only that a guard refused the instructions.
  `errors.Is(err, agentturn.ErrGuard)` still holds. (#33)
- The memory manifest is recorded as a delta, agentmemory v0.0.7's
  `Manifest.RecordSince`, when the kit can see that it will fold: in
  the session `New` opened, when the last manifest record on its path
  is the kit's own. A write to one entry of 600 then costs that entry,
  not 84 KB. Everywhere else it is whole, since a delta on a manifest
  not in force on the path is one `ApplyManifestRecord` refuses: two
  kits of a handoff, a `Rebase` or `/clear`, a child's session, a
  recorder on the context. A path a `Rebase` moved off the kit's last
  record now gets the render again even when it did not move, unless
  its last record is whole and says the same, which the kit adopts, so
  two kits of a handoff over one memory do not rewrite it at every
  turn.
- A shadowed skill's omission names the skill that holds its name from
  agentskill v0.0.8's `Skill.ShadowedBy`. One shadowed under a
  qualified name was blamed on the bare name's winner.

### Added

- `WithOptionalSkills(dirs...)`: a skills directory that does not exist
  is passed over, and one that exists and cannot be read is still an
  error. The README's example uses it for `~/.dax/skills`, which failed
  `New` for every user who had not written a skill. When none of the
  directories exists and no other source is given, there is no skills
  part and no skill tool. `WithSkills` stays strict. (#29)

### Documentation

- `docs/manual.md` writes out the guard pass over `New`'s render, the
  parts function's fallback to that guarded render, the manifest
  `memory_save` is based on, the recorder on the context, the compactor
  and the re-grant. The undefined `replaceMemoryGroup` is now
  `withMemoryGroup`, described where it is called. `TestTheManualPathKeepsARedactedSecretOutOfTheRecord` runs the
  manual path under `guard.Redact` and reads every config entry. (#32)

## v0.0.2 - 2026-09-29

The round 4 findings (#15 to #26), and the siblings' round 4 releases
taken up. Three of the fixes close places the trust boundary leaked:
#19, #24 and #18.

### Security

- A secret an input guard kept from the model is no longer written to
  the session. `New` runs the input guards over each instructions part,
  as each turn does, so `Config.Instructions` is the text they leave,
  and `Kit.PartsFor` falls back to that guarded render, never the
  layers' own. The recorder settles a run's first config entry from the
  configuration's instructions before any hook runs, so with
  `guard.Redact` the first entry of every session carried the text the
  model never saw. A guard that refuses a part at `New` is now an error
  from `New`. The test reads every config entry, not the last. (#19)
- **Breaking**: `WithSkillGrantScope` revokes on every turn whose
  transcript tail, back to the last item the model or a tool produced,
  holds a user message, not only on turn 1 of a run that ended with one.
  A follow-up, a steer, or a prompt with a developer note, an item
  reference or any other item after the user's message kept the last
  request's grant, so a skill's `allowed-tools` covered a command the
  next message asked for. A `Resume` after an approval still keeps the
  grant. One case remains: an output `Agent.Deliver` hands in after a
  steer ends the tail as a `Resume`'s does, since `TurnStartInfo` does
  not say what arrived; that is agentturn's to add. (#24)
- The engine `WithPolicy` builds is given `agentpolicy.WithTools` with
  the new `Kit.LookupTool`, the union as of the current turn, so the
  batch hold reads a sibling's confinement: a call beside a confined
  command was held with nobody asked. An `agentpolicy.WithTools` in
  `WithPolicy`'s options replaces the kit's. (#18)
- **Breaking**: with `WithPolicy`, `WithBeforeToolCall` hooks are folded
  into the engine's decision through agentpolicy v0.0.7's `WithHooks`
  rather than chained after it, so a product hook that asks about a call
  holds its siblings with it, where they ran before anyone answered. A
  hook may now be called for a call more than once, and before the loop
  hands it that call, so it must decide a call the same way each time.
  With `WithEngine` or no policy they are chained as before.

### Changed

- **Breaking**: requires agenttool and agenttool/mcpclient v0.0.10,
  agentturn and agentturn/session v0.0.11, agentsession v0.0.11,
  agentskill v0.0.7, agentmemory v0.0.6 and agentpolicy v0.0.7. Sessions
  are written as `agentsession/0.8`. `examples/a2a` requires the
  matching agentturn front and tool modules.
- **Breaking**: `Kit.PartsFor` takes a `context.Context` first, the
  signature agentturn/session v0.0.11's `WithInstructionsParts` takes.
  A caller adds the parameter. It is safe on a nil kit.
- **Breaking**: the memory block is a group of parts, one per piece
  `agentmemory.RenderParts` returns (`memory`, `memory/<scope>`,
  `memory/<scope>/<name>`, `memory/<scope>:omitted`, `memory:summary`)
  and `agentmemory.Usage()` as the new `PartMemoryUsage`,
  `memory:usage`. The joined instructions are unchanged. A write to one
  entry is recorded as that entry's part and the summary rather than the
  whole block: through the kit the same 13 byte save cost a 33 KB delta.
  `WithOrder` places the group by `PartMemory`. A caller that looked for
  one part with ID `memory` in `Kit.Parts` reads the group. (#21)
- **Breaking**: a memory `Omission.What` is `agentmemory.PartID(scope,
  name)`, `memory/<scope>/<name>`, where it was `<scope>/<name>`, so the
  IDs in `instructions_omitted` are the part IDs agentmemory documents.
  (#21)
- **Breaking**: a skill grant's default source is `"agentskill:"` and
  the skill's listed name, and `SkillGrant.Skill` is the listed name, so
  a root `deploy` and a qualified `apps/web:deploy` are two grants; one
  replaced the other's. `WithSkillGrants` says a source function should
  key on `ListedName()` too. (#25)
- `memory_save` is given `agentmemory.WithRendered(kit.MemoryManifest)`
  ahead of `WithMemoryTools`, so a write another session made after the
  render is reported by `LostUpdates` and the model is told; through the
  kit it was lost unreported. (#22)
- The memory manifest is recorded once per render per session, keyed by
  the session ID on the run's context, where it was once per kit: a kit
  under `WithRecorder` inside a parent's child agent wrote it to the
  first child session alone. The ID is there when the child runs under
  the recorder's `ChildContext`, as `WithChildAgent`'s does and the
  manual says to; a child run without it is keyed as the recorder's own
  session, as before. (#20)
- `Kit.Omitted` names the skill a shadowed one lost to, read from
  `Catalog.Listed`, where it named the last skill of that name in
  source order, a qualified or unlisted one included. A skill shadowed
  under a qualified name already taken is still blamed on the bare
  name's winner, since agentskill does not record which it lost to.
  (#26)
- `WithMCPTransport` labels a server by what the transport reaches: a
  `*mcp.CommandTransport`'s program, an HTTP transport's scheme and
  host, or its type as before. (#16)
- **Breaking**: a `WithMCP` server's label is `mcp:#<n> <program>`, the
  command's first word, where it was the whole command line. Arguments
  are where a credential is put, and the label reaches `Conflict`,
  `Kit.Tools`, `WithToolFilter` and errors from `New`, which products
  log. A `WithToolFilter` that matched the arguments matches the
  program and position. (#16)
- `Kit.PartsFor`, answering for the configuration's own instructions,
  returns the omissions of the render they came from, where it returned
  the latest render's.
- `Kit.Transcript`'s doc says what it returns for a fork, the prefix up
  to the header's `Base`. (#17)

### Added

- `WithFoldObserver(fn)` is told of each fold compaction makes, after
  the recorder wrote it. `compact.WithOnFold` holds one function, so a
  product's own in `WithCompaction` was replaced whenever a session was
  recorded; `WithCompaction`'s doc now says so. (#15)
- `WithMCPStderr(w)` sends the stderr of every server `WithMCP` starts
  to `w`, serialising the servers' writes, and `New`'s error for a
  server that fails to connect ends with the last two kilobytes it wrote
  there; it went to the null device. (#16)
- With `WithToolElicitor` set, every MCP server is dialed with
  mcpclient v0.0.10's `WithElicitation`, so a question a server asks
  mid-call reaches the elicitor; the option's doc promised it and the
  client offered no elicitation. Every server may then ask the user; a
  product passes a less trusted server its own `ElicitationHandler`
  through `mcpclient.WithClientOptions`, which takes precedence.
- `WithMemoryReadScopes(scopes...)` renders scopes the model may read
  and not write: the memory tools are built over the writable scopes
  with `agentmemory.WithReadScopes` naming these. A panic from
  `agentmemory.Tools`, such as a read scope `WithMemory` also makes
  writable, is an error from `New`, and so is a memory with no writable
  scope. (#23)
- `PartsFrom(kits...)` is a parts function for a recorder several kits
  share, the first kit whose parts join to the request answering, and
  the `Kit` doc has a paragraph on recording a handoff between two
  kits. (#17)
- `Kit.AgentOptions()` is agentturn/session v0.0.11's `AgentOptions`
  for the session the kit opened: the transcript at the leaf and the
  calls pending there, so a call held before a restart is seeded as
  held and an approval of it runs, where seeded from the transcript
  alone it is unknown and v0.0.11 refuses the approval.
- `Kit.LookupTool(name)`, in the signature `agentpolicy.WithTools`
  takes, for a product building its own engine for `WithEngine`. It
  reads the kit's union, not a run's, so concurrent runs whose tool
  lists differ by name read the list offered last; its doc says to give
  such runs a kit each. (#18)

## v0.0.1 - 2026-09-28

The first version of the module, with the fixes the round 3 studies
asked for folded in. A product built on the pinned commit b752942 has
the changes marked **Breaking** below to take.

### Changed

- **Breaking**: requires openresponses v0.0.12, agenttool and
  agenttool/mcpclient v0.0.9, agentturn and agentturn/session v0.0.10,
  agentsession v0.0.9, agentskill v0.0.6, agentmemory v0.0.5 and
  agentpolicy v0.0.6. The README's version table now matches `go.mod`,
  and `TestTheREADMEVersionTableIsGoMod` fails when it does not. (#10)
- **Breaking**: the memory part ends with `agentmemory.Usage()`, as the
  skills part ends with the catalogue's usage, since the memory tools
  are offered whenever the block is; `docs/manual.md` says so. The
  paragraph is paid for out of `WithInstructionBudget` first, and a
  block the budget drops takes it along. A golden that pins the joined
  instructions moves. (#9)
- **Breaking**: the input guards run over each instructions part on its
  own before the join, then over the whole request as before, so a part
  `guard.Redact` rewrote holds the rewritten text and `Kit.Parts` is the
  request that was sent. A guard therefore sees each part's text twice,
  once alone and once joined. (#2)
- **Breaking**: `WithSkillGrants` grants on every read of a skill and
  reports every read; the kit no longer skips a skill it granted before,
  so a read after a revoke puts the grant back. The grant wrapper is
  `agenttool.Wrap`, which forwards every property the catalogue's tool
  declares, so the kit no longer has a wrapper type a product cannot
  construct. (#12, #7)
- **Breaking**: `WithTransform` and compaction compose through
  agentturn v0.0.10's `ChainTransform`, the product's first, where `New`
  refused the pair. (agentturn#115)
- `Kit.Omitted` and `Kit.OmittedParts` report the memory entries the
  last render left out, where they reported the first render's.
- The README's approval example answers with
  `agentturn.Approve(id).WithBy(agentpolicy.ByHuman)`, so a person's
  approval is recorded as a person's, and a run test pins that the
  kit-built run records the `by`. (#4)

### Added

- `WithSession` and `WithResumedSession` open the recorder with
  `session.WithInstructionsParts(kit.PartsFor)`, so its config entries
  carry `instructions_parts` and `instructions_omitted` rather than one
  string, and a memory write is a delta naming the memory part.
  `Kit.PartsFor(req)` returns the parts a request's instructions are
  composed of, or nil when they do not join to it, in the signature the
  option takes. (#2, needs agentturn#114)
- With a session configured, the engine the kit builds and the guards
  record their verdicts through `Recorder.Annotate` under
  `agentpolicy.VerdictNS`, in the shape `Verdict.Record` gives: every
  engine verdict, and every guard verdict that blocked or gave a
  reason. A per-part guard verdict names its part in `Subject`,
  `instructions/<id>`. `WithVerdictObserver` is the product's observer
  beside the recording, and the one that sees the guards' verdicts. An
  `agentpolicy.WithObserver` passed to `WithPolicy` runs beside the
  kit's, since agentpolicy v0.0.6 keeps every observer, so the
  recording no longer depends on which option a product used. The
  guards are now a `guard.Chain`. (#3, needs agentpolicy#31 and the
  accumulating observers of agentpolicy v0.0.6)
- `WithRecorder(rec)` builds a kit onto a recorder it did not open, an
  evaluation runner's or a parent's, and binds everything to it that it
  binds to its own: `ToolRecorder`, the fold, a child agent, the memory
  manifest and the verdicts. It opens nothing, `Kit.Attach` is a no-op
  under it, and it is refused beside `WithSession` or
  `WithResumedSession`. (#6)
- `WithChildAgent` runs the child under `Recorder.ChildContext` through
  `childagent.WithRunContext`, and with `WithMemory` puts the child's
  session ID under `agentmemory.WithSession` as well, so a memory the
  child saves names the child's session. The README says a host does the
  same for its own run with `agentmemory.WithSession(ctx,
  kit.SessionID())`. (#5)
- `WithSkillGrantScope` revokes every skill grant when a new user
  message starts a run, as Claude Code clears `allowed-tools` at the
  next message; a `Resume` after an approval and a `Continue` keep
  them; `Kit.RevokeSkillGrants(ctx)` is
  the same revoke for a front to call. (#12, #7)
- `WithToolWrap(func(source, tool) tool)` replaces tools in the union,
  with the source label `WithToolFilter` is given, inside the kit's own
  wrapper, so a replay or a logger reaches every tool and a served skill
  read still grants. (#8)
- `Kit.Tools()` lists each tool in the union at `New` with its source
  label, as `ToolOrigin`, so a product names the libraries' tools to a
  policy without a filter as a side channel. (#11)
- `WithToolElicitor(by, fn)` sets `Config.ToolElicitor`, new in
  agentturn v0.0.10, around the recorder's `Elicitor` when there is a
  session, so a tool's question and its answer are on the record.

### The first version

The first version of the module: `New`, `Kit`, `Kit.Config`, and the
options that feed them. `Config()` returns a plain `agentturn.Config`,
every field of which a product could have set by hand with the same
values; [`docs/manual.md`](docs/manual.md) names the call for each one.

The kit owns three compositions no single library can own:

- **Instruction assembly.** `Kit.Parts` is the ordered
  `[]agentsession.InstructionPart` whose texts, joined with
  `agentkit.Separator`, are `Config.Instructions`. The default order is
  the product's prompt, the skill catalogue, memory, then the AGENTS.md
  chain; `WithOrder` changes it and
  [`docs/ordering.md`](docs/ordering.md) argues every position.
  `Kit.Omitted` is one list of everything the layers left out, and
  `WithInstructionBudget` bounds the joined text.
- **Hook composition**, through agentturn's `Chain*` functions in one
  documented order per contested field. `Transform` has no chain, so
  compaction and a product's own `Transform` are mutually exclusive and
  `New` refuses both.
- **The tool set**, a stable union of the product's tools, the skill
  catalogue's, memory's, each MCP server's and any provider's, behind
  `agentpolicy`'s `ToolProvider`. Every source carries a label, so a
  duplicate name at `New` is a `Conflict` naming both sides; one that
  appears at turn time drops the later tool and reports it through
  `WithToolConflict`. `WithToolFilter` selects tools by source.

Options for the rest of `agentturn.Config`: `WithModel`, `WithName`,
`WithInstructions`, `WithSkills`, `WithMemory`, `WithAgentsMD`,
`WithPolicy`, `WithGuards`, `WithTools`, `WithMCP`, `WithChildAgent`,
`WithDeferredTools`, `WithToolProvider`, `WithCompaction`,
`WithCompactor`, `WithRequest`, `WithRequestExtra`, `WithReasoning`,
`WithText`, `WithMaxTurns`, `WithFilter`, `WithRetry`,
`WithToolExecution`, `WithMaxParallelTools`, and one `With*` per hook.

Sessions: `WithSession` and `WithResumedSession` start or resume a
session, bind `compact.WithOnFold` to its recorder, and set
`Config.ToolRecorder` to the recorder's `RecordFunc`, so a record a
tool writes with `agenttool.WriteRecord` while it runs lands in the
session beside its call. `Kit.Attach`
subscribes that recorder to the agent and returns the unsubscribe —
the one step a `Config` cannot carry, since the agent does not exist
until `Config()` has been handed to `agentturn.New`, and a session
never attached records nothing. It is a no-op without a session. The
memory manifest is recorded under `agentmemory.ManifestNS` when its
hash moves, and `Kit.Transcript` is what a resumed session left off
at.

`WithSkillGrants` grants a skill's `allowed-tools` to the policy engine
when the model reads that skill. It is off by default and attributes
the grant to an untrusted source unless the caller says otherwise,
because a skill widening what the agent may do is the product's call.

[`docs/composition.md`](docs/composition.md) and the nested
`examples/a2a` module cover calling an a2a peer, serving as one, and
recording a child agent into the parent's session.
