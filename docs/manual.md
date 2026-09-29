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
| `ToolProvider` | `WithTools`, `WithSkills`, `WithMemory`, `WithMCP`, `WithChildAgent`, `WithDeferredTools`, `WithToolProvider`, `WithToolFilter`, `WithToolWrap`, `WithPolicy` | `append` the slices, wrap each tool, and wrap the list in `engine.ToolProvider` — see below |
| `BeforeTurn` | `WithSkillGrantScope`, `WithBeforeTurn` | `agentturn.ChainBeforeTurn(revokeOnUserMessage, yours...)` — see below |
| `BeforeModelCall` | `WithMemory`, `WithGuards`, `WithVerdictObserver`, `WithBeforeModelCall` | `agentturn.ChainBeforeModelCall(instructions, chain.BeforeModelCall(), yours...)` — see below |
| `BeforeToolCall` | `WithPolicy`, `WithBeforeToolCall` | `engine.BeforeToolCall()`, the engine built with `agentpolicy.WithHooks(yours...)`; with `WithEngine` or no policy, `agentturn.ChainBeforeToolCall(engine.BeforeToolCall(), yours...)` — see below |
| `AfterToolCall` | `WithAfterToolCall` | assign it; the kit contests nothing here |
| `OutputGuard` | `WithGuards`, `WithOutputGuard` | `agentturn.ChainOutputGuard(chain.OutputGuard(), yours...)` |
| `ShouldStopAfterTurn` | `WithGuards`, `WithShouldStopAfterTurn` | `agentturn.ChainShouldStopAfterTurn(chain.ShouldStopAfterTurn(), yours...)` |
| `Transform` | `WithCompaction`, `WithCompactor`, `WithTransform`, `WithFoldObserver` | `agentturn.ChainTransform(yours, compact.NewLocal(model, compact.WithModel(name), compact.WithBudget(n), compact.WithOnFold(fold)).Transform)`, where `fold` is `rec.Fold` and then the observer — see below |
| `ToolRecorder` | `WithSession`, `WithResumedSession`, `WithRecorder` | `rec.RecordFunc()`; without a session the kit leaves it nil, and the loop honours a recorder the product installs with `agenttool.ContextWithRecorder` on the prompt's context |
| `ToolElicitor` | `WithToolElicitor` | `rec.Elicitor(by, fn)` with a session, `fn` without one; without the option the kit leaves it nil, and an elicitor on the prompt's context applies |

`chain` in the rows above is one `guard.Chain{Guards: gs, Observer:
observe}`, where `observe` is the verdict observer described under
[`BeforeModelCall`](#beforemodelcall-in-full); with no session and no
`WithVerdictObserver` it has no observer, which is what the package
functions `guard.BeforeModelCall(gs...)` and friends build.

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
	block + "\n\n" + agentmemory.Usage(),
	agentsmd.Render(chain.Files),
}, "\n\n")
```

`"\n\n"` is `agentkit.Separator`, which is `agentsession.PartSeparator`:
the session format's own, so the joined text of `Kit.Parts()` is what
the request carried and what its hash covers.

The kit holds the memory block as parts, `agentmemory.RenderParts`, one
part per piece and `agentmemory.Usage()` as `memory:usage` after them.
`agentmemory.Render` is `RenderParts` joined with the same separator, so
the text above is unchanged; the parts are what `Kit.Parts` and the
recorder see, so a write to one entry is recorded as that entry's part
and the summary. The scopes rendered are `WithMemory`'s and then any
`WithMemoryReadScopes` it does not name.

With `WithGuards`, `New` runs the input guards over each part before
the join, as each turn's hook does (below), so `Instructions` is the
text they leave.

`cat.Usage()` is appended only when `cat.Tool()` is offered, since it
tells the model to reach the listed skills through that tool.
`WithoutSkillTool` drops both. `agentmemory.Usage()` is always appended
to a memory block that is sent, since `WithMemory` always offers
`agentmemory.Tools`; a block the budget drops takes it along.

Under `WithInstructionBudget`, the layers that take a bound are given
one: `agentsmd.Options.Budget` and `agentmemory.WithMaxTotalBytes`, in
the allocation [ordering.md](ordering.md) argues for, the memory
block's bound less the usage paragraph, which cannot be bounded. Both are
exported, and a product that wants to spend the budget differently sets
them itself and leaves the kit's budget at zero.

## `ToolProvider`

```go
tools := append([]agenttool.Tool(nil), productTools...)
tools = append(tools, childagent.New(childCfg,
	childagent.WithObserver(rec.Observe),
	childagent.WithRunContext(func(ctx context.Context, callID string) context.Context {
		ctx = rec.ChildContext(ctx, callID)
		return agentmemory.WithSession(ctx, session.SessionIDFromContext(ctx))
	})))
tools = append(tools, cat.Tool())
tools = append(tools, agentmemory.Tools(store, writable,
	agentmemory.WithRendered(kit.MemoryManifest), // the block the model read
	agentmemory.WithReadScopes(read...),          // WithMemoryReadScopes
)...)
for i, t := range tools {
	tools[i] = wrap(sourceOf(t), t) // WithToolWrap
	if t.Name() == agentskill.ToolName {
		tools[i] = grant(tools[i]) // WithSkillGrants, outside the product's wrapper
	}
}

cfg.ToolProvider = engine.ToolProvider(func(ctx context.Context) []agenttool.Tool {
	return append(tools, remote.Tools()...) // the MCP list per turn
})
```

`grant` is `WithSkillGrants`, under "What the kit does" below. The
product's wrapper runs inside it, so what `grant` wraps is the tool the
product's wrapper returned, and `agenttool.Wrap` is how both keep every
property the tool declares.

The kit sets `ToolProvider` and never `Tools`, because an MCP server's
list changes and the loop reads the provider once per turn. With no
policy there is no `engine.ToolProvider` wrapper and the inner function
is the field.

`agentmemory.WithRendered` bases a `memory_save` on the hash the block
showed the model, so a write another session made after the render is
a lost update `agentmemory.LostUpdates` reports; the kit re-renders
before every model call, so `Kit.MemoryManifest` is that render. The
read scopes are left out of `writable`, since `agentmemory.Tools`
panics on a scope that is both; `New` reports that panic, and a memory
with no writable scope, as an error.

`remote` is `mcpclient.Connect(ctx, t, opts...)`, and the kit adds two
things there. For a command `WithMCP` builds, `cmd.Stderr` is set to
`WithMCPStderr`'s writer, with the last two kilobytes kept for the
error `New` returns when the server fails to start. With
`WithToolElicitor` set, `mcpclient.WithElicitation()` goes ahead of
`opts`, since a client offers elicitation only when asked.

`WithChildAgent` is the line above with `childagent.New`: the kit binds
`WithObserver` and `WithRunContext` to the recorder, the memory bridge
only when `WithMemory` is configured, and adds nothing else, so a
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
  list — `slices.DeleteFunc` on the `append` above — and the
  **wrapper**, `WithToolWrap`, the loop over `tools[i]`. What the kit
  adds to both is the source label, which it has because it did the
  union and the libraries the tools came from did not. `Kit.Tools()`
  returns the same labels once, beside each tool's name, for a product
  naming the libraries' tools to its policy.

## `BeforeModelCall`, in full

```go
observe := func(ctx context.Context, v agentpolicy.Verdict) {
	if v.Guard == "" || v.Action != agentturn.Allow || v.Reason != "" {
		ns, data := v.Record()
		rec.Annotate(ctx, ns, json.RawMessage(data))
	}
	productObserver(ctx, v) // WithVerdictObserver
}
chain := guard.Chain{Guards: guards, Observer: observe}

instructions := func(ctx context.Context, req *openresponses.Request) error {
	pieces, m, err := agentmemory.RenderParts(ctx, store, scopes)
	if err != nil {
		return err
	}
	parts = replaceMemoryGroup(parts, pieces) // each piece a part, then memory:usage
	sid := session.SessionIDFromContext(ctx)   // the child's, under ChildContext
	if sid == "" {
		sid = rec.SessionID()
	}
	if h := m.Hash(); h != recorded[sid] {
		ns, data := m.Record()
		if _, err := rec.Annotate(ctx, ns, json.RawMessage(data)); err != nil {
			return err
		}
		recorded[sid] = h
	}
	sent := make([]agentsession.InstructionPart, 0, len(parts))
	for _, p := range parts {
		one := openresponses.Request{Instructions: p.Text}
		if err := chain.BeforeModelCall()(ctx, &one); err != nil {
			return err
		}
		if one.Instructions != "" {
			p.Text = one.Instructions
			sent = append(sent, p)
		}
	}
	req.Instructions = agentsession.JoinInstructions(sent)
	return nil
}
cfg.BeforeModelCall = agentturn.ChainBeforeModelCall(
	instructions,
	chain.BeforeModelCall(),
	productHook,
)
```

This is what `WithMemory`, `WithGuards`, `WithVerdictObserver` and
`WithBeforeModelCall` compose to, and it is the whole of it. The
re-render is a `BeforeModelCall` and not a `Transform` because a
`Transform` cannot reach the instructions and what it injects is not
recorded.

The guards run twice: over each part alone, so a rewrite such as
`guard.Redact`'s lands in the part it belongs to and `sent` is what the
request carried, and then over the whole request, its items and the
joined text, where `guard.Limit` measures what a server sees. The kit's
observer also sets each per-part verdict's `Subject` to
`instructions/<part id>`, since the chain does not know the part.
Without memory the instructions hook runs only when there are guards,
and only on a request whose instructions are still the parts' join.

`recorded` is keyed by the session the annotation lands in, because one
recorder writes several: a kit under `WithRecorder` inside a parent's
child agent annotates a new child session on each call, and each of
them carries the manifest of the render its run was shown.

The engine the kit builds is given the same `observe`, through
`agentpolicy.WithObserver` ahead of the product's own options. That is
an engine option, not a config field. The engine keeps every observer
it is given (agentpolicy v0.0.6), so a product's own
`agentpolicy.WithObserver` runs beside the kit's recording.

## `BeforeToolCall`, and the engine

```go
// lookup resolves a name in the union the provider last returned,
// which is what Kit.LookupTool does.
lookup := func(name string) (agenttool.Tool, bool) { t, ok := last[name]; return t, ok }
engine, err := agentpolicy.Build(policy, matchers,
	agentpolicy.WithTools(lookup),
	agentpolicy.WithObserver(observe),
	agentpolicy.WithHooks(yours...), // WithBeforeToolCall
)
cfg.BeforeToolCall = engine.BeforeToolCall()
```

The product's hooks go into the engine rather than after it
(agentpolicy v0.0.7), so a hook that asks about a call holds its
siblings in the same batch hold as the policy's own asks; chained after
the engine, the siblings ran before anyone answered. The lookup is how
the engine reads a sibling's confinement before the loop hands it that
sibling's call; `Kit.LookupTool` reads the union the provider last
returned, before the policy's filter. All three are engine options, not
config fields. With `WithEngine` the kit can give the engine none of
them, and chains the product's hooks after the engine's with
`agentturn.ChainBeforeToolCall`.

## `Transform`, and the fold

```go
fold := func(ctx context.Context, f compact.Fold) error {
	if err := rec.Fold(ctx, f); err != nil { // with a session
		return err
	}
	observer(ctx, f) // WithFoldObserver
	return nil
}
```

`compact.WithOnFold` holds one function, so the kit writes the one
that calls both, and passes it after the product's `compact.Option`s.
A `compact.WithOnFold` among those is therefore replaced when there is a
session or an observer, which `WithCompaction` says; with neither, the
kit passes no `WithOnFold` and the product's stands.

## `BeforeTurn`

```go
revokeOnUserMessage := func(ctx context.Context, info agentturn.TurnStartInfo) (openresponses.Items, error) {
	if newUserMessage(info.Transcript) {
		for _, name := range granted { // the sources the grants were made under
			engine.Revoke(ctx, name)
		}
	}
	return nil, nil
}

// newUserMessage: the transcript's tail, back to the last item the model
// or a tool produced, holds a user message. Anything else, a developer
// note, an item reference, a compaction, is passed over.
newUserMessage := func(tr agentturn.Transcript) bool {
	for i := len(tr) - 1; i >= 0; i-- {
		item, _ := agentturn.Unhide(tr[i])
		switch it := item.(type) {
		case *openresponses.Message:
			if it.Role == openresponses.RoleUser {
				return true
			}
			if it.Role == openresponses.RoleAssistant {
				return false
			}
		case *openresponses.FunctionCall, *openresponses.FunctionCallOutput, *openresponses.ReasoningItem:
			return false
		}
	}
	return false
}
```

This is `WithSkillGrantScope`, and `kit.RevokeSkillGrants(ctx)` is the
loop in it. It runs on every turn, not only the first: a follow-up
continues the run it joins and a steer arrives between turns, and
either is a new message that must end the last request's grant. A
`Resume`'s first turn ends with the answered calls' outputs, so an
approval keeps the grant.

## What the kit does that no line here covers

Six things, all outside `agentturn.Config`:

- `Kit.Attach(agent)` is `rec.Attach(agent)`, and returns the same
  unsubscribe. It cannot be a config field because the recorder
  subscribes to the *agent*, which does not exist until `Config()` has
  been handed to `agentturn.New`. The kit's only addition is returning
  a no-op instead of nil when there is no session, or when
  `WithRecorder` gave it a recorder whose owner attaches it, so a
  caller need not branch. A session that is never attached records
  nothing:

  ```go
  agent := agentturn.New(kit.Config())
  defer kit.Attach(agent)()   // manually: defer rec.Attach(agent)()
  ```

- `Kit.Omitted()` collects what each layer reported it left out —
  `agentsmd.Result.Omitted`, `agentmemory.Manifest.Omitted`, the skills
  `agentskill.Catalog.Listed` does not offer — into one list. A product
  reads the three itself if it would rather.
- `WithSkillGrants` calls `agentpolicy.Engine.GrantSet` with the rules
  from `agentskill.Skill.Rules()` each time the model reads a skill,
  around the catalogue's tool through `agenttool.Wrap`, and
  `Kit.RevokeSkillGrants` calls `agentpolicy.Engine.Revoke` for each
  source it granted under. All four calls are exported; the kit is only
  the place they meet.
- `WithSession` and `WithResumedSession` open the recorder with
  `session.WithInstructionsParts(kit.PartsFor)`, so its config entries
  carry `instructions_parts` and `instructions_omitted`. `PartsFor` is
  exported and has that option's signature, so a recorder opened
  elsewhere takes it the same way, and `agentkit.PartsFrom(kits...)`
  asks several kits in turn for a recorder they share.
- `Kit.AgentOptions()` is `session.AgentOptions(sess)`, read in `New`:
  the transcript at the leaf and the calls pending there, for
  `agentturn.New`.
- `Kit.LookupTool(name)` reads the union the provider last returned,
  and is what the kit's engine is given through `agentpolicy.WithTools`.

None of them changes a field of the config, so none can make the manual
path a different path.
