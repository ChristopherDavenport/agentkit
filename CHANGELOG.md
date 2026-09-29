# Changelog

The format is [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## Unreleased

The first version of the module, with the fixes the round 3 studies
asked for folded in. A product built on the pinned commit b752942 has
the changes marked **Breaking** below to take.

### Changed

- **Breaking**: requires openresponses v0.0.12, agenttool and
  agenttool/mcpclient v0.0.9, agentturn and agentturn/session v0.0.10,
  agentsession v0.0.9, agentskill v0.0.6, agentmemory v0.0.5 and
  agentpolicy v0.0.5. The README's version table now matches `go.mod`,
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
  beside the recording. An `agentpolicy.WithObserver` passed to
  `WithPolicy` still replaces the kit's, as any later observer would.
  The guards are now a `guard.Chain`. (#3, needs agentpolicy#31)
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
