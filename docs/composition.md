# Composing with other agents

agentturn's composition story is a model, a tool, a peer and an
in-process agent. The kit has options for three of them. For a peer it
has none, and this document is why, and how to do it anyway.

A peer does not need one, which is the point: `Config()` returns a
plain `agentturn.Config`, so everything that takes a config takes the
kit's. An agent the kit assembled and one assembled by hand compose
identically. If a peer needed an option, the kit would have a seam, and
the seam would be the bug.

A child agent has an option for the opposite reason: not because the
composition needs a seam, but because the thing it composes with — the
session recorder — is something only the kit has, since only the kit
opened it.

Both examples below are compiled by CI. The a2a one is the nested
module `examples/a2a`; the child agent ones are `ExampleWithChildAgent`
and `ExampleWithDeferredTools` in the root package's tests.

## A peer over a2a

### Calling one

A remote agent is an `agenttool.Tool`, so it goes in through
`WithTools` and joins the kit's namespace like anything else: a name it
shares with a built-in is the same `Conflict` at `New`, a policy
filters it the same way, and it is in the same stable order.

```go
client, err := a2aclient.NewFromCard(ctx, card)
if err != nil {
	return err
}
peer := toola2a.New(client, card)

kit, err := agentkit.New(ctx,
	agentkit.WithModel(model, "gpt-5"),
	agentkit.WithTools(read, write, peer),
)
```

### Being one

```go
cfg := kit.Config()
card := fronta2a.AgentCard(ctx, cfg, "https://me.example/a2a", "1.0")
exec := fronta2a.New(cfg)
```

The card's name and description are `Config.Name` and
`Config.Description`, which is what `WithName` sets — that is most of
why `WithName` exists — and its skills are the tools the kit unioned
from the product, the skill catalogue, memory and every MCP server.

A front that records each conversation as a session of its own,
`fronta2a.WithRecorderFor` with a recorder per context ID, puts that
recorder on the run's context with `agentkit.ContextWithRecorder` as
well as pointing `ToolRecorder` at it. `ToolRecorder` reaches only what
a tool writes. The policy's and the guards' verdicts, the memory
manifest, a fold and a tool's question are written by hooks the kit
bound at `New`, and those follow the recorder on the context. Without
it they land in the kit's own session, or nowhere, and the
conversation's session holds the calls but not the rules that let them
run.

The store holds each session such a front opens, a lock and the whole
session in memory, until it is released. `RecordEach` releases a
conversation's session when the last task running in it ends, and the
next message resumes it; a front that never releases holds every
session it has served, and no other process can open one.

Two things stay the kit's however the front records: a skill grant,
which is a rule set on the kit's one engine and belongs to one
conversation, so the kit revokes its grants the first time it serves a
second (`agentkit.ErrSkillGrantConversation`); and an MCP server's
connection, dialed once at `New`, whose identity, an OAuth token among
it, every conversation shares. A front that needs either per
conversation or per user builds a kit for each. For an OAuth-protected
server, `mcpclient.StoreTokens` keeps each user's grant in a
`TokenStore` keyed by the endpoint and that user, so the kit built for
them after a restart connects without asking again.

`examples/a2a` wraps both as `Peer` and `Serve`, `RecordEach` is the
per-conversation recording, and `Both` is the round trip: an agent that
calls a peer and is one.

### Why there is no `WithPeer`

Cost. `agentturn/tools/a2a` and `agentturn/front/a2a` are nested
modules on `a2aproject/a2a-go`. Importing either takes a build from
**64 packages to 201**: gRPC, protobuf, genproto, vtprotobuf, `x/net`,
`x/text`. Every consumer of the kit would carry a gRPC stack so that
the ones speaking a2a could save two lines.

That is the same argument as `openresponses/providers`, which is also
reachable through `WithModel` without the kit importing it. A product
that speaks a2a takes the dependency knowingly; one that does not,
does not.

`front/a2a` is additionally a server, which the README's `What it is
not` rules out.

## An in-process child agent

`agentturn/tools/agent` is in the agentturn **root** module, so unlike
a2a it costs the kit nothing to import, and `WithChildAgent` offers a
child as a tool:

```go
kit, err := agentkit.New(ctx,
	agentkit.WithModel(model, "gpt-5"),
	agentkit.WithSession(sessions, agentsession.Header{CWD: cwd}),
	agentkit.WithChildAgent(agentturn.Config{
		Name:        "explore",
		Description: "Delegate a read-only investigation to a sub-agent.",
		Model:       model,
		ModelName:   "gpt-5",
		Tools:       []agenttool.Tool{read, bash},
		MaxTurns:    10,
	}),
)
```

The option earns its place on two bindings: when a session is
configured, the kit passes `childagent.WithObserver(rec.Observe)` to
the child, so the child's own run is recorded live into a session
linked to the parent's, and `childagent.WithRunContext` with the
recorder's `ChildContext`, so the child's tools see that session's ID.
With `WithMemory` configured the same ID goes on the context under
`agentmemory.WithSession`, the key the memory journal reads, so a
memory the child saves names the child's session and not the parent's:

```go
childagent.WithRunContext(func(ctx context.Context, callID string) context.Context {
	ctx = rec.ChildContext(ctx, callID)
	return agentmemory.WithSession(ctx, session.SessionIDFromContext(ctx))
})
```

The recorder does not exist until `New` has opened the session, so
those bindings are not something a product can do in the option list
— which is the whole reason this is an option and a peer is not. The
caller's own `childagent.Option`s are applied after the kit's, so
passing an observer or a run context still wins.

Without a session, or without the recording, a child is an ordinary
tool and needs no option:

```go
kit, err := agentkit.New(ctx,
	agentkit.WithModel(model, "gpt-5"),
	agentkit.WithTools(childagent.New(agentturn.Config{
		Name:        "explore",
		Description: "Delegate a read-only investigation to a sub-agent.",
		Model:       model,
		ModelName:   "gpt-5",
		Tools:       []agenttool.Tool{read, bash},
		MaxTurns:    10,
	})),
)
```

### The general form

`WithChildAgent` is one case of a wider ordering: a tool that cannot be
built until the kit has opened the session, the policy engine or the
skill catalogue, because the kit is what opens them.
`WithDeferredTools` is that ordering for any such tool.

```go
agentkit.WithDeferredTools(func(k *agentkit.Kit) []agenttool.Tool {
	return []agenttool.Tool{newAuditTool(k.Engine(), k.Recorder())}
})
```

The `Kit` it is handed is not finished — `Config()` is not built yet —
but `Recorder`, `Session`, `Engine` and `Catalog` are, which is
everything a tool could need from it. A child that is itself a kit is
built here with `WithRecorder(k.Recorder())`, so it records into the
parent's recorder rather than opening a session of its own. Writing the child agent this way
is exactly what `WithChildAgent` does, so reach for it only when the
tool is not a child.

## What the kit does own about all of this

The tool namespace. Whatever the source, every tool goes through the
same union: stable order, a duplicate name refused at `New` with a
`Conflict` naming both sources, a later duplicate dropped and reported
through `WithToolConflict`, and `agentpolicy`'s filter in front of the
result. A peer, a child, an MCP tool and a built-in are the same kind
of thing to the kit, and that is the only claim it makes about them.
