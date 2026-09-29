package agentkit

import (
	"context"
	"fmt"

	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentpolicy/guard"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agentsmd"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agenttool/mcpclient"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/agentturn/session"
	childagent "github.com/ChristopherDavenport/agentturn/tools/agent"
	"github.com/ChristopherDavenport/openresponses"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Option configures [New]. An option records a choice; every choice is
// applied in New, in the order the package documents rather than the
// order the options were given, so which option comes before which
// never changes the result.
//
// Repeating one option is a different matter, and the option says which
// it does. Most replace: a second [WithModel] or [WithInstructions]
// wins. The ones that accumulate keep the order they were given, and
// for the tool sources that order is load-bearing — [WithSkills]
// decides which of two skills of one name shadows the other, and
// [WithTools], [WithMCP] and [WithToolProvider] decide which side of a
// [Conflict] is kept.
type Option func(*settings)

// settings is every choice New was given, before any of it is acted on.
type settings struct {
	name        string
	description string

	model        agentturn.Model
	modelName    string
	request      openresponses.Request
	requestExtra map[string]any
	reasoning    openresponses.ReasoningConfig
	text         openresponses.TextConfig
	maxTurns     int
	retry        agentturn.Retry
	toolExec     agentturn.ExecutionMode
	maxParallel  int

	instructions string
	budget       int64
	order        []string

	agentsMDPath string
	agentsMD     agentsmd.Options
	agentsMDSet  bool

	skillSources []agentskill.Source
	// skillToolWith and skillToolWithout record the two options
	// separately, rather than as one flag and its value, so that asking
	// for the tool and withholding it is a contradiction New can refuse
	// instead of a silent last-one-wins.
	skillDirs        []string
	skillToolWith    bool
	skillToolWithout bool
	skillToolOpt     []agentskill.ToolOption
	skillGrants      bool
	skillGrantScope  bool
	skillSource      func(*agentskill.Skill) agentpolicy.Source
	skillGrant       func(SkillGrant)

	memStore  agentmemory.Store
	memScopes []agentmemory.Scope
	memRender []agentmemory.RenderOption
	memTools  []agentmemory.ToolOption

	tools      []agenttool.Tool
	deferTools []deferred
	mcp        []mcpDial
	conflict   func(Conflict)
	toolFilter func(source string, t agenttool.Tool) bool
	toolWrap   func(source string, t agenttool.Tool) agenttool.Tool

	engine      *agentpolicy.Engine
	policy      agentpolicy.Policy
	policySet   bool
	matchers    map[string]agentpolicy.ToolMatcher
	engineOpts  []agentpolicy.Option
	guards      []guard.Guard
	verdicts    func(context.Context, agentpolicy.Verdict)
	toolProvide []func(context.Context) []agenttool.Tool

	sessionStore  agentsession.Store
	sessionHeader agentsession.Header
	sessionID     string
	sessionResume bool
	sessionOpts   []session.Option
	sessionSet    bool
	recorder      *session.Recorder

	compactor    compact.Compactor
	compactModel openresponses.Streamer
	compactOpts  []compact.Option
	compactSet   bool

	beforeTurn      []func(context.Context, agentturn.TurnStartInfo) (openresponses.Items, error)
	beforeModelCall []func(context.Context, *openresponses.Request) error
	beforeToolCall  []func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error)
	afterToolCall   func(context.Context, agentturn.ToolResultInfo) (*agentturn.ToolOverride, error)
	elicitor        agenttool.Elicitor
	elicitBy        string
	outputGuard     []func(context.Context, agentturn.OutputInfo) (*openresponses.Message, error)
	shouldStop      []func(context.Context, agentturn.TurnInfo) (bool, error)
	transform       func(context.Context, agentturn.Transcript) (agentturn.Transcript, error)
	filter          func(agentturn.Transcript) agentturn.Transcript
}

// deferred is one tool source that cannot be built until the kit has
// built the rest of itself. kind and name make the label a [Conflict]
// blames: the option it came from, its position among the deferred
// sources, and, for a child agent, which agent.
type deferred struct {
	kind string
	name string
	fn   func(*Kit) []agenttool.Tool
}

func (d deferred) label(i int) string {
	s := fmt.Sprintf("%s #%d", d.kind, i+1)
	if d.name != "" {
		s += " " + d.name
	}
	return s
}

// mcpDial is one MCP server to connect to.
type mcpDial struct {
	command   string
	transport sdk.Transport
	opts      []mcpclient.Option
}

// WithName sets [agentturn.Config.Name] and Description: how the agent
// names itself when it is composed as a tool, an MCP server or an agent
// card.
func WithName(name, description string) Option {
	return func(s *settings) { s.name, s.description = name, description }
}

// WithModel sets the model and the model name of every request. It is
// the one required option: [New] refuses a kit without a model, as the
// loop refuses a config without one.
func WithModel(m agentturn.Model, name string) Option {
	return func(s *settings) { s.model, s.modelName = m, name }
}

// WithRequest sets the base request every call is built from:
// tool_choice, parallel_tool_calls, max_output_tokens, temperature and
// the rest. The kit owns none of its members and copies it through.
func WithRequest(req openresponses.Request) Option {
	return func(s *settings) { s.request = req }
}

// WithRequestExtra sets members the request type does not name, merged
// over [WithRequest]'s own Extra on every call: a vendor's field, a
// preview flag, anything the endpoint takes and openresponses does not
// model.
func WithRequestExtra(extra map[string]any) Option {
	return func(s *settings) { s.requestExtra = extra }
}

// WithReasoning sets the reasoning effort and summary of every request.
func WithReasoning(r openresponses.ReasoningConfig) Option {
	return func(s *settings) { s.reasoning = r }
}

// WithText sets the output format and verbosity of every request.
func WithText(t openresponses.TextConfig) Option {
	return func(s *settings) { s.text = t }
}

// WithMaxTurns stops a run after n turns. Zero means no limit.
func WithMaxTurns(n int) Option {
	return func(s *settings) { s.maxTurns = n }
}

// WithRetry sends a failed model call again, under the policy r. The
// zero [agentturn.Retry], which is the default, retries nothing; the
// loop's own [agentturn.DefaultRetryable] and [agentturn.DefaultBackoff]
// apply to the members r leaves nil. Only an attempt that has not
// committed is retried, which is agentturn's rule and not the kit's.
func WithRetry(r agentturn.Retry) Option {
	return func(s *settings) { s.retry = r }
}

// WithToolExecution selects parallel batches, the default, or
// sequential ones. A tool that declares itself agenttool.Sequential
// runs alone whatever this says.
func WithToolExecution(mode agentturn.ExecutionMode) Option {
	return func(s *settings) { s.toolExec = mode }
}

// WithMaxParallelTools bounds a parallel batch to n calls at a time.
// Zero, the default, means agenttool.DefaultMaxParallel.
func WithMaxParallelTools(n int) Option {
	return func(s *settings) { s.maxParallel = n }
}

// WithInstructions sets the product's own prompt: the first
// instructions part, [PartProduct], the frame every other part refines.
func WithInstructions(text string) Option {
	return func(s *settings) { s.instructions = text }
}

// WithAgentsMD reads the AGENTS.md chain that applies at path and
// renders it as the [agentsmd.PartID] part. opts are
// [agentsmd.Options] verbatim: the names to look for, the root the walk
// stops after, the explicit Extra files, and the per-file and total
// byte bounds.
//
// A budget set here is the layer's own and applies whatever
// [WithInstructionBudget] says. When both are set the smaller of the
// two binds; see docs/ordering.md.
func WithAgentsMD(path string, opts agentsmd.Options) Option {
	return func(s *settings) {
		s.agentsMDPath, s.agentsMD, s.agentsMDSet = path, opts, true
	}
}

// WithSkills discovers skills in the given directories, in order, and
// renders the catalogue as the [PartSkills] part. A directory that does
// not exist is an error from [New], as it is from [agentskill.Dir]:
// a misspelled skill path silently offering no skills is the failure
// this refuses.
//
// Unless [WithoutSkillTool] says otherwise, the catalogue's tool is
// offered and [agentskill.Catalog.Usage] is appended to the part, which
// is the pairing agentskill asks for: the usage paragraph tells the
// model to reach the listed skills through a tool, so it is written
// only when that tool is there.
func WithSkills(dirs ...string) Option {
	return func(s *settings) { s.skillDirs = append(s.skillDirs, dirs...) }
}

// WithSkillSources adds skill sources that are not local directories:
// an embed.FS, a zip, an adapter over a remote store. They are
// discovered after the directories [WithSkills] named, so a directory
// shadows a source given here by the same name.
func WithSkillSources(sources ...agentskill.Source) Option {
	return func(s *settings) { s.skillSources = append(s.skillSources, sources...) }
}

// WithSkillTool offers the catalogue's tool with the given options. It
// is offered by default when any skill source is configured; this is
// how a caller changes its options without changing that.
func WithSkillTool(opts ...agentskill.ToolOption) Option {
	return func(s *settings) {
		s.skillToolWith = true
		s.skillToolOpt = append(s.skillToolOpt, opts...)
	}
}

// WithoutSkillTool renders the catalogue into the prompt without
// offering the tool that reads a skill. The usage paragraph is then not
// appended either, since it describes a tool the model does not have.
// A product that serves skill bodies its own way wants this.
func WithoutSkillTool() Option {
	return func(s *settings) { s.skillToolWithout = true }
}

// WithSkillGrants grants a skill's allowed-tools to the policy engine
// each time the model reads that skill. source builds the
// [agentpolicy.Source] the grant is attributed to; nil means a source
// named "agentskill:" and the skill's name, with the skill's location
// as its path, which is untrusted and therefore contributes its deny
// and ask rules alone.
//
// A grant lasts until something revokes it. [WithSkillGrantScope]
// revokes every skill's grant when a run starts, which is the lifetime
// Claude Code gives allowed-tools; without it a grant lasts the life of
// the engine unless the product calls [Kit.RevokeSkillGrants]. A skill
// read again is granted again, and reported again: a repeated
// [agentpolicy.Engine.GrantSet] under one source name replaces the
// set, so a read after a revoke puts the grant back. Two skills the
// source function gives one name share one grant, and the later read
// replaces the earlier's rules.
//
// Untrusted is the default because a skill is a file someone else
// wrote. A product that trusts the tree its skills came from returns a
// Source with Trusted set, and then a skill's allowed-tools widens what
// the agent may do the moment the model reads it. That is the whole
// point of allowed-tools and it is also a privilege escalation, so the
// kit will not assume it.
//
// It has no effect without a policy engine, and none without skills,
// so a product may add it unconditionally and the two behind flags.
// [WithoutSkillTool] is the one combination [New] refuses: a grant
// happens when the model reads a skill, which it does through the
// catalogue's tool, so withholding that tool withholds every grant.
func WithSkillGrants(source func(*agentskill.Skill) agentpolicy.Source) Option {
	return func(s *settings) { s.skillGrants, s.skillSource = true, source }
}

// WithSkillGrantScope revokes every grant a skill's read made when a
// run starts, so a grant lasts the run that read the skill, as Claude
// Code clears allowed-tools when the next message arrives: a later run
// that wants the tools reads the skill again. It is a
// [agentturn.Config.BeforeTurn] hook that calls [Kit.RevokeSkillGrants]
// on turn 1, ahead of the product's own BeforeTurn.
//
// Only the sources the kit granted are revoked; a product's own
// [agentpolicy.Engine.GrantSet] calls are left alone. Every run on the
// engine revokes them, so two agents sharing one engine share one
// scope. It has no effect without [WithSkillGrants].
func WithSkillGrantScope() Option {
	return func(s *settings) { s.skillGrantScope = true }
}

// WithSkillGrantReport is told what the engine did with each skill's
// allowed-tools: what it granted, what it refused and why, and a
// skill whose allowed-tools would not parse. A grant widens what the
// agent may do, so a front that shows the user the policy in force
// wants to see it happen.
func WithSkillGrantReport(fn func(SkillGrant)) Option {
	return func(s *settings) { s.skillGrant = fn }
}

// WithMemory renders the memory block for the given scopes, in order,
// as the [PartMemory] part, offers the memory tools, and re-renders the
// block before every model call so the model sees the freshest state.
// [agentmemory.Usage], the paragraph that tells the model what the
// block is and which tool makes which change, follows the block in the
// same part, since the tools are offered whenever the block is.
func WithMemory(store agentmemory.Store, scopes ...agentmemory.Scope) Option {
	return func(s *settings) { s.memStore, s.memScopes = store, scopes }
}

// WithMemoryRender passes options to [agentmemory.Render], both for the
// first render and for the per-turn one.
func WithMemoryRender(opts ...agentmemory.RenderOption) Option {
	return func(s *settings) { s.memRender = append(s.memRender, opts...) }
}

// WithMemoryTools passes options to [agentmemory.Tools].
func WithMemoryTools(opts ...agentmemory.ToolOption) Option {
	return func(s *settings) { s.memTools = append(s.memTools, opts...) }
}

// WithInstructionBudget bounds the joined instructions to n bytes. The
// parts that can be bounded are bounded, the rest are measured, and
// [New] fails when the parts that cannot be bounded already exceed n
// rather than sending a prompt the caller asked not to send. Zero, the
// default, leaves each layer its own bound. How the budget is spent is
// argued in docs/ordering.md.
func WithInstructionBudget(n int64) Option {
	return func(s *settings) { s.budget = n }
}

// WithOrder replaces the order the parts are joined in. ids are part
// identifiers — [PartProduct], [PartSkills], [PartMemory],
// [agentsmd.PartID] — and every configured part must appear exactly
// once, or [New] fails. The default order and the argument for it are
// in docs/ordering.md.
func WithOrder(ids ...string) Option {
	return func(s *settings) { s.order = append([]string(nil), ids...) }
}

// WithTools adds the product's own tools. They come first in the tool
// set, so a listing is reproducible.
func WithTools(ts ...agenttool.Tool) Option {
	return func(s *settings) { s.tools = append(s.tools, ts...) }
}

// WithMCP connects to an MCP server run as a subprocess: command is
// split on whitespace into the program and its arguments. A server that
// needs an environment, a working directory or an argument with a space
// in it is dialed with [WithMCPTransport] instead.
func WithMCP(command string, opts ...mcpclient.Option) Option {
	return func(s *settings) {
		s.mcp = append(s.mcp, mcpDial{command: command, opts: opts})
	}
}

// WithMCPTransport connects to an MCP server over the given transport.
func WithMCPTransport(t sdk.Transport, opts ...mcpclient.Option) Option {
	return func(s *settings) {
		s.mcp = append(s.mcp, mcpDial{transport: t, opts: opts})
	}
}

// WithToolConflict is called when two tools claim one name at turn
// time, which is the only point at which a remote's tool list can
// collide with a name it did not collide with when [New] checked. The
// later tool is dropped, the earlier is kept, and fn is told. A
// conflict present at New is an error from New instead.
func WithToolConflict(fn func(Conflict)) Option {
	return func(s *settings) { s.conflict = fn }
}

// WithToolProvider supplies tools from a source the kit does not know
// about, called once per turn. Its tools come last, after the MCP
// servers', and a name it repeats is a [Conflict] resolved the same
// way. Several providers are each their own source, in the order given,
// so a collision between two of them says which is which.
func WithToolProvider(fn func(context.Context) []agenttool.Tool) Option {
	return func(s *settings) { s.toolProvide = append(s.toolProvide, fn) }
}

// WithToolFilter drops tools from the union before the model is offered
// them: fn is called once per tool per turn, with the label of the
// source that produced it — "WithTools", "WithSkills", "WithMemory",
// "mcp:#1 some-server" — and keeps the tool when it returns true.
//
// It is how a product takes five tools from a server that offers forty.
// The union is the one place every source meets, and the label is
// knowledge only the kit has, which is why the filter is here and not
// in the libraries the tools came from. A policy is the other way to
// withhold a tool and a better one where it fits: a deny rule is
// recorded, explicable and the same rule that stops the call.
//
// It runs before the duplicate check, so filtering one of two tools
// that claim a name resolves the conflict rather than reporting it.
func WithToolFilter(fn func(source string, t agenttool.Tool) bool) Option {
	return func(s *settings) { s.toolFilter = fn }
}

// WithToolWrap replaces tools in the union with what fn returns: fn is
// given each tool with the label of its source, the same label
// [WithToolFilter] is given, and returns the tool to offer in its
// place, or the tool itself to leave it alone. It is how a middleware
// reaches every tool, a replay, a timer or a logger, from inside the
// kit rather than around the provider.
//
// It runs before the kit's own wrapper, so the skill tool's grant is
// made on what fn's tool returned: a replay that serves a skill read
// from a recording still grants. It runs after the filter, which sees
// the tool its source produced. A fixed source's tools, those from
// [WithTools], [WithChildAgent], [WithDeferredTools], [WithSkills] and
// [WithMemory], are wrapped once, in [New]; an MCP server's and a
// [WithToolProvider]'s are wrapped each turn, since their lists are
// fetched each turn. A wrapper that keeps a tool's properties uses
// [agenttool.Wrap]. The duplicate check, [Kit.Tools] and the policy
// read the name of the tool fn returned, and a nil return drops the
// tool.
func WithToolWrap(fn func(source string, t agenttool.Tool) agenttool.Tool) Option {
	return func(s *settings) { s.toolWrap = fn }
}

// WithDeferredTools adds tools that cannot be built until the kit has
// built the rest of itself: fn is called once, inside [New], after the
// session is open and the policy engine exists, and its tools join the
// set where [WithTools] puts them.
//
// The case it was written for, a child agent whose own runs are
// recorded, is [WithChildAgent] now; this is the general form, for a
// tool that wants the policy engine, the skill catalogue or the session
// itself. Without it such a tool has to be built lazily inside a
// per-turn provider, which is a cache and a nil check standing in for
// an ordering the kit already knows.
//
// The Kit it is given is not yet finished: [Kit.Config] is not built.
// [Kit.Recorder], [Kit.Session], [Kit.Engine] and [Kit.Catalog] are.
func WithDeferredTools(fn func(*Kit) []agenttool.Tool) Option {
	return func(s *settings) {
		s.deferTools = append(s.deferTools, deferred{kind: "WithDeferredTools", fn: fn})
	}
}

// WithChildAgent offers cfg as a tool, through agentturn/tools/agent,
// so the model can delegate a piece of work to an agent of its own. The
// tool is named [agentturn.Config.Name] unless childagent.WithToolName
// says otherwise.
//
// When a session is configured the child's run is recorded into it,
// live and linked to the parent's, because the kit binds
// childagent.WithObserver to the recorder, and the child runs under
// the recorder's [session.Recorder.ChildContext] through
// childagent.WithRunContext, so what the child's tools attribute to a
// session names the child's. With [WithMemory] as well, the same
// context carries the child's session ID under
// [agentmemory.WithSession], which is the key the memory journal
// reads: a memory the child saves names the child's session. Those
// bindings are the reason this option exists rather than the child
// going in through [WithTools]: the recorder does not exist until [New]
// has opened the session, so a product doing this by hand reaches for
// [WithDeferredTools] and a nil check. The caller's own options are
// applied after the kit's, so passing childagent.WithObserver or
// childagent.WithRunContext here still wins.
//
// The kit cannot do the same for the parent's own run, whose context
// is the host's: a host that wants the parent's memory writes to name
// its session prompts with agentmemory.WithSession(ctx, kit.SessionID()).
//
// The child is an ordinary [agenttool.Tool] in every other way: it
// joins the set where [WithTools] puts it, the policy filters it, and a
// name it shares with another tool is a [Conflict].
func WithChildAgent(cfg agentturn.Config, opts ...childagent.Option) Option {
	return func(s *settings) {
		s.deferTools = append(s.deferTools, deferred{
			kind: "WithChildAgent",
			name: cfg.Name,
			fn: func(k *Kit) []agenttool.Tool {
				// A fresh slice: appending to opts would grow the
				// caller's array and make two child agents share it.
				all := make([]childagent.Option, 0, len(opts)+2)
				if rec := k.Recorder(); rec != nil {
					all = append(all,
						childagent.WithObserver(rec.Observe),
						childagent.WithRunContext(childContext(rec, k.memory)))
				}
				all = append(all, opts...)
				return []agenttool.Tool{childagent.New(cfg, all...)}
			},
		})
	}
}

// WithPolicy builds a policy engine from p and the matchers, and wires
// its tool filter and its BeforeToolCall hook. Use [Kit.Engine] to
// reach the engine a front needs for Deferred and Release.
//
// When a session is configured, or [WithVerdictObserver] is, the kit
// gives the engine an observer through [agentpolicy.WithObserver],
// ahead of opts: it records each verdict and hands it to the product's
// observer. An agentpolicy.WithObserver in opts replaces the kit's, as
// it would replace any earlier one, so a product that passes its own
// there records nothing through the kit; pass it to
// [WithVerdictObserver] instead.
func WithPolicy(p agentpolicy.Policy, matchers map[string]agentpolicy.ToolMatcher, opts ...agentpolicy.Option) Option {
	return func(s *settings) {
		s.policy, s.matchers, s.policySet = p, matchers, true
		s.engineOpts = append(s.engineOpts, opts...)
	}
}

// WithEngine uses an engine the product built. It is [WithPolicy] for a
// product that needs the engine before the kit exists, and the two are
// mutually exclusive. The kit cannot give an engine it did not build an
// observer, so its verdicts are recorded only if the product's own
// observer records them.
func WithEngine(e *agentpolicy.Engine) Option {
	return func(s *settings) { s.engine = e }
}

// WithGuards adds output and input guards. Each one runs on the request
// before it is sent, on each assistant message as it completes, and on
// the turn, through a [guard.Chain]'s BeforeModelCall, OutputGuard and
// ShouldStopAfterTurn.
//
// Before the request is checked whole, each instructions part is
// checked on its own, as a [guard.Input] carrying that part's text and
// no items, so a guard that rewrites instructions, [guard.Redact] for
// one, rewrites the part the text is in and [Kit.Parts] holds what was
// sent. The whole request, its items and the joined instructions, is
// then checked as before, which is where a guard that measures the
// whole, [guard.Limit], has its say. A guard therefore sees each part's
// text twice, once alone and once joined.
//
// The chain's observer records each verdict that blocked or gave a
// reason when a session is configured, and hands every verdict to
// [WithVerdictObserver].
func WithGuards(gs ...guard.Guard) Option {
	return func(s *settings) { s.guards = append(s.guards, gs...) }
}

// WithVerdictObserver is told every verdict the policy engine the kit
// built and the guards reach, after the kit has recorded it. It is the
// product's observer, for a front that shows the policy at work; the
// kit binds the recording itself.
//
// With a session configured the kit writes each of the engine's
// verdicts, and each guard verdict that blocked or gave a reason,
// through [session.Recorder.Annotate] under [agentpolicy.VerdictNS], in
// the shape [agentpolicy.Verdict.Record] gives. A guard's bare allow is
// not written, as a hook's allow with no reason is not: a guard runs on
// every part and every message, and recording its silence would
// outweigh the run.
func WithVerdictObserver(fn func(context.Context, agentpolicy.Verdict)) Option {
	return func(s *settings) { s.verdicts = fn }
}

// WithSession starts a session from h and records the run into it. The
// recorder subscribes through [Kit.Attach]; the store stays the
// caller's to sync, release and close.
//
// The recorder is opened with [session.WithInstructionsParts] bound to
// [Kit.PartsFor], ahead of opts, so its config entries carry the
// instructions as [Kit.Parts] and what the layers left out as
// instructions_omitted. A session.WithInstructionsParts in opts
// replaces the kit's.
func WithSession(store agentsession.Store, h agentsession.Header, opts ...session.Option) Option {
	return func(s *settings) {
		s.sessionStore, s.sessionHeader, s.sessionSet = store, h, true
		s.sessionResume = false
		s.sessionOpts = append(s.sessionOpts, opts...)
	}
}

// WithResumedSession continues the session with the given ID at its
// leaf, through [session.Resume]. [Kit.Session] then holds the session,
// whose Context().Items is what the agent should be seeded with. The
// recorder takes the kit's parts as under [WithSession].
func WithResumedSession(store agentsession.Store, id string, opts ...session.Option) Option {
	return func(s *settings) {
		s.sessionStore, s.sessionID, s.sessionSet = store, id, true
		s.sessionResume = true
		s.sessionOpts = append(s.sessionOpts, opts...)
	}
}

// WithRecorder records into a recorder the product opened, for a kit
// built where the recorder already exists: under an evaluation
// runner's configuration function, which hands it the recorder that
// writes the task, or inside a parent's [WithDeferredTools]. Everything
// the kit binds to a session it binds to rec: [agentturn.Config.ToolRecorder],
// the fold, a child agent, the memory manifest and the verdicts.
//
// The kit opens nothing and attaches nothing: [Kit.Attach] is a no-op,
// since the recorder's owner attaches it, and [Kit.Session] and
// [Kit.Transcript] are empty, since the owner seeded the agent. The
// recorder's options were the owner's to choose, so the kit cannot
// give it its parts; an owner that wants them passes
// session.WithInstructionsParts with a function that calls
// [Kit.PartsFor] on the kit it is about to build. It is mutually
// exclusive with [WithSession] and [WithResumedSession], and a nil rec
// is no recorder.
func WithRecorder(rec *session.Recorder) Option {
	return func(s *settings) { s.recorder = rec }
}

// WithCompaction folds the transcript with the model the kit was given
// when it grows past budget tokens. When a session is recorded, the
// fold is recorded with it.
func WithCompaction(budget int, opts ...compact.Option) Option {
	return func(s *settings) {
		s.compactSet = true
		s.compactOpts = append(s.compactOpts, compact.WithBudget(budget))
		s.compactOpts = append(s.compactOpts, opts...)
	}
}

// WithCompactionModel folds with a model other than the agent's, which
// is how a cheap model summarises for an expensive one.
func WithCompactionModel(m openresponses.Streamer) Option {
	return func(s *settings) { s.compactSet, s.compactModel = true, m }
}

// WithCompactor folds with a compactor the product built.
func WithCompactor(c compact.Compactor, opts ...compact.Option) Option {
	return func(s *settings) {
		s.compactSet, s.compactor = true, c
		s.compactOpts = append(s.compactOpts, opts...)
	}
}

// WithBeforeTurn adds a hook to the start of each turn. Several are
// joined with [agentturn.ChainBeforeTurn], in the order given.
func WithBeforeTurn(fn func(context.Context, agentturn.TurnStartInfo) (openresponses.Items, error)) Option {
	return func(s *settings) { s.beforeTurn = append(s.beforeTurn, fn) }
}

// WithBeforeModelCall adds a hook on the built request. It runs after
// the kit's own: after memory has re-rendered the instructions and
// after the guards have seen them.
func WithBeforeModelCall(fn func(context.Context, *openresponses.Request) error) Option {
	return func(s *settings) { s.beforeModelCall = append(s.beforeModelCall, fn) }
}

// WithBeforeToolCall adds a policy on each tool call, after the
// engine's. Decisions fold deny over ask over allow, as
// [agentturn.ChainBeforeToolCall] describes.
func WithBeforeToolCall(fn func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error)) Option {
	return func(s *settings) { s.beforeToolCall = append(s.beforeToolCall, fn) }
}

// WithAfterToolCall sets the hook that may replace a tool's result. The
// kit contests nothing here, so it is the product's field alone and
// setting it twice keeps the second.
func WithAfterToolCall(fn func(context.Context, agentturn.ToolResultInfo) (*agentturn.ToolOverride, error)) Option {
	return func(s *settings) { s.afterToolCall = fn }
}

// WithToolElicitor sets the elicitor a tool's question to the user
// reaches mid-call, an MCP server's elicitation among them, as
// [agentturn.Config.ToolElicitor]. With a session configured the kit
// sets the recorder's [session.Recorder.Elicitor] around fn, so each
// question and its answer are written under the call that asked; by
// names who answers, in the session format's words, such as
// [agentpolicy.ByHuman]. Without the option the field stays nil and an
// elicitor on the prompt's context applies.
func WithToolElicitor(by string, fn agenttool.Elicitor) Option {
	return func(s *settings) { s.elicitBy, s.elicitor = by, fn }
}

// WithOutputGuard adds a guard on each assistant message, after the
// guards [WithGuards] added. Each sees what the one before it left.
func WithOutputGuard(fn func(context.Context, agentturn.OutputInfo) (*openresponses.Message, error)) Option {
	return func(s *settings) { s.outputGuard = append(s.outputGuard, fn) }
}

// WithShouldStopAfterTurn adds a reason to end a run after a turn,
// after the policy's. The chain stops at the first hook that stops the
// run, so the error on RunEnd says which one fired.
func WithShouldStopAfterTurn(fn func(context.Context, agentturn.TurnInfo) (bool, error)) Option {
	return func(s *settings) { s.shouldStop = append(s.shouldStop, fn) }
}

// WithTransform sets the product's transcript transform. With
// compaction as well, the two are joined with
// [agentturn.ChainTransform], the product's first, so the fold is over
// what the product shaped. Setting it twice keeps the second.
func WithTransform(fn func(context.Context, agentturn.Transcript) (agentturn.Transcript, error)) Option {
	return func(s *settings) { s.transform = fn }
}

// WithFilter sets the filter that drops app-only items before each
// model call. nil, the default, means [agentturn.DefaultFilter].
func WithFilter(fn func(agentturn.Transcript) agentturn.Transcript) Option {
	return func(s *settings) { s.filter = fn }
}

// childContext is the run context WithChildAgent gives a child: the
// recorder's ChildContext, which puts the child's session ID on the
// context under the session package's key, and, with memory
// configured, the same ID under agentmemory's, since the memory journal
// reads its own key and neither package imports the other.
func childContext(rec *session.Recorder, memory bool) func(context.Context, string) context.Context {
	return func(ctx context.Context, callID string) context.Context {
		ctx = rec.ChildContext(ctx, callID)
		if memory {
			if id := session.SessionIDFromContext(ctx); id != "" {
				ctx = agentmemory.WithSession(ctx, id)
			}
		}
		return ctx
	}
}
