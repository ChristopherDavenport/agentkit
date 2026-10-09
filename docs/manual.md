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

The blocks below are written against the versions the README's table
names, and need agentturn and agentturn/session **v0.0.15 or later**.
The fold block relies on `compact.WithOnFold` adding a callback, which
v0.0.14 brought, and the resume on `session.CompactOptions` and the
reasoning attribution `session.AgentOptions` returns, which v0.0.15
did. agentpolicy v0.0.10 and agentmemory v0.0.9 require v0.0.15, so a
product on the siblings without the kit selects it; one that pins an
older agentpolicy runs `go get github.com/ChristopherDavenport/agentturn@v0.0.15`.
At v0.0.13 a product's `WithOnFold` after the recorder's replaces it,
and no fold is recorded.

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
| `BeforeToolCall` | `WithPolicy`, `WithMemory`, `WithSkillGrants`, `WithSkillGrantScope`, `WithBeforeToolCall` | `agentturn.ChainBeforeToolCall(keepSaveBase, decide)`, `decide` being `engine.BeforeToolCall()` under the call's grant scope, the engine built with `agentpolicy.WithHooks(refuseEndedGrant, yours...)`; with `WithEngine` or no policy, `yours...` after the engine and no `refuseEndedGrant` — see below |
| `AfterToolCall` | `WithAfterToolCall` | assign it; the kit contests nothing here |
| `OutputGuard` | `WithGuards`, `WithOutputGuard` | `agentturn.ChainOutputGuard(chain.OutputGuard(), yours...)` |
| `ShouldStopAfterTurn` | `WithGuards`, `WithShouldStopAfterTurn` | `agentturn.ChainShouldStopAfterTurn(chain.ShouldStopAfterTurn(), yours...)` |
| `Transform` | `WithCompaction`, `WithCompactionModel`, `WithCompactor`, `WithTransform`, `WithFoldObserver` | `agentturn.ChainTransform(yours, compact.NewLocal(model, compact.WithModel(name), compact.WithOnFold(fold), compact.WithBudget(n), theirs...).Transform)`, or `compact.New(compactor, ...)` with the same options for `WithCompactor`, where `fold` is `rec.Fold` and then the observer — see below |
| `ToolRecorder` | `WithSession`, `WithResumedSession`, `WithRecorder` | `rec.RecordFunc()`; without a session the kit leaves it nil, and the loop honours a recorder the product installs with `agenttool.ContextWithRecorder` on the prompt's context |
| `ToolElicitor` | `WithToolElicitor` | a function that calls `rec.Elicitor(by, fn)` for the run's recorder, `recorderFor(ctx)` below, and `fn` without one; without the option the kit leaves it nil, and an elicitor on the prompt's context applies |

`chain` in the rows above is one `guard.Chain{Guards: gs, Observer:
observe}`, where `observe` is the verdict observer described under
[`BeforeModelCall`](#beforemodelcall-in-full). With no recorder and no
`WithVerdictObserver` the observer does nothing, and the chain is what
the package functions `guard.BeforeModelCall(gs...)` and friends build.

`recorderFor(ctx)`, in the blocks below, is the recorder a run records
into: the one a front put on the run's context, then the kit's own,
then none.

```go
recorderFor := func(ctx context.Context) *session.Recorder {
	if r := agentkit.RecorderFromContext(ctx); r != nil { // your own key, without the kit
		return r
	}
	return rec // WithSession, WithResumedSession or WithRecorder; nil without
}
```

A front that serves one kit to many conversations, a session each,
prompts each run with `agentkit.ContextWithRecorder(ctx, convRec)`.
The engine, the guards and the hooks are bound once, so a recorder a
front sets with `SetConfig` cannot reach them. The context is the one
thing every one of them is handed. A product without the kit puts the
recorder on the context under a key of its own and reads it the same
way.

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

With `WithGuards` the text is not that join but the guards' pass over
each of its parts, the same pass each turn's hook makes
([below](#beforemodelcall-in-full)):

```go
layers := []agentsession.InstructionPart{
	{ID: "product", Text: prompt},
	{ID: "skills", Text: cat.Prompt() + "\n\n" + cat.Usage()},
	// one part per agentmemory.RenderParts piece, then memory:usage
	{ID: "agentsmd", Text: agentsmd.Render(chain.Files)},
}
first, err := guardParts(ctx, guard.Chain{Guards: guards}, layers)
if err != nil {
	return err // names the part: "instructions/memory/user/aws: ..."
}
cfg.Instructions = agentsession.JoinInstructions(first)
```

`guardParts` is the loop in the `BeforeModelCall` block, and `first` is
kept: it is what the parts function falls back to
([`partsFor`](#what-the-kit-does-that-no-line-here-covers)). The
recorder settles a run's first config entry from `cfg.Instructions`
before any hook runs, so without this pass a secret `guard.Redact` keeps
from the model is written to the session in that entry. There is no
observer on this pass: there is no run to record a verdict under, and
the first turn's pass reports the same verdicts.

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

`cwd` and `opts` are `WithAgentsMD`'s, and `opts` is passed to
`agentsmd.Chain` verbatim. A product whose project is not on the host's
file system, such as a container or a remote workspace, sets
`opts.FS` to that workspace's `fs.FS`; the path and `opts.Root` are
then names in it, `"."` its root, each file's `Path` is its name there,
and `opts.Extra` stays OS paths:

```go
opts := agentsmd.Options{FS: ws.FS(), Root: "."}
chain, _ := agentsmd.Chain(".", opts) // or a sub-directory's name
```

which is the chain `agentkit.WithAgentsMD(".", opts)` reads.

`dirs` are `WithSkills`' and `WithOptionalSkills`' in the order given;
an optional one is passed over when `agentskill.Dir` fails with
`fs.ErrNotExist`, and refused otherwise, as a `WithSkills` one always
is.

`cat.Usage()` is appended only when `cat.Tool()` is offered, since it
tells the model to reach the listed skills through that tool.
`WithoutSkillTool` drops both. `agentmemory.Usage()` is always appended
to a memory block that is sent, since `WithMemory` offers
`agentmemory.Tools`; a block the budget drops takes it along, and the
writes with it: the request that block was dropped from is not offered
`memory_save`, `memory_patch` or `memory_forget`, and the run refuses
them, outright or when a held one is approved (below).

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
	childagent.WithObserver(func(ctx context.Context, ev agentturn.Event) {
		if r := recorderFor(ctx); r != nil {
			r.Observe(ctx, ev)
		}
	}),
	childagent.WithRunContext(func(ctx context.Context, callID string) context.Context {
		parent := scopeOf(ctx) // below
		child := callID
		if r := recorderFor(ctx); r != nil {
			ctx = r.ChildContext(ctx, callID)
			if id := session.SessionIDFromContext(ctx); id != "" {
				child = id
				if withMemory {
					ctx = agentmemory.WithSession(ctx, id)
				}
			}
		}
		// Under WithSkillGrants only: the child's own grant scope, under its
		// conversation's, which the child's skill reads grant under, and a
		// record of whose child it is, so the conversation's next message
		// ends its grants: they are bound to the parent's user-message mark.
		return agentpolicy.ContextWithGrantScope(ctx, parent+"/"+child)
	})))
// The catalogue's tool as it stands: ReloadSkills puts another
// catalogue's in its place, and every run sees it from its next call.
tools = append(tools, agenttool.Wrap(cat.Tool(), func(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
	return currentCat().Tool().Execute(ctx, call) // under a lock in real code
}))

// memory_save is based on the render its call was composed from:
// saveBase[call], kept by keepSaveBase when the call was decided, then
// rendered[run], which the BeforeModelCall hook keeps, then the manifest
// in force on the run's session's path at the call. A call in a run
// that finds none of them is refused; one outside any run takes the last.
based := func(m func() agentmemory.Manifest) []agenttool.Tool {
	return agentmemory.Tools(store, writable,
		agentmemory.WithRendered(m),         // the block the model read
		agentmemory.WithReadScopes(read...), // WithMemoryReadScopes
	)
}
memTools := based(func() agentmemory.Manifest { return last })
for i, t := range memTools {
	if t.Name() == agentmemory.SaveTool {
		memTools[i] = agenttool.Wrap(t, func(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
			key := sessionID(ctx) + "\x00" + call.ID // a call ID names a call in one conversation
			run := agentturn.RunIDFromContext(ctx)
			m, ok := saveBase[key]
			if ok {
				delete(saveBase, key)
			} else if r, found := rendered[run]; found {
				m, ok = r.man, true
			}
			if !ok {
				m, ok = manifestAtCall(ctx, call.ID) // below
			}
			if !ok && run != "" {
				return agenttool.Result{}, errSaveBase // "... search for the entry with memory_search and save again"
			}
			if !ok {
				return t.Execute(ctx, call) // based on last
			}
			for _, own := range based(func() agentmemory.Manifest { return m }) {
				if own.Name() == agentmemory.SaveTool {
					return own.Execute(ctx, call)
				}
			}
			return t.Execute(ctx, call)
		})
	}
	// The writes are refused in a run whose last render the budget
	// dropped: the request did not offer them, and the model was shown no
	// block to write over.
	if t := memTools[i]; t.Name() == agentmemory.SaveTool || t.Name() == agentmemory.PatchTool || t.Name() == agentmemory.ForgetTool {
		memTools[i] = agenttool.Wrap(t, func(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
			if r, ok := rendered[agentturn.RunIDFromContext(ctx)]; ok && r.dropped {
				return agenttool.Result{}, errMemoryDropped // "... memory_search still reads it"
			}
			return t.Execute(ctx, call)
		})
	}
}
tools = append(tools, memTools...)
for i, t := range tools {
	tools[i] = wrap(sourceOf(t), t) // WithToolWrap
	if t.Name() == agentskill.ToolName {
		tools[i] = grant(tools[i]) // WithSkillGrants, outside the product's wrapper
	}
}

offered := engine.ToolProvider(func(ctx context.Context) []agenttool.Tool {
	list := append(tools, remote.Tools()...) // the MCP list per turn
	if run := agentturn.RunIDFromContext(ctx); run != "" {
		unions[run] = byName(list) // for lookup, below
	} else {
		own = byName(list)
	}
	return list
})
cfg.ToolProvider = func(ctx context.Context) []agenttool.Tool {
	// Under WithSkillGrants the engine's offer filter reads the grant
	// scope of the conversation off the context it is consulted with.
	return offered(agentpolicy.ContextWithGrantScope(ctx, scopeOf(ctx)))
}
```

`grant` is `WithSkillGrants`, under "What the kit does" below. The
product's wrapper runs inside it, so what `grant` wraps is the tool the
product's wrapper returned, and `agenttool.Wrap` is how both keep every
property the tool declares.

The kit sets `ToolProvider` and never `Tools`, because an MCP server's
list changes and the loop reads the provider once per turn. With no
policy there is no `engine.ToolProvider` wrapper and the inner function
is the field, and without skill grants the wrapper is `offered` itself,
the scope being put on the context only because the kit makes grants
under it.

`agentmemory.WithRendered` bases a `memory_save` on the hash the block
showed the model, so a write another session made after the render is
a lost update `agentmemory.LostUpdates` reports. The render is the
save's own run's: the hook keeps each run's last render under
`agentturn.RunIDFromContext(ctx)`, and the save is built per call over
it (`based(...)[0]` is `memory_save`, the first tool `agentmemory.Tools`
returns). Two runs off one kit render in turn, and a save based on the
other run's render, which may already hold a write a third session made
in between, discards that write with nothing reported. A save held for
approval runs in the `Resume`, a run of its own that has not rendered,
so `saveBase` keeps its run's render: `keepSaveBase`, the first
`BeforeToolCall` hook ([below](#beforetoolcall-and-the-engine)), puts
it there each time a `memory_save` call is decided in a run that
rendered, keyed by the session on the context and the call ID, since
two conversations whose provider numbers its calls both have a
`call_0`. It runs ahead of whatever holds the call, the kit's engine,
one built for `WithEngine` or a product hook, so the base is kept
whoever holds it. After a restart, or once the bounded maps have
dropped the run, `manifestAtCall` folds the manifest records on the
path of the run's session, `recorderFor(ctx)`'s, read through
`rec.Store().Open`, up to the call, and takes what is in force there
only when no other run's render can be the last record:

```go
manifestAtCall := func(ctx context.Context, callID string) (agentmemory.Manifest, bool) {
	r := recorderFor(ctx)
	if r == nil {
		return agentmemory.Manifest{}, false
	}
	sess, err := r.Store().Open(ctx, runSessionID(ctx, r)) // the live session the store holds
	if err != nil {
		return agentmemory.Manifest{}, false
	}
	var (
		f          agentmemory.ManifestFold
		folded, ok bool
		mixed      bool
		open       = map[string]bool{}
	)
	for _, e := range sess.Path(sess.Leaf()) {
		switch e := e.(type) {
		case *agentsession.CustomEntry:
			if e.NS == agentmemory.ManifestNS {
				ok = f.Apply(e.Data) == nil
				folded = true
			}
		case *agentsession.RunEntry:
			if e.IsStart() {
				mixed = len(open) > 0 // a run started while another was open
				open[e.RunID] = true
			} else if e.IsEnd() {
				delete(open, e.RunID)
			}
		case *agentsession.ItemEntry:
			if fc, isCall := e.Item.(*openresponses.FunctionCall); isCall && fc.CallID == callID {
				if mixed || len(open) > 1 {
					return agentmemory.Manifest{}, false // may be another run's render
				}
				return f.Manifest(), folded && ok
			}
		}
	}
	return agentmemory.Manifest{}, false
}
```

Two runs off one kit may share a session, and a render the second run
recorded between the first run's start and its call is the last record
before the call. Each run's renders are recorded inside it, so when no
other run started in that window and none is open at the call, every
record since the call's run started is that run's.

A call in a run that finds no base is refused with a result telling the
model to search for the entry and save again: a save based on another
run's render could discard a write the model never saw, with nothing
reported. `last`, what `Kit.MemoryManifest` returns, is the base only
for a call outside any run. `rendered` and `saveBase` keep their newest
1,024 keys. The read scopes are left out of `writable`, since
`agentmemory.Tools` panics on a scope that is both; `New` reports that
panic, and a memory with no writable scope, as an error.

`remote` is `mcpclient.Connect(ctx, t, opts...)`, and the kit adds two
things there. For a command `WithMCP` builds, `cmd.Stderr` is set to
`WithMCPStderr`'s writer, with the last two kilobytes kept for the
error `New` returns when the server fails to start. With
`WithToolElicitor` set, `mcpclient.WithElicitation()` goes ahead of
`opts`, since a client offers elicitation only when asked.

`Kit.AddMCP` and `Kit.AddMCPTransport` connect a server after `New`,
the same way, and `Kit.RemoveMCP` closes one. By hand they are a list
of remotes the provider reads each turn, after `New`'s servers and
ahead of `WithToolProvider`'s:

```go
var (
	mu    sync.Mutex
	added []*mcpclient.Remote
)
provider := func(ctx context.Context) []agenttool.Tool {
	out := append(tools, remote.Tools()...)
	mu.Lock()
	for _, r := range added {
		out = append(out, r.Tools()...)
	}
	mu.Unlock()
	return append(out, yours(ctx)...) // WithToolProvider
}

// Mid-session: dial as New does, off the lock, since a dial may wait on
// a sign-in; refuse a name already offered; add.
r, err := mcpclient.Connect(ctx, t, opts...)
if err != nil {
	return err
}
for _, tool := range r.Tools() {
	if from, taken := offeredBy(tool.Name()); taken { // the source of a name in provider(ctx)
		return errors.Join(agentkit.Conflict{Name: tool.Name(), Kept: from, Dropped: label}, r.Close())
	}
}
mu.Lock()
added = append(added, r)
mu.Unlock()

// Removing one: out of the list under the lock, closed after it, since
// Close takes a few seconds at most (agenttool v0.0.15 ends the calls
// in flight with mcpclient.ErrClosed rather than waiting for them).
mu.Lock()
added = slices.DeleteFunc(added, func(a *mcpclient.Remote) bool { return a == r })
mu.Unlock()
return r.Close()
```

`label` is the kit's `mcp:#<n> <what>`, numbered after the servers
before it, which labels each added server's tools for `WithToolFilter`,
`WithToolWrap`, a `Conflict` and `Kit.Tools()`. A turn already running
keeps the tools it was offered, and the recorder writes the new ones as
`tools_added` on the next. The kit holds its lock as the block does, for
the list and never across the dial or the close, so `Kit.Tools()`,
another `RemoveMCP` and `Close` answer while a server is dialed or
closed; `Close` cancels a dial in flight and closes every server, added
or `New`'s, together.

`WithChildAgent` is the line above with `childagent.New`: the kit binds
`WithObserver` and `WithRunContext` to the run's recorder, the memory
bridge only when `WithMemory` is configured, and adds nothing else, so a
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
	if r := recorderFor(ctx); r != nil && (v.Guard == "" || v.Action != agentturn.Allow || v.Reason != "") {
		ns, data := v.Record()
		r.Annotate(ctx, ns, json.RawMessage(data))
	}
	productObserver(ctx, v) // WithVerdictObserver
}
chain := guard.Chain{Guards: guards, Observer: observe}

// guardParts runs the input guards over each part alone. A refusal
// names its part, and so does each verdict's Subject, since the chain
// does not know the part.
guardParts := func(ctx context.Context, c guard.Chain, parts []agentsession.InstructionPart) ([]agentsession.InstructionPart, error) {
	out := make([]agentsession.InstructionPart, 0, len(parts))
	for _, p := range parts {
		pc := c
		if obs := c.Observer; obs != nil {
			subject := "instructions/" + p.ID
			pc.Observer = func(ctx context.Context, v agentpolicy.Verdict) {
				v.Subject = subject
				obs(ctx, v)
			}
		}
		one := openresponses.Request{Instructions: p.Text}
		if err := pc.BeforeModelCall()(ctx, &one); err != nil {
			return nil, fmt.Errorf("instructions/%s: %w", p.ID, err)
		}
		if one.Instructions != "" {
			p.Text = one.Instructions
			out = append(out, p)
		}
	}
	return out, nil
}

instructions := func(ctx context.Context, req *openresponses.Request) error {
	pieces, m, dropped, err := renderUnder(ctx, store, scopes, share) // RenderParts under the budget; see Instructions
	if err != nil {
		return err
	}
	// The memory group's parts replaced by one part per piece and then
	// memory:usage, in the position the first of them held.
	layers = withMemoryGroup(layers, pieces)
	if r := recorderFor(ctx); r != nil {
		if err := record(ctx, r, m); err != nil { // below
			return err
		}
	}
	if sent, err = guardParts(ctx, chain, layers); err != nil {
		return err
	}
	last, rendered[agentturn.RunIDFromContext(ctx)] = m, render{m, dropped} // memory_save's base
	req.Instructions = agentsession.JoinInstructions(sent)
	if dropped {
		req.Tools = slices.DeleteFunc(slices.Clone(req.Tools), func(t openresponses.Tool) bool {
			f, ok := t.(*openresponses.FunctionTool)
			return ok && (f.Name == agentmemory.SaveTool || f.Name == agentmemory.PatchTool || f.Name == agentmemory.ForgetTool)
		})
	}
	return nil
}
cfg.BeforeModelCall = agentturn.ChainBeforeModelCall(
	instructions,
	chain.BeforeModelCall(),
	productHook,
)
```

`record` writes the manifest to the session the annotation lands in:

```go
record := func(ctx context.Context, r *session.Recorder, m agentmemory.Manifest) error {
	sid := runSessionID(ctx, r) // session.SessionIDFromContext, the child's under ChildContext, else r.SessionID()
	prev, seen := recorded[sid]
	var f *fold
	folds := false
	if sess, err := r.Store().Open(ctx, sid); err == nil { // the live session the store holds
		path := sess.Path(sess.Leaf())
		if run := agentturn.RunIDFromContext(ctx); run != "" && !recordsRun(path, run) {
			return nil // a run the session does not record: an agent attached to nothing
		}
		f = folded[sid].upTo(path) // an agentmemory.ManifestFold, from the record it last reached
		if folds = f.inForce() != ""; folds && f.inForce() == m.Hash() {
			recorded[sid] = recordedManifest{m, f.at}
			return nil // nothing moved
		}
	} else if seen && prev.man.Hash() == m.Hash() && lastWritten[r, sid] == prev.entry {
		return nil
	}
	ns, data := m.Record()
	if folds {
		// The smallest record over the manifests the fold holds, which is
		// a delta on the one in force, or on this kit's own last one after
		// a handoff or a restart while the fold still holds it.
		ns, data = f.Record(m) // agentmemory.ManifestFold.Record (v0.0.10)
	}
	entry, err := r.Annotate(ctx, ns, json.RawMessage(data))
	if err != nil {
		return err
	}
	recorded[sid], lastWritten[r, sid] = recordedManifest{m, entry}, entry
	return nil
}
```

`fold` is an `agentmemory.ManifestFold` kept per session with the ID of
the last record it folded and the hash of the manifest in force: `upTo`
folds the records after that one on the path, or the whole path again
when it is no longer there, after a `Rebase`, and `inForce` is that
hash, empty while the last record would not fold. The fold holds the
distinct manifests in force, the last `agentmemory.ManifestFoldDepth`,
and `Record` writes the smallest record over them, so the kit keeps no
copy of its own (agentmemory v0.0.10; before it, the kit kept them again
beside the fold with their hashes, since the fold did not expose them).
`recordsRun` says whether the path records the run: a run entry starts
it and none has ended it, or the path holds no run entry at all, a
session something that writes no runs records, read as one run.

This is what `WithMemory`, `WithGuards`, `WithVerdictObserver` and
`WithBeforeModelCall` compose to, and it is the whole of it. The
re-render is a `BeforeModelCall` and not a `Transform` because a
`Transform` cannot reach the instructions and what it injects is not
recorded.

The guards run twice: over each part alone, so a rewrite such as
`guard.Redact`'s lands in the part it belongs to and `sent` is what the
request carried, and then over the whole request, its items and the
joined text, where `guard.Limit` measures what a server sees. Each
per-part verdict's `Subject` is `instructions/<part id>`, so the record
says which memory entry or AGENTS.md file a guard rewrote or refused.
Without memory the instructions hook runs only when there are guards,
and only on a request whose instructions are still the parts' join.

`recorded` is keyed by the session the annotation lands in, because one
recorder writes several: a kit under `WithRecorder` inside a parent's
child agent annotates a new child session on each call, and each of
them carries the manifest of the render its run was shown.

A manifest is written whenever the render differs from the manifest in
force on the path of the session it lands in, and as
`agentmemory.ManifestFold.Record` returns it: the smallest of the whole
manifest and a delta on each manifest the fold holds, which is a delta
on the one in force or on this kit's own last manifest while the fold
still resolves it, and whole only where nothing is in force, a new
session or a child's. In a handoff the manifest in force is the other
kit's, which shares nothing with this one, and a delta on it was the
whole manifest at every hand-back. A kit restarted into the handoff has
no last manifest of its own in memory, and the fold still holds the one
it wrote before, so its record is a delta on that one while it is among
them; past `ManifestFoldDepth` other manifests it writes whole, as a kit
that was never restarted does. Such a delta is refused by `agentmemory.ApplyManifestRecord`,
so a reader of the session folds with `agentmemory.ManifestFold`
(agentmemory v0.0.9), as the kit does. The path is read through the
recorder's store: `Open` on a session a store holds hands back the live
session it holds, which is how the recorder itself reopens one, so the
session `New` opened, one on a run's context and a child's are read
alike. Every agent built from a kit with a session is attached to it or
prompted under `ContextWithRecorder`; a run that is neither, an agent
attached to nothing or to a recorder of its own with none on its
context, is one the session does not record, and its renders are not
written there: the agent delivers run_start to the recorder before the
run's first model call, so a run the recorder is attached to is open on
the path at every one of its model calls, and a record with no run
around it would be read, by `foldAtCall` after a restart, as the render
of whichever run was open. Such a run's own held save, approved after a
restart, is refused. What is in force is the fold of the path's records, whoever
wrote them: another kit's of a handoff, a `Rebase` or `/clear`, a
restart. So a kit handed
back to after another kit wrote records its render again even when it
did not move, and a render that says what the path already says is not
written. The fold is kept per session and brought up from the record it
last reached, so a turn reads only what was written since the last, not
every record from the root. A store that cannot open
the session gets whole writes, and `lastWritten`, shared by every kit
in the process, is what tells a kit that another kit wrote there since
its own record. `sent` is
shared with the parts function below, under a lock in real code.

The engine the kit builds is given the same `observe`, through
`agentpolicy.WithObserver` ahead of the product's own options, whether
or not the kit has a session, since a run may bring one on its
context. That is
an engine option, not a config field. The engine keeps every observer
it is given (agentpolicy v0.0.6), so a product's own
`agentpolicy.WithObserver` runs beside the kit's recording.

## `BeforeToolCall`, and the engine

```go
// lookup resolves a name in the union the provider last returned to the
// run whose call is decided, which is what Kit.LookupToolFor does: the
// run's own list by the run's ID, and for a run it has none for, or none,
// the kit's own: the list offered outside any run, never another run's.
lookup := func(ctx context.Context, name string) (agenttool.Tool, bool) {
	union, ok := unions[agentturn.RunIDFromContext(ctx)] // a bounded map, filled by the provider
	if !ok {
		union = own
	}
	t, found := union[name]
	return t, found
}
engine, err := agentpolicy.Build(policy, matchers,
	agentpolicy.WithToolsFor(lookup),
	agentpolicy.WithObserver(observe),
	agentpolicy.WithHooks(refuseEndedGrant, yours...), // WithSkillGrantScope, then WithBeforeToolCall
)

// keepSaveBase keeps a memory write's render, the manifest and whether
// the budget dropped the block, for the Resume that may run it, and
// decides nothing (WithMemory).
keepSaveBase := func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
	switch info.Call.Name {
	case agentmemory.SaveTool, agentmemory.PatchTool, agentmemory.ForgetTool:
		if r, ok := rendered[info.RunID]; ok {
			saveBase[sessionID(ctx)+"\x00"+info.Call.CallID] = r
		}
	}
	return nil, nil
}

// scope is the grant scope a run on ctx belongs to, Kit.GrantScope: the
// conversation a skill read there is granted to, and the only one whose
// calls the grant decides. Below.
scopeOf := func(ctx context.Context) string {
	if s := agentpolicy.GrantScopeFromContext(ctx); s != "" {
		return s // a front's own, or a child agent's, which the child's run context carries
	}
	return conversation(ctx) // below
}

// decide is the engine's hook under the grant scope of the call's
// conversation (WithSkillGrants): the sets the engine consults are the
// policy's, the unscoped ones and those of this scope.
decide := func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
	return engine.BeforeToolCall()(agentpolicy.ContextWithGrantScope(ctx, scopeOf(ctx)), info)
}

// refuseEndedGrant blocks, inside the engine, a call that only a grant
// the scope ended allowed and that the engine would only ask about
// (WithSkillGrantScope). endedBy is what the last revocations under
// BeforeTurn ended in each grant scope, by the source each was made
// under, which a read of the skill clears and ReloadSkills prunes of
// skills no longer listed.
type asking struct{}
refuseEndedGrant := func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
	ended := endedBy[scopeOf(ctx)]
	if !scoped || len(ended) == 0 || ctx.Value(asking{}) != nil {
		return nil, nil
	}
	// Every subject of the call must be covered by an ended rule of a
	// skill the catalogue still lists, or the engine decides it.
	subjects := []agentpolicy.Subject{{Args: info.Args}}
	if m := matchers[info.Call.Name]; m.Subjects != nil {
		split, err := m.Subjects(ctx, info.Args)
		if err != nil || len(split) == 0 {
			return nil, nil // the engine fails it closed
		}
		subjects = split
	}
	var skills []string
	for _, s := range subjects {
		tool := cmp.Or(s.Tool, info.Call.Name)
		covered := false
		for _, lg := range ended {
			if _, listed := catalog.Lookup(lg.skill); !listed {
				continue
			}
			for _, r := range lg.rules {
				if _, carve := r.CarveOut(); carve || !r.MatchesTool(tool) {
					continue
				}
				if m := matchers[tool].Match; r.Bare() || m != nil && m(r.Spec, s.Args) {
					covered = true
					if !slices.Contains(skills, lg.skill) {
						skills = append(skills, lg.skill)
					}
					break
				}
			}
			if covered {
				break
			}
		}
		if !covered {
			return nil, nil
		}
	}
	// What the engine decides for the call without the grants, which are
	// revoked: Engine.Would, the decision before the batch hold with the
	// same rules, confinement and hooks, this one among them, and so asked
	// not to refuse again.
	v, err := engine.Would(context.WithValue(ctx, asking{}, true), info)
	if err != nil || v.Action != agentturn.Defer {
		return nil, nil
	}
	if v.Rule == nil && hooksGiven { // yours..., or WithPolicy's options, which may add some
		return nil, nil // the default's ask or a hook's: a grant would not have changed it
	}
	reason := "the tools skill " + skills[0] + " granted ended with the user's last message; " +
		"read the skill again with the " + agentskill.ToolName + " tool, then make this call again"
	return &agentturn.ToolDecision{Action: agentturn.Block, By: agentpolicy.ByPolicy, Reason: reason}, nil
}
cfg.BeforeToolCall = agentturn.ChainBeforeToolCall(keepSaveBase, decide)
```

Without skill grants `decide` is `engine.BeforeToolCall()` itself: the
scope is put on the context only because the kit makes grants under it,
and the kit's `ToolProvider` is wrapped the same way, the context given
to `engine.ToolProvider` carrying the scope, since the engine's offer
filter reads a bare deny of a scoped set from it. A product that calls
the engine on a conversation's behalf puts the scope on the context it
calls with, `agentpolicy.ContextWithGrantScope(ctx, kit.GrantScope(ctx))`:
`engine.Answers` for a `Resume`, `engine.GrantsFor` to show the grants in
force, `engine.RevokeScope` when the conversation ends. `keepSaveBase`
decides nothing and goes first, ahead of whatever may hold or allow the
call: it keeps the render for any holder.

`refuseEndedGrant` is the one decision the kit makes, and it makes it
inside the engine, as the first of the hooks `agentpolicy.WithHooks`
folds into the engine's own, ahead of the product's. Under
`WithSkillGrantScope` a call that every rule of an ended grant would
have allowed, and that the engine would only ask about, is blocked with
a reason the model reads, naming the skill and the tool that reads it
again. The engine would defer it to the ask rule, and the refusal the
model read then, a reviewer's, said nothing of the skill; the note the
turn began with is several items up, and a model refused acts on the
refusal in front of it. Inside the engine rather than chained ahead of
it because the engine's batch hold reads a chained Block as an ask: the
blocked call's allowed siblings were held for a question nobody was
asked. Folded in, the engine's fold takes the Block, the siblings are
decided beside it, and the engine records the decision as the call's
verdict, so the kit writes none itself; it is not reported to
`WithSkillGrantReport`. With `WithEngine` the kit cannot fold a hook
into the engine, so there is no refusal.

Whether the engine would have allowed the call without the grant is
`Engine.Would`'s answer (agentpolicy v0.0.11): the verdict `Decide`
reaches before the batch hold, with the same policy, the grants still in
force under the call's scope, the call's confinement and the hooks
folded in, and nothing remembered or observed. The kit refuses when it
is a `Defer`, which is what the grant answered. A call it allows needs no
grant, and the engine folds its hooks only after a policy that denies,
so a denied call never reaches this one, and a deny rule beats a grant
in any case. Because the hooks are folded into `Would`, this one is
among them, which is what `asking` is for, and the product's are called
once more for it, so a hook must decide a call the same way however
often it is asked. With hooks of the product's, or options that may add
some, the kit refuses only when an ask rule is behind the verdict
(`v.Rule`), since a hook's question would be asked whatever a grant did
and the verdict does not say whose it is. An agentpolicy option of the
product's, `WithAliases` or `WithConfinement` among them, is read as the
engine reads it, where the conservative test this replaced, over
`Engine.Policy()` and `Engine.Grants()`, gave up on any option.

The memory tools the kit offers are wrapped with `agenttool.Wrap`
around what `agentmemory.Tools` returns. The wrapper reads the kept
render first, then the render of the call's run, then, after a restart
or once the kit has dropped the run, the manifest in force at the call
on the run's session's path. `memory_save` runs against a save built
with `agentmemory.WithRendered` over that manifest, so a held save keeps
its base across the Resume, and is refused when none is known rather
than based on another run's render. All three writes are refused when
the render says the budget dropped the block, whether the model called
them outright or a person approved a held one: the kept render carries
the flag, and the manifest recorded at the call carries its mark, no
entry shown and the entries the block held listed among the omitted with
`agentmemory.OmitBlock` for their reason (agentmemory v0.0.10). The
render of an empty store omits nothing, so a drop of one is not told
apart from a block that held nothing after a restart. A session
recorded before agentkit v0.0.7 wrote the drop with no reason on its
entries, and is not read as one.

The product's hooks go into the engine rather than after it
(agentpolicy v0.0.7), so a hook that asks about a call holds its
siblings in the same batch hold as the policy's own asks; chained after
the engine, the siblings ran before anyone answered. The lookup is how
the engine reads a sibling's confinement before the loop hands it that
sibling's call; `Kit.LookupToolFor` reads the union the provider last
returned to the decision's own run, before the policy's filter, through
`agentpolicy.WithToolsFor` (v0.0.11), so two runs off one kit whose
tool lists differ each read their own. All three are engine options, not
config fields. With `WithEngine` the kit can give the engine none of
them, and chains the product's hooks after the engine's with
`agentturn.ChainBeforeToolCall`.

## `Transform`, and the fold

```go
fold := func(ctx context.Context, f compact.Fold) error {
	if r := recorderFor(ctx); r != nil {
		if err := r.Fold(ctx, f); err != nil {
			return err
		}
	}
	observer(ctx, f) // WithFoldObserver
	return nil
}
opts := []compact.Option{compact.WithModel(name), compact.WithOnFold(fold)}
if !reasoning.IsZero() && summariser == model { // WithReasoning's, else WithRequest's; only when the agent's model summarises
	opts = append(opts, compact.WithRequest(func(r *openresponses.Request) { r.Reasoning = reasoning }))
}
opts = append(opts, compact.WithBudget(n))
opts = append(opts, yours...)
if sess != nil { // the session WithSession or WithResumedSession opened
	failed, err := session.CompactOptions(sess)
	if err != nil {
		return err
	}
	opts = append(opts, failed...)
}
t := compact.NewLocal(summariser, opts...) // WithCompaction: model; WithCompactionModel: its model
t = compact.New(compactor, opts...)        // WithCompactor
```

The agent's model name comes first under either, so a
`compact.WithModel` of the product's wins, and a compactor's request
names a model as a summary's does. `compact.WithOnFold` adds a
callback (agentturn v0.0.14), so the kit passes `fold` ahead of the
product's `compact.Option`s, whether or not there is a session, since a
run may bring a recorder on its context, and a `compact.WithOnFold`
among the product's is called after it, once the fold is recorded.
`WithFoldObserver` is the kit's own way to hear of a fold, and it hears
a failed one too, with `Fold.Err` set and no summary: a front that says
the model has forgotten something says it for a fold whose `Err` is
nil.

A summary the agent's model writes is asked under the agent's
reasoning: a thinking model left at its server's default reasons
through the summary's cap and answers no text. So for an agent at
effort low or above the summary is asked at that effort too, and a
thinking model spends part of the summary's cap, half the budget,
reasoning before it writes; the fold succeeds, later and thinner. A
product whose agent thinks passes, in `yours`,
`compact.WithRequest(func(r *openresponses.Request) { r.Reasoning =
openresponses.ReasoningConfig{Effort: openresponses.ReasoningEffortNone} })`,
or the lowest effort its provider accepts; `compact.WithRequest` is one
function, so the product's replaces the kit's. The kit does not pick a
lower effort itself because it does not know the provider's floor:
several reasoning models refuse `none`, and a fold that succeeds
thinner today would then fail every time. Under `WithCompactionModel`
the kit passes no reasoning, since the product chose that model
knowing it and a configuration meant for the agent's model may be
refused by, or wasted on, another; `compact.New` ignores
`compact.WithRequest`, so under `WithCompactor` there is nothing to
pass. `session.CompactOptions` is the last fold on the resumed session's path
that failed, so a restart does not ask again for a summary that failed
before it; a product that records under `WithRecorder` or a recorder on
the context passes it in `yours`.

## `BeforeTurn`

```go
// userMarkOf is the user message items are under: how many user
// messages they hold and a digest of the last one, its role and the JSON
// of its content; false when they hold none.
userMarkOf := func(items openresponses.Items) (userMark, bool) {
	var mark userMark
	var last *openresponses.Message
	for _, item := range items {
		item, _ := agentturn.Unhide(item)
		if m, ok := item.(*openresponses.Message); ok && m.Role == openresponses.RoleUser {
			mark.n++
			last = m
		}
	}
	if last == nil {
		return userMark{}, false
	}
	content, _ := json.Marshal(last.Content)
	sum := sha256.Sum256(append([]byte(string(last.Role)+"\x00"), content...))
	return userMark{n: mark.n, digest: hex.EncodeToString(sum[:])}, true
}

revokeOnUserMessage := func(ctx context.Context, info agentturn.TurnStartInfo) (openresponses.Items, error) {
	mark, marked := userMarkOf(info.Transcript)
	if marked {
		marks[info.RunID] = mark // what a grant made in this run is bound to; a bounded map
	}
	sc := scopeOf(ctx) // the conversation's grant scope; another conversation's grants are not touched
	sctx := agentpolicy.ContextWithGrantScope(ctx, sc)
	stale := false // a grant was made under an earlier user message
	for _, lg := range live[sc] {
		if lg.bound && (mark.n > lg.under.n || mark.digest != lg.under.digest) {
			stale = true
		}
	}
	if !newUserMessage(info.Transcript) && !(marked && stale) {
		return nil, nil
	}
	var ended []liveGrant // the grants in force, by skill, with the rules the engine took
	inEngine := map[string]bool{}
	for _, set := range engine.GrantsFor(sctx) {
		inEngine[set.Source.Name] = true
	}
	for _, name := range granted[sc] { // the sources the grants were made under
		if lg, ok := live[sc][name]; ok && inEngine[name] {
			ended = append(ended, lg) // one the product revoked from the engine itself ended nothing here
		}
		engine.Revoke(sctx, name)
	}
	delete(live, sc)
	// The child agents run under the conversation end with its message:
	// their scopes are the conversation's and a slash and the child's.
	for child := range granted {
		if strings.HasPrefix(child, sc+"/") {
			engine.RevokeScope(agentpolicy.ContextWithGrantScope(ctx, child))
			delete(granted, child)
			delete(live, child)
		}
	}
	for _, lg := range ended {
		endedBy[sc][lg.source] = lg // for refuseEndedGrant, until the skill is read again
	}
	if len(ended) == 0 {
		return nil, nil
	}
	// "The tools these skills granted ended with the user's new message:
	// sysinfo (bash(uptime:*)). ... read the skill again with the skill
	// tool, even if its instructions are already above."
	return openresponses.Items{openresponses.DeveloperText(grantsEndedNote(ended))}, nil
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
loop in it. The note is a fact on the transcript, so the record holds
it: a model with the skill's text above from the message before goes
straight to the tool, and without it the call is refused as an
ordinary ask with no word of the grant that ended; `refuseEndedGrant`
above refuses that call with the same word. It runs on every turn, not only
the first: a follow-up continues the run it joins and a steer arrives
between turns, and either is a new message that must end the last
request's grant. A `Resume`'s first turn ends with the answered calls'
outputs, so an approval keeps the grant. A message ends the
grants of its own conversation, and of the child agents run under it,
and no other conversation's.

The mark is what binds a grant to a message. The skill tool's wrapper,
after `engine.GrantSet`, keeps `live[source] = liveGrant{skill, rules,
under: marks[agentturn.RunIDFromContext(ctx)], bound: ok}`: the mark
the hook kept for the run the read was made in, which is the same ID
`TurnStartInfo.RunID` carries, and no mark for a read made in no run,
which the tail test alone then ends. A replayed grant, `New`'s or
`Kit.RegrantSkills`', is bound to `userMarkOf(session.Transcript(sess))`,
the items `Kit.AgentOptions` seeds the agent with, so the first turn
after a restart does not end what the restart granted again. Not the
path's `ItemEntry` items: a fold's summary stands in for the messages
it folded in the seeded transcript, a user-role message of its own, and
a restart after a fold past the last prompt would read the prompt's
digest as stale and end the grant with "the user's new message" though
none arrived. The tail test alone missed a message another agent's run
received: the kit's agent hands the conversation to another kit's, that
agent takes the user's next message and hands back, and the kit's first
turn opens on the transfer's output. It also missed a steer delivered
before an output, which `TurnStartInfo` does not report. Both are one
more user message, so the count ends them. A compaction ends nothing by
itself: in the run the turn's transcript keeps the message the fold
summarised, so the count falls and the digest holds, and across a
restart the grant is bound to the summary that stands in for it; the
message after either changes the digest.

## What the kit does that no line here covers

Nine things, all outside `agentturn.Config`:

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
  the place they meet. A read the tool refuses with
  `agentskill.ErrSkillChanged`, the skill file gone, renamed or no
  longer parsing since discovery, is reported too, by the name the
  call's `name` argument gave, with `Err` wrapping that error; the
  error goes to the model as before, the earlier read's grant stands,
  and the front answers with `Kit.ReloadSkills`, since the model is told
  to discover the skills again and cannot:

  ```go
  res, err := tool.Execute(ctx, call)
  if errors.Is(err, agentskill.ErrSkillChanged) {
  	var args struct{ Name string `json:"name"` }
  	_ = json.Unmarshal(call.Args, &args)
  	report(agentkit.SkillGrant{Skill: args.Name, Err: err}) // Location from cat.Lookup(args.Name)
  }
  ```
- `WithSession` and `WithResumedSession` open the recorder with
  `session.WithInstructionsParts(kit.PartsFor)`, so its config entries
  carry `instructions_parts` and `instructions_omitted`. `PartsFor` is
  exported and has that option's signature, so a recorder opened
  elsewhere takes it the same way, and
  `agentkit.PartsFrom(&triage, &billing)` asks several kits in turn for
  a recorder they share, reading each variable on every request, since
  the recorder is opened before the kits exist. By hand it is:

  ```go
  partsFor := func(_ context.Context, req openresponses.Request) ([]agentsession.InstructionPart, []agentsession.OmittedPart) {
  	for _, parts := range [][]agentsession.InstructionPart{sent, first} {
  		if agentsession.JoinInstructions(parts) == req.Instructions {
  			return parts, omitted
  		}
  	}
  	return nil, nil // a request some other hook rewrote: recorded as a string
  }
  ```

  `sent` is what the last turn's hook sent and `first` is `New`'s
  guarded render, which the recorder asks about for a run's first
  config entry. Never the layers' own render: that is text a guard kept
  from the model.
- `Kit.AgentOptions()` is `session.AgentOptions(sess, rec.ReadOptions()...)`,
  read in `New`: the transcript at the leaf, `session.Transcript`, with
  the items the filter kept from the model put back; the model each
  reasoning item came from, `agentturn.WithReasoningModels`; and the
  calls pending there, read through a fork's origin, for
  `agentturn.New`. `Kit.Transcript()` is `session.Transcript(sess)`.
- `Kit.LookupToolFor(ctx, name)` reads the union the provider last
  returned to the run the context belongs to, and is what the kit's
  engine is given through `agentpolicy.WithToolsFor`; `Kit.LookupTool(name)`
  reads the kit's own, the union offered outside any run, the fallback
  for a context with no run or a run the kit has no union for: never
  another run's, which under concurrent conversations can be another
  conversation's tools.
- `WithSkillGrants`' grants belong to the conversation that read the
  skill. `GrantSet` and `Revoke` take the grant scope off the context
  they are called with, and the engine consults the unscoped sets and
  those of the call's own scope, so the kit makes each grant under the
  scope its read's context names and puts the call's scope on the
  context it decides under, `decide` above. A run's scope, which is
  `Kit.GrantScope(ctx)`, is the scope its context carries, else its
  conversation:

  ```go
  conversation := func(ctx context.Context) string {
  	if r := agentkit.RecorderFromContext(ctx); r != nil {
  		return r.SessionID()
  	}
  	if sid := session.SessionIDFromContext(ctx); sid != "" {
  		if rec == nil || opened && sid != rec.SessionID() { // not under WithRecorder
  			return sid
  		}
  	}
  	if rec != nil {
  		return rec.SessionID()
  	}
  	return "agentkit:no-session" // not "", which is the unscoped set that decides every call
  }
  ```

  so a front that names its conversations with
  `session.ContextWithSessionID` alone is several conversations, and a
  read in one grants nothing in another. Under `WithSkillGrants` a
  child agent `WithChildAgent` offers runs under a scope of its own, its
  conversation's and a slash and its session ID (the call's ID and a
  number without a recording), `parent+"/"+child` in the `ToolProvider`
  block above, so its parent's skills grant it nothing. The slash makes
  a name that reads well and nothing more: the kit records which scopes
  are a conversation's children when each child's run starts, so a
  grandchild under a child that read no skill is one of them, and the
  conversation's next message, or `Kit.RevokeSkillGrants`, ends exactly
  those, `engine.RevokeScope` for each, never a scope whose name merely
  begins with the conversation's. A child's grants are bound to the
  user-message mark of its parent's turn, so a message the transcript's
  tail test misses ends them as it ends the parent's. Without skill
  grants the kit puts no scope on a child, and a scope the product
  put on the host's context, with its own grants under it, is the
  child's. The kit forgets what it kept of a conversation's grants at
  `Kit.RevokeSkillGrants(ctx)`, which a front calls when the
  conversation ends, as it calls `engine.RevokeScope` for grants of its
  own under the scope. `WithSkillGrantReport` hears every conversation's
  reads and `SkillGrant.Scope` says whose. The revocation under the
  scope records a verdict per source, `revoked the rules granted by
  <source>`, as before, and a replay also ends everything on a
  verdict `revoked the rules granted under <scope>`, which is what
  `Engine.RevokeScope` records.
- With `WithSkillGrants` and a session `New` opened, `New` grants
  again what the session's journal left in force, and
  `Kit.RegrantSkills(ctx, sess)` does the same for a session a front
  resumed itself. It replays the path forward: an `agentskill:read`
  record of a skill's own instructions makes that skill's source live,
  and a verdict whose reason is `revoked the rules granted by <source>`
  ends it. `agentpolicy.Engine.Revoke` records that verdict when it
  removed a rule, and the kit records it for a source whose set held
  none, an untrusted skill's or one whose rules were all refused,
  whether the scope or `Kit.RevokeSkillGrants` revoked it. A record
  whose name the catalogue now gives a skill at another `Location` is
  passed over. One whose `SHA256` is not the digest of
  `agentskill.Skill.Instructions()` now, which agentskill ends with the
  skill's file list and each file's size, or whose `FrontmatterSHA256`
  is not `Skill.FrontmatterSHA256()`, is not the skill the model read:
  it stays live until a revocation after it, and at the end, instead of
  a grant, the report is told with `Replayed` set, `FrontmatterChanged`
  saying which digest moved, and `Err` wrapping
  `agentkit.ErrSkillGrantChanged`, and the session gets a verdict,
  through `observe` on the restart's own context, whose reason is `not
  granted again the tools of skill <name>: the skill changed since it
  was read`. So a skill that writes into its own directory, or whose
  `allowed-tools` were narrowed, or whose edit `Kit.ReloadSkills` picked
  up with no read after, is not granted again after a restart, and the
  front hears it. What is granted again is the rules
  the `granted <rule> by <source>` verdicts written just before the
  read's record name. A live read of a skill with rules that no verdict
  names the source of since its last read or revocation, granted or
  refused, was granted by a kit whose observer wrote nowhere, as a run
  recorded by the front with `session.ContextWithSessionID` and no
  `agentkit.ContextWithRecorder` on its context is; it is reported with
  `Replayed` set and `Err` wrapping `agentkit.ErrSkillGrantUnrecorded`
  and nothing is granted for it. When the record carries the frontmatter's digest,
  the skill's `allowed-tools` are as they were, so those rules are
  granted as the engine recorded them, after `agentpolicy.WithAliases`
  expanded them, each whose specifier one of the skill's rules has;
  otherwise only those spelled as `agentskill.Skill.Rules()` spells
  them, which under an alias is none, and the report says so. A grant
  the engine took wider than recorded is revoked. So a restart may
  narrow a grant and never widens one, nor trusts a read the engine
  withheld; under `WithEngine` the kit records no verdict, and the
  catalogue's rules are granted as they stand. The default source names
  `Skill.FrontmatterSHA256()` as its `Hash`, so every verdict about a
  grant says what it was built from. The replay's `GrantSet`
  runs under a context the kit's observer passes over, so it records no
  verdict, the ones it repeats being on the path already, and
  `WithSkillGrantReport` is told with `SkillGrant.Replayed` set. Under
  `WithSkillGrantScope` the replay also starts after the path's last
  user message, so the scope holds where the journal is silent: an
  engine the product built for `WithEngine`, or a verdict whose write
  failed. A grant lives in the engine, so a restart between a held call
  and its approval lost it. The journal matches a revocation to a read
  by the source name the source function gives now, so one that renames
  its sources loses the match; and a revocation that races a read in
  flight may be journaled before the read's record, which the replay
  then grants. Without the scope, a product that needs either ruled out
  revokes after the restart. `Kit.RegrantSkills` writes the regrant's
  verdicts and a later revocation through the recorder that writes the
  session, the one on `ctx` or the kit's own, and when the kit records
  its engine's verdicts and neither does, it returns an error wrapping
  `agentkit.ErrSkillGrantRecorder` before binding or granting anything;
  under `WithEngine` it binds with no recorder, as before.

- `Kit.ReloadSkills(ctx)` discovers the skills again over the same
  sources and puts the new catalogue behind the skill tool (`currentCat`
  above) and its part in place of the old, guarded as `New` guarded the
  first. It sets the next `Config()`'s `Instructions`; a config already
  handed out keeps its own, which `partsFor` still recognises, and its
  agent takes the new one with `SetConfig` between runs, unless the
  instructions hook re-renders each request, under `WithMemory` or
  `WithGuards`.

None of them changes a field of the config, so none can make the manual
path a different path.
