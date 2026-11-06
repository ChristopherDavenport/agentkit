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

A skill grant follows the conversation. It is a rule set on the kit's
one engine, kept under the grant scope of the conversation that read the
skill, the session of the run's recorder or the session ID the run's
context names, `kit.GrantScope(ctx)`, and it decides no other
conversation's calls. A front that ends a conversation calls
`kit.RevokeSkillGrants(ctx)` with its context, so nothing the
conversation or its child agents opened outlives it, and hears from
its error when the session would not record a revocation, and `WithSkillGrantReport` hears every
conversation's reads with `SkillGrant.Scope` saying whose. An MCP
server's connection stays the kit's: it is dialed once at `New`, and its
identity, an OAuth token among it, every conversation shares. A front
that needs that per conversation or per user builds a kit for each. For
an OAuth-protected server, `mcpclient.StoreTokens` keeps each user's
grant in a `TokenStore` keyed by the endpoint and that user, so the kit
built for them after a restart connects without asking again.

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

The option earns its place on bindings the option list cannot make:
when a session is configured, the kit passes `childagent.WithObserver(rec.Observe)` to
the child, so the child's own run is recorded live into a session
linked to the parent's, and `childagent.WithRunContext` with the
recorder's `ChildContext`, so the child's tools see that session's ID.
With `WithMemory` configured the same ID goes on the context under
`agentmemory.WithSession`, the key the memory journal reads, so a
memory the child saves names the child's session and not the parent's:

```go
childagent.WithRunContext(func(ctx context.Context, callID string) context.Context {
	parent := kit.GrantScope(ctx)
	ctx = rec.ChildContext(ctx, callID)
	ctx = agentmemory.WithSession(ctx, session.SessionIDFromContext(ctx))
	// Whenever the kit has an engine: the child's own grant scope, under
	// its conversation's. The kit also records whose child it is, for
	// kit.RevokeSkillGrants, and under WithSkillGrants which user message
	// its grants are bound to, for the conversation's next message.
	return agentpolicy.ContextWithGrantScope(ctx, parent+"/"+session.SessionIDFromContext(ctx))
})
```

The grant scope is the third binding. A skill a conversation read grants
its tools to that conversation's calls, and a child run from a tool call
inherits its parent's context, so without a scope of its own it would
run under what its parent was granted. Whenever the kit has an engine
it gives the child's context one, the child's session ID under its
conversation's, or the call's ID and a number when there is no
recording, and records it as the conversation's child, with skill
grants or without; a set granted under the conversation's scope, a
person's "allow for this session", does not decide the child's calls.
`kit.RevokeSkillGrants` ends every set under the child's scope with the
conversation's, a front's as well as a skill's, and under
`WithSkillGrants` so does the conversation's next message; a skill the
child reads is the child's alone. The kit forgets a child's scope again
when its call is over, its runs ended and the last not stopped on a
held call, unless the engine holds a set under it or a grandchild's is
linked under it: a front that never ends its conversation does not keep
one per child run, and a set it makes under a scope after that is its
own to revoke with `engine.RevokeScope`.

A child that should act on the conversation's grants is offered with
`WithChildAgent(cfg, agentkit.ShareGrantScope())`: it runs under the
conversation's scope, with the recorder, memory and mark bindings kept,
and no scope of its own is linked. agentpolicy decides a call against
the unscoped sets and its context's scope alone, with no inheritance,
so the share runs both ways: the child takes the person's "allow for
this session" and the skills its parent read, and a skill the child
reads grants its tools to the conversation, the parent included, until
the conversation's next message under `WithSkillGrantScope`. A child
that sees its parent's grants and keeps its own reads to itself needs
scope inheritance in agentpolicy first.

The child's instructions are its config's own string unless it asks
for the kit's parts. `WithChildAgent` takes the kit's choices for the
child, and agentturn's own options through `ChildAgentOptions`;
`ChildParts` composes the
child's prompt and the parts it names with `Kit.ChildConfig`, so a
child that should know the conversation's memory or its AGENTS.md
gets them rendered for each of its requests and recorded as parts,
each with its source and trust:

```go
agentkit.WithChildAgent(exploreCfg,
	agentkit.ChildParts(agentkit.PartMemory, agentsmd.PartID),
	agentkit.ChildAgentOptions(childagent.WithAskParent()))
```

A memory entry the child is shown is then on its path as an
`agentmemory` part labelled `untrusted`, as on the parent's, where a
string the product copied in would carry no label.

The recorder does not exist until `New` has opened the session, so
those bindings are not something a product can do in the option list
— which is the whole reason this is an option and a peer is not. The
caller's own `childagent.Option`s, given through
`agentkit.ChildAgentOptions`, are applied after the kit's, so passing
an observer or a run context still wins.

Without a session, or without the recording, and without an engine,
a child is an ordinary tool and needs no option:

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

## A handoff team

agentturn's `handoff.Team` switches one agent between configurations
and adds a transfer tool to each agent's offer per handoff. When the
agents are kits, the transfers have to meet the kit's tool pipeline as
the agents' own tools do, or a transfer the policy denies is still
offered and a product's wrapper never sees one. The team's `Offer` is
where that happens, once:

```go
team, err := handoff.New(
	handoff.Agent{Config: triage.TeamConfig(), Handoffs: []handoff.To{{Agent: "Billing agent"}}},
	handoff.Agent{Config: billing.TeamConfig()},
)
team.Offer = agentkit.TeamOffer(triage, billing)
```

`TeamConfig` is the kit's config with no tool provider, so the list the
team hands `Offer` is the transfers alone, and `Kit.Offer` unions them
with the kit's own sources, labelled `handoff`, through the filter, the
wrappers, the duplicate check and the policy. A transfer keeps its
name over a kit tool that claims it too, a remote server's tool that
appears at runtime: the kit tool is dropped from that agent's offer and
reported through `WithToolConflict`. Built from `Config()`
instead, the kit's tools would pass the pipeline in the provider and
again in `Offer`. A recorder the kits share takes
`agentkit.PartsFrom(&triage, &billing)`, so each agent's instructions
are recorded as its own parts.

## What the kit does own about all of this

The tool namespace. Whatever the source, every tool goes through the
same union: stable order, a duplicate name refused at `New` with a
`Conflict` naming both sources, a later duplicate dropped and reported
through `WithToolConflict` (a handoff transfer is kept over any of the
kit's), and `agentpolicy`'s filter in front of the
result. A peer, a child, an MCP tool and a built-in are the same kind
of thing to the kit, and that is the only claim it makes about them.
