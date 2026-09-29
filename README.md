# agentkit

Assembly for the nine libraries: one call that turns a product's
choices into an `agentturn.Config`, and the one module in the workspace
that imports every other. It owns the composition no single library can
own and nothing else.

```go
kit, err := agentkit.New(ctx,
	agentkit.WithModel(model, "gpt-5"),
	agentkit.WithInstructions("Be brief."),
	agentkit.WithAgentsMD(cwd, agentsmd.Options{Root: repoRoot}),
	agentkit.WithSkills(".dex/skills", filepath.Join(home, ".dex", "skills")),
	agentkit.WithMemory(store, "user", "project"),
	agentkit.WithPolicy(policy, matchers),
	agentkit.WithTools(read, write, edit, bash),
	agentkit.WithMCP("some-server --stdio"),
	agentkit.WithSession(sessions, agentsession.Header{CWD: cwd}),
	agentkit.WithCompaction(60_000),
)
if err != nil {
	return err
}
defer kit.Close()

for _, o := range kit.Omitted() {
	log.Printf("not given to the model: %s (%s)", o.What, o.Reason)
}

agent := agentturn.New(kit.Config())
defer kit.Attach(agent)()
```

`New` does the work that can fail — discovery, validation, opening the
session, dialing MCP — and reports it once. `Config()` is then pure and
can be called per run. `Close()` releases what `New` opened.

`Attach` is the one step a `Config` cannot carry. The recorder has to
subscribe to the agent, and the agent does not exist until after
`Config()` — so `Attach` subscribes it and returns the unsubscribe,
which is why the call is doubled: `kit.Attach(agent)` runs at the
`defer` and subscribes, and the function it returns runs at scope exit
and unsubscribes. A session configured but never attached records
nothing and says nothing about it, so attach where the agent is built.
Without a session it is a no-op, so the line does not need a branch
around it.

Every method, and every config `Config()` returns, is safe to use from
any goroutine. The configs are not independent of each other: concurrent
runs off one kit share its memory state, which is the right sharing for
two runs of one agent and the wrong sharing for two agents. Give each
agent its own kit; the `Kit` doc says exactly what is shared.

## The rule

`Config()` returns a plain `agentturn.Config`. Every field it sets a
product could have set by hand, with the same values, by calling the
same exported functions. There are no private seams, no wrapper types a
caller cannot construct, and no behaviour that exists only when the kit
assembled it.

That rule is the whole design. It means the easy path and the manual
path are the same path, so the kit never becomes load-bearing: a
product that outgrows one of its decisions replaces that decision
without leaving the kit, and a product that outgrows all of them
deletes the import and keeps working. A kit that hid one seam would be
a framework, and the stack already has a better answer than a
framework.

The test of the rule is mechanical. [`docs/manual.md`](docs/manual.md)
names, for every field of `agentturn.Config` the kit sets, the exported
call a product would write instead;
`TestEveryFieldTheKitSetsIsDocumented` fails if the kit sets a field
that document does not name, and `TestTheManualPathIsTheSamePath`
writes the manual side out for a full configuration and compares it to
the kit's.

## What it owns

Three things, each because no library can own it without importing its
siblings.

### 1. Instruction assembly

`agentsmd.Render`, `agentskill.Catalog.Prompt` + `Usage`, and
`agentmemory.Render` + `Usage` each produce a block of instruction
text; each `Usage` paragraph is joined with its block exactly when the
tool it describes is offered. Joining them with `"\n\n"` is the whole composition, and three
problems follow from nobody owning the join: the order is a policy
nobody states, the session's `instructions_parts` record has a shape
and no writer, and every layer reports its omissions to nobody.

So the kit builds `[]Part` — which is
`agentsession.InstructionPart`, not a type of its own — in a stated
order, under a total budget, and returns both the joined text for
`Config.Instructions` and the parts and the omissions for a recorder.
A session the kit opens takes them through
`session.WithInstructionsParts(kit.PartsFor)`, so a memory write is
recorded as a change to the memory part rather than the whole prompt
again. The input guards run over each part before the join, so a part
`guard.Redact` rewrote is recorded as it was sent.

| # | part id | source | why here |
|---|---|---|---|
| 1 | `product` | the product's own prompt | the frame everything else refines |
| 2 | `skills` | `agentskill` | a catalogue, not an instruction |
| 3 | `memory` | `agentmemory` | what the agent knows, before what the repo says |
| 4 | `agentsmd` | `agentsmd` | the repository has the last word |

Every position is a decision a product reverses with `WithOrder`, and
every one of them is argued in
[`docs/ordering.md`](docs/ordering.md) rather than asserted in code.

### 2. Hook composition

`agentturn.Config` has one field per hook and more than one layer wants
each; assigning one twice keeps the second and loses the first with no
error and no sign. agentturn's `Chain*` functions exist because that
contest was silent, and the kit is where they get called, in one order:

| field | order |
|---|---|
| `BeforeModelCall` | memory re-render, then the guards over each part and over the whole request, then the product's |
| `BeforeToolCall` | the policy engine, then the product's |
| `OutputGuard` | the guards, then the product's |
| `ShouldStopAfterTurn` | the policy's, then the product's |
| `BeforeTurn` | the skill grants' revoke under `WithSkillGrantScope`, then the product's |
| `Transform` | the product's, then `compact`, with `WithOnFold` bound to the recorder when there is one |

With a session, the engine's verdicts and the guards' are recorded
under `agentpolicy.VerdictNS`, so the record says which rule held a
call and not only that the policy did; `WithVerdictObserver` sees the
same verdicts.

### 3. The tool set

A run's tools come from the product, `agentskill.Catalog.Tool()`,
`agentmemory.Tools`, `mcpclient`, and `agentturn/tools/agent` for a
child. Five sources, one namespace, and nothing below the kit detects a
collision: two tools with one name reach the model as two entries and
the loop dispatches whichever the set returns first.

The kit unions them in a stable order — product tools, then child
agents and other deferred sources, then skills, then memory, then MCP,
then any provider the product gave — refuses a duplicate present at
`New` with a `Conflict` naming both sources, drops the later of one
that appears at turn time and tells `WithToolConflict`, and puts
`agentpolicy`'s `ToolProvider` in front of the result so a tool a bare
deny rule removes is never offered. Every source carries a label, so a
conflict says which two to fix.

`WithToolFilter` is the other thing a union can do that its parts
cannot: it sees each tool with the label of the source that produced
it, which is how a product takes five tools from a server offering
forty. It runs before the duplicate check, so dropping one of two tools
claiming a name resolves the collision rather than reporting it.
`WithToolWrap` is the same seam for replacing a tool, a replay or a
logger over every tool, inside the kit's own wrapper, and `Kit.Tools()`
lists each tool with its label once, which is how a product names the
libraries' tools to a policy whose default asks. The kit does not allow
them itself: whether a library's tool runs unasked is a decision.

## What it also exposes

A front needs things a `Config` cannot carry: `kit.Engine()` for
`Deferred` and `Release`, `kit.Recorder()` and `kit.SessionID()`,
`kit.Catalog()`, `kit.Tools()`, and `kit.MemoryManifest()` for the hash
that says whether the render moved.

A front that asks a person about a held call says so when it answers,
or the record cannot tell the approval from one a script gave:

```go
answers, err := kit.Engine().Release(ctx, end,
	agentturn.Approve(callID).WithBy(agentpolicy.ByHuman))
if err != nil {
	return err
}
end, err = agent.Resume(ctx, answers...)
```

A host that wants its own run's memory writes to name its session puts
the ID on the context it prompts with,
`agentmemory.WithSession(ctx, kit.SessionID())`; the kit cannot, since
that context is the host's. A child agent's run is the kit's, and
`WithChildAgent` does it there.

A kit built where a recorder already exists, under an evaluation
runner or inside a parent's `WithDeferredTools`, takes it with
`WithRecorder(rec)`: everything the kit binds to a session it binds to
that recorder, and it opens and attaches nothing.

`WithSkillGrants` is the one place a skill's `allowed-tools` meets a
policy: reading a skill grants its rules to the engine through
`agentpolicy.Engine.GrantSet`. It is off by default and, when on,
attributes the grant to an **untrusted** source unless the caller's own
source function says otherwise, so a skill widens what the agent may do
only when the product has said it trusts the tree the skill came from.
`WithSkillGrantScope` ends each grant when the next run starts, as
Claude Code clears `allowed-tools` at the next message, and
`kit.RevokeSkillGrants(ctx)` ends them when a front says; a skill read
again is granted again.

## Composing with other agents

agentturn's composition story is a model, a tool, a peer and an
in-process agent. The kit has options for three of them and, by
design, none for a peer — because a peer does not need one. `Config()`
returns a plain `agentturn.Config`, so a remote peer is an ordinary
tool through `WithTools`, and serving the agent as a peer is
`fronta2a.New(kit.Config())`. If a peer needed an option, the kit
would have a seam.

`WithChildAgent` is the in-process one, and it exists for a different
reason than a seam: it binds the child's observer and run context to
the session recorder, which only the kit has, because only the kit
opened the session, so the child's run is recorded into its own linked
session and what its tools write, a memory among them, names that
session.

[`docs/composition.md`](docs/composition.md) has both directions, and
the child-agent case where the recorder the kit made has to reach the
child. The a2a code is the nested module
[`examples/a2a`](examples/a2a), compiled by CI: the a2a packages pull
in a gRPC stack that would take a build from 64 packages to 201, so
they stay out of the root and out of every consumer that never speaks
a2a.

## What it is not

- Not a UI, a server, a launcher or a deployment tool. `agenttui` is
  the terminal client and it does not import this module.
- Not a new tool type, model type, store type or hook type. Everything
  it hands to the loop came from a library's exported constructor.
- Not orchestration. The composition story stays agentturn's: a model,
  a tool, a peer, an in-process agent.
- Not a place for defaults that hide a decision. An option that changes
  behaviour is argued in `docs/` or it does not exist.

## Modules

| library | version |
|---|---|
| `openresponses` | v0.0.12 |
| `agenttool`, `agenttool/mcpclient` | v0.0.9 |
| `agentturn`, `agentturn/session` | v0.0.10 |
| `agentsession` | v0.0.9 |
| `agentsmd` | v0.0.2 |
| `agentskill` | v0.0.6 |
| `agentmemory` | v0.0.5 |
| `agentpolicy` | v0.0.5 |

Every sibling is required at a released version with no `replace`, and
`make no-replace` enforces it: the kit is the module that proves the
released libraries compose, so it has to build against the versions a
consumer would fetch. `TestTheREADMEVersionTableIsGoMod` holds the
table above to `go.mod`, so a bump that forgets the table fails.
