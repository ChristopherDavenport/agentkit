# The manual path

The kit's one rule is that `Config()` returns a plain
`agentturn.Config`, every field of which a product could have set by
hand, with the same values, by calling the same exported functions.

The test of the rule is this document. For every field of
`agentturn.Config` the kit sets, there is a line here naming the
exported call a product would write instead, and
`TestEveryFieldTheKitSetsIsDocumented` fails if the kit sets a field
this document does not name. `TestTheManualPathIsTheSamePath` writes
the manual side out for a full configuration and compares it to the
kit's.

If a line here ever has to say "and then something the kit does
privately", the kit has become a framework and the rule is broken.

## The fields

| field | the kit's option | what a product writes instead |
|---|---|---|
| `Name`, `Description` | `WithName` | assign them |
| `Model`, `ModelName` | `WithModel` | assign them |
| `Request`, `RequestExtra`, `Reasoning`, `Text`, `MaxTurns`, `Filter` | `WithRequest`, `WithRequestExtra`, `WithReasoning`, `WithText`, `WithMaxTurns`, `WithFilter` | assign them; the kit copies them through and owns no member of any |
| `Retry`, `ToolExecution`, `MaxParallelTools` | `WithRetry`, `WithToolExecution`, `WithMaxParallelTools` | assign them; the kit contests none of them and reads none of them |
| `Instructions` | `WithInstructions`, `WithSkills`, `WithMemory`, `WithAgentsMD` | `strings.Join` of the four blocks, in the order of [ordering.md](ordering.md) — see below |
| `Tools` | — | the kit never sets it; it sets `ToolProvider`, which the loop reads in its place |
| `ToolProvider` | `WithTools`, `WithSkills`, `WithMemory`, `WithMCP`, `WithChildAgent`, `WithDeferredTools`, `WithToolProvider`, `WithToolFilter`, `WithPolicy` | `append` the slices and wrap in `engine.ToolProvider` — see below |
| `BeforeTurn` | `WithBeforeTurn` | `agentturn.ChainBeforeTurn(fns...)` |
| `BeforeModelCall` | `WithMemory`, `WithGuards`, `WithBeforeModelCall` | `agentturn.ChainBeforeModelCall(rerender, guard.BeforeModelCall(gs...), yours...)` |
| `BeforeToolCall` | `WithPolicy`, `WithBeforeToolCall` | `agentturn.ChainBeforeToolCall(engine.BeforeToolCall(), yours...)` |
| `AfterToolCall` | `WithAfterToolCall` | assign it; the kit contests nothing here |
| `OutputGuard` | `WithGuards`, `WithOutputGuard` | `agentturn.ChainOutputGuard(guard.OutputGuard(gs...), yours...)` |
| `ShouldStopAfterTurn` | `WithGuards`, `WithShouldStopAfterTurn` | `agentturn.ChainShouldStopAfterTurn(guard.ShouldStopAfterTurn(gs...), yours...)` |
| `Transform` | `WithCompaction`, `WithCompactor`, `WithTransform` | `compact.NewLocal(model, compact.WithModel(name), compact.WithBudget(n), compact.WithOnFold(rec.Fold)).Transform` |
| `ToolRecorder` | `WithSession`, `WithResumedSession` | `rec.RecordFunc()`; without a session the kit leaves it nil, and the loop honours a recorder the product installs with `agenttool.ContextWithRecorder` on the prompt's context |

A field no option named is left at its zero value, so the loop's own
default applies. `chain1` returns the one hook unchanged when there is
one, so a field only one layer contested holds that layer's function
and not a chain around it.

## `Instructions`

```go
cat, _ := agentskill.DiscoverDirs(dirs...)
chain, _ := agentsmd.Chain(cwd, opts)
block, manifest, _ := agentmemory.Render(ctx, store, scopes)

cfg.Instructions = strings.Join([]string{
	prompt,
	cat.Prompt() + "\n\n" + cat.Usage(),
	block,
	agentsmd.Render(chain.Files),
}, "\n\n")
```

`"\n\n"` is `agentkit.Separator`, which is `agentsession.PartSeparator`:
the session format's own, so the joined text of `Kit.Parts()` is what
the request carried and what its hash covers.

`cat.Usage()` is appended only when `cat.Tool()` is offered, since it
tells the model to reach the listed skills through that tool.
`WithoutSkillTool` drops both.

Under `WithInstructionBudget`, the layers that take a bound are given
one: `agentsmd.Options.Budget` and `agentmemory.WithMaxTotalBytes`, in
the allocation [ordering.md](ordering.md) argues for. Both are
exported, and a product that wants to spend the budget differently sets
them itself and leaves the kit's budget at zero.

## `ToolProvider`

```go
tools := append([]agenttool.Tool(nil), productTools...)
tools = append(tools, childagent.New(childCfg, childagent.WithObserver(rec.Observe)))
tools = append(tools, cat.Tool())
tools = append(tools, agentmemory.Tools(store, scopes)...)
tools = append(tools, remote.Tools()...)

cfg.ToolProvider = engine.ToolProvider(func(ctx context.Context) []agenttool.Tool {
	return append(tools, remote.Tools()...) // the MCP list per turn
})
```

The kit sets `ToolProvider` and never `Tools`, because an MCP server's
list changes and the loop reads the provider once per turn. With no
policy there is no `engine.ToolProvider` wrapper and the inner function
is the field.

`WithChildAgent` is the line above with `childagent.New`: the kit binds
`WithObserver` to the recorder it made and adds nothing else, so a
product that builds its own child hands it to `WithTools` and the
result is the same tool. `WithDeferredTools` is the same again for a
tool that needs the engine, the catalogue or the session; both exist
because the kit is what opens those, not because it wraps them.

Two things the kit adds that the `append` above does not:

- The **duplicate check**. `append` puts both tools in the list and the
  loop dispatches whichever comes first. The kit refuses a duplicate
  present at `New` with a `Conflict` naming both sources, and drops the
  later of one that appears at turn time, telling `WithToolConflict`. A
  product that wants the same check without the kit writes the same
  loop over `t.Name()`.
- The **filter**, `WithToolFilter`, which is a `func` over the same
  list — `slices.DeleteFunc` on the `append` above. What the kit adds
  is the source label, which it has because it did the union and the
  libraries the tools came from did not.

## `BeforeModelCall`, in full

```go
rerender := func(ctx context.Context, req *openresponses.Request) error {
	block, m, err := agentmemory.Render(ctx, store, scopes)
	if err != nil {
		return err
	}
	parts[memoryIndex].Text = block
	req.Instructions = agentsession.JoinInstructions(parts)
	if h := m.Hash(); h != recorded {
		recorded = h
		ns, data := m.Record()
		return rec.Annotate(ctx, ns, json.RawMessage(data))
	}
	return nil
}
cfg.BeforeModelCall = agentturn.ChainBeforeModelCall(
	rerender,
	guard.BeforeModelCall(guards...),
	productHook,
)
```

This is what `WithMemory`, `WithGuards` and `WithBeforeModelCall`
compose to, and it is the whole of it. The re-render is a
`BeforeModelCall` and not a `Transform` because a `Transform` cannot
reach the instructions and what it injects is not recorded.

## What the kit does that no line here covers

Three things, all outside `agentturn.Config`:

- `Kit.Attach(agent)` is `rec.Attach(agent)`, and returns the same
  unsubscribe. It cannot be a config field because the recorder
  subscribes to the *agent*, which does not exist until `Config()` has
  been handed to `agentturn.New`. The kit's only addition is returning
  a no-op instead of nil when there is no session, so a caller need not
  branch. A session that is never attached records nothing:

  ```go
  agent := agentturn.New(kit.Config())
  defer kit.Attach(agent)()   // manually: defer rec.Attach(agent)()
  ```

- `Kit.Omitted()` collects what each layer reported it left out —
  `agentsmd.Result.Omitted`, `agentmemory.Manifest.Omitted`, the skills
  `agentskill.Catalog.Listed` does not offer — into one list. A product
  reads the three itself if it would rather.
- `WithSkillGrants` calls `agentpolicy.Engine.GrantSet` with the rules
  from `agentskill.Skill.Rules()` when the model reads a skill. Both
  calls are exported; the kit is only the place they meet.

None of them changes a field of the config, so none can make the manual
path a different path.
