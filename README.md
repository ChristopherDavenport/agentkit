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
	agentkit.WithOptionalSkills(
		filepath.Join(repoRoot, ".dax", "skills"),
		filepath.Join(home, ".dax", "skills"),
	),
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
session, dialing MCP — and reports it once. The skill directories are
optional because most repositories and most users have none;
`WithSkills` refuses a directory that is not there, for a product whose
skills ship with it. `Config()` is then pure and
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
`session.WithInstructionsParts(kit.PartsFor)`, and the memory block
is a part per entry, so a memory write is recorded as that entry's part
and the summary line rather than the whole block again. `PartsFrom`
does the same for a recorder several kits share, such as two agents
handing a conversation to each other. The input guards run over each part before the join, so a part
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
| `BeforeToolCall` | the policy engine, under the grant scope of the call's conversation, with the scope's refusal of a call only an ended grant allowed and then the product's hooks folded into it (`agentpolicy.WithHooks`) |
| `OutputGuard` | the guards, then the product's |
| `ShouldStopAfterTurn` | the policy's, then the product's |
| `BeforeTurn` | the skill grants' revoke at each new user message under `WithSkillGrantScope`, then the product's |
| `Transform` | the product's, then `compact`, with `WithOnFold` bound to the recorder and `WithFoldObserver`, which hears a failed fold too, with `Fold.Err` set and the transcript sent whole |

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
that says whether the render moved. A front resuming a session seeds
the agent with `kit.AgentOptions()`, `session.AgentOptions`: the
transcript at the leaf with the items the filter kept from the model,
the model each reasoning item came from, so a switch of models after a
restart does not send one provider another's reasoning, and the calls
pending there, so a call held before a restart can still be approved.
Under `WithSkillGrants`, `New` has already granted again what the
session's skill reads granted, so the task goes on under them, and
under `WithCompaction` the fold backs off from the last one that
failed on the session's path:

```go
agent := agentturn.New(kit.Config(), kit.AgentOptions()...)
```

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
A grant belongs to the conversation that read the skill: the engine
keeps it under that conversation's grant scope, its session ID
(`agentpolicy.ContextWithGrantScope`, `kit.GrantScope(ctx)`), and
decides only that conversation's calls, so one kit serves any number of
conversations and a child agent runs under a scope of its own.
`WithSkillGrantScope` ends each grant when the user's next message in
its conversation arrives, whichever agent's run received it, as Claude
Code clears `allowed-tools` at the next message, and
`kit.RevokeSkillGrants(ctx)` ends a conversation's when a front says; a
skill read again is granted again, and under the scope the model is told
which grants a message ended and that a read restores them, and a call
only an ended grant allowed is refused, inside the engine, naming the
skill to read again. The kit refuses when `Engine.Would`, the engine's
own side-effect-free decision, says it would ask, so the policy, the
grants in force, the tool's confinement and every agentpolicy option
and hook are read as the engine reads them.
`kit.ReloadSkills(ctx)` discovers the skills again, for an agent that
writes a skill and uses it in the same conversation.

## Composing with other agents

agentturn's composition story is a model, a tool, a peer and an
in-process agent. The kit has options for three of them and, by
design, none for a peer. `Config()` returns a plain `agentturn.Config`,
so a remote peer is an ordinary tool through `WithTools`, and serving
the agent as a peer is `fronta2a.New(kit.Config())`.

Recording what a served agent does is the one place the kit adds a
seam, `agentkit.ContextWithRecorder`. A front that records each
conversation as its own session opens a recorder per conversation in
`fronta2a.WithRecorderFor` and prompts each run with
`agentkit.ContextWithRecorder(ctx, rec)`, so the verdicts, the memory
manifest and the folds the kit's hooks write land in that
conversation's session and not in the kit's. `RecordEach` in
[`examples/a2a`](examples/a2a/a2a.go) is that function, releasing each
session when its conversation's last task ends; copy it. An engine a
product built for `WithEngine` records its verdicts there only if its
own observer reads `agentkit.RecorderFromContext`. A skill grant follows
the conversation, since the engine keeps it under that conversation's
grant scope and decides no other's calls; an MCP server's connection
stays the kit's, and every conversation calls it as whoever authorized
the connection. A conversation is the session of the recorder on the
run's context, or the session `session.ContextWithSessionID` names
there, or a scope the front names with `agentpolicy.ContextWithGrantScope`,
so a front that names its conversations by any of them is several
conversations to the kit; one that names them none, as
`fronta2a.New(kit.Config())` alone does, is one. A front ends a
conversation with `kit.RevokeSkillGrants(ctx)`. A front that needs an
MCP connection per conversation or per user builds a kit for each, and
keeps each user's OAuth grant across restarts with
`mcpclient.StoreTokens`, keyed by the server and that user
(`WithMCPTransport` shows the wiring).

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
| `openresponses` | v0.0.14 |
| `agenttool`, `agenttool/mcpclient` | v0.0.15 |
| `agentturn`, `agentturn/session` | v0.0.16 |
| `agentsession` | v0.0.20 |
| `agentsmd` | v0.0.2 |
| `agentskill` | v0.0.11 |
| `agentmemory` | v0.0.10 |
| `agentpolicy` | v0.0.11 |

Every sibling is required at a released version with no `replace`, and
`make no-replace` enforces it: the kit is the module that proves the
released libraries compose, so it has to build against the versions a
consumer would fetch. `TestTheREADMEVersionTableIsGoMod` holds the
table above to `go.mod`, so a bump that forgets the table fails.
