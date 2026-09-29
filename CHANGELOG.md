# Changelog

The format is [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

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
