# Changelog

The format is [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/).

## Unreleased

### Added

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
