package agentkit

import (
	"context"
	"fmt"
	"io"
	"slices"

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
	skillDirs        []skillDir
	skillToolWith    bool
	skillToolWithout bool
	skillToolOpt     []agentskill.ToolOption
	skillGrants      bool
	skillGrantScope  bool
	skillSource      func(*agentskill.Skill) agentpolicy.Source
	skillGrant       func(SkillGrant)

	memStore  agentmemory.Store
	memScopes []agentmemory.Scope
	memRead   []agentmemory.Scope
	memRender []agentmemory.RenderOption
	memTools  []agentmemory.ToolOption

	tools      []agenttool.Tool
	deferTools []deferred
	mcp        []mcpDial
	mcpStderr  io.Writer
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
	// compactLocal records WithCompaction apart from compactSet, so a
	// compactor beside it, which folds another way under another budget,
	// is a contradiction New can refuse.
	compactLocal bool
	foldObserver func(context.Context, compact.Fold)

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

// summaryReasoning is the reasoning the agent's requests carry, which a
// fold's summary request is given too: [WithReasoning]'s, else the one
// [WithRequest]'s base request names, as the loop resolves it.
func (s *settings) summaryReasoning() openresponses.ReasoningConfig {
	if !s.reasoning.IsZero() {
		return s.reasoning
	}
	return s.request.Reasoning
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

// WithReasoning sets the reasoning effort and summary of every request,
// the summary request a [WithCompaction] fold sends among them.
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
	return func(s *settings) {
		for _, d := range dirs {
			s.skillDirs = append(s.skillDirs, skillDir{path: d})
		}
	}
}

// WithOptionalSkills is [WithSkills] for a directory the user may not
// have made, such as ~/.dex/skills: one that does not exist is passed
// over, and one that exists and cannot be read is still an error from
// [New]. The directories take their place among WithSkills' in the
// order the options were given, since that order decides which of two
// skills of one name shadows the other.
//
// Keep WithSkills for the directory the product configures, where a
// misspelling that silently offers no skills is the failure to refuse.
// A directory passed over is not reported, since it held nothing to
// leave out; a product that shows it stats the path itself. When every
// skill directory was optional and none is there, and no
// [WithSkillSources] were given, there is no catalogue: no skills part
// and no skill tool, as if skills were not configured.
func WithOptionalSkills(dirs ...string) Option {
	return func(s *settings) {
		for _, d := range dirs {
			s.skillDirs = append(s.skillDirs, skillDir{path: d, optional: true})
		}
	}
}

// skillDir is one directory WithSkills or WithOptionalSkills named.
type skillDir struct {
	path     string
	optional bool
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
// named "agentskill:" and the skill's listed name,
// [agentskill.Skill.ListedName], with the skill's location as its
// path, which is untrusted and therefore contributes its deny and ask
// rules alone. A source function should key its name on ListedName
// too: a root "deploy" and a qualified "apps/web:deploy" share Name,
// and two skills given one source name share one grant.
//
// A grant lasts until something revokes it, and a restart does not: with
// a session [New] opened, New replays the session's journal and grants
// again each skill read no recorded revocation ended, so an approval
// after a restart goes on under the grants the task had. A restart may
// narrow a grant and never widens one: what is granted again is the
// rules the journal says the read was granted, as the engine recorded
// them after [agentpolicy.WithAliases] expanded them, when the read
// recorded the digest of the frontmatter it was granted from and the
// skill has it still; a skill whose instructions or frontmatter changed
// since the read is not granted at all; under [WithEngine], where the
// kit records no verdict, it is the catalogue's rules as they stand. The replay records no
// verdict, since the ones it repeats are on the path, and is reported
// with [SkillGrant.Replayed] set. A front that resumes a conversation
// itself calls [Kit.RegrantSkills].
//
// The grants belong to one conversation, since the engine applies a
// grant to every decision it makes: the session the kit opened or was
// given, or the conversation of the first grant, [ContextWithRecorder].
// The first call the kit decides in another conversation revokes every
// grant and the kit grants nothing after it; see
// [ErrSkillGrantConversation]. A front that grants skills to many
// conversations gives each its own kit. [WithSkillGrantScope]
// revokes every skill's grant when the user's next message arrives, which is the lifetime
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
// A source function that trusts a skill by its name or its directory
// trusts whatever is written there next, an agent's own skills
// included. To trust what a person approved, set Trusted only when
// [agentskill.Skill.FrontmatterSHA256], the digest of the frontmatter
// the rules are parsed from, is one they approved, and, to approve the
// text too, the SHA-256 of [agentskill.Skill.Instructions]. The digest of
// the instructions alone does not cover allowed-tools. The default
// source names the frontmatter's digest as its Hash, so every verdict
// about a grant says what it was built from; a source function does the
// same by setting Hash.
//
// A grant lives in the engine, so [New] over a resumed session and
// [Kit.RegrantSkills] grant again what the session's reads left in
// force, and only for a skill that is what the model read: the digest
// of the instructions the catalogue's tool served, which agentskill
// ends with the skill's file list and each file's size, and the digest
// of its frontmatter. So a skill that writes into its own directory, a
// draft changelog or a log, is not granted again after a restart, nor
// is one whose allowed-tools were edited or whose body
// [Kit.ReloadSkills] picked up with no read after; the front hears each
// through [WithSkillGrantReport], with [ErrSkillGrantChanged], and a
// read of the skill grants by the skill as it is now.
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
// message from the user arrives, as Claude Code clears allowed-tools
// when the next message arrives: a later request that wants the tools
// reads the skill again. The message may start a run, follow the run's
// answer through [agentturn.Agent.FollowUp], be steered in between
// turns, come with a developer note after it, or arrive in another
// agent's run, as it does when the kit's agent handed the conversation
// to another kit's and is handed it back; each revokes. A run that
// Resume starts after an approval, or that Continue starts, is the same
// task going on and keeps them, after a restart too, since the scope's
// revocations are in the session's journal.
//
// It is a [agentturn.Config.BeforeTurn] hook, ahead of the product's
// own. Each grant is bound to the user message in force when the read
// made it: the count of user messages in the turn's transcript and a
// digest of the last one, its role and the JSON of its content. The
// hook calls [Kit.RevokeSkillGrants] on every turn whose transcript
// holds more user messages than a grant's count, or whose last user
// message is not the one it digested, and on every turn whose
// transcript's tail, back to the last item the model or a tool
// produced, holds a user message, which catches a grant made in no run
// and so bound to nothing. A compaction that folds away older messages
// lowers the count and keeps the last message, so it ends nothing by
// itself. A grant [New] or [Kit.RegrantSkills] grants again is bound
// to the message the session's path ends under, which the agent seeded
// from it starts under, so the restart's first turn keeps it. A steer
// delivered before an output is one more user message, so it revokes
// on the turn that follows, which the tail test alone left to
// [agentturn.TurnStartInfo].
//
// Under the scope the model is told, in a developer note at the turn
// that ended a grant, which skills' tools ended and that a read
// restores them; and a call that only an ended grant allowed is refused
// by the kit's [agentturn.Config.BeforeToolCall] hook, ahead of the
// engine, with a reason naming the skill and the tool that reads it
// again, until the skill is read again. The engine would have deferred
// the call to a reviewer, whose refusal says nothing of the skill. The
// kit refuses only when the engine cannot allow the call on its own, as
// far as its exported state says: no rule of the policy or of a grant
// in force names the call's tools, the default does not allow or an ask
// rule names them, and the tool does not say it runs confined; a call
// a deny rule names gets the deny's reason. A call's subjects are the
// tool's [agentpolicy.Subjects] split under [WithPolicy]'s matchers and
// each must be covered by an ended rule, bare or with a specifier the
// tool's matcher matches; under [WithEngine] the kit has no matchers
// and only a bare rule covers. A hook the product folded into the
// engine is not consulted first. The refusal is recorded as a verdict
// under [agentpolicy.VerdictNS], as the engine's would be, and is not
// reported through [WithSkillGrantReport].
//
// Only the sources the kit granted are revoked; a product's own
// [agentpolicy.Engine.GrantSet] calls are left alone. Every run on the
// engine in the conversation the grants belong to revokes them, so two
// agents sharing one engine share one scope; a message in another
// conversation the kit serves revokes nothing, since the kit makes no
// grant there. It has no effect without [WithSkillGrants].
func WithSkillGrantScope() Option {
	return func(s *settings) { s.skillGrantScope = true }
}

// WithSkillGrantReport is told what the engine did with each skill's
// allowed-tools: what it granted, what it refused and why, a skill
// whose allowed-tools would not parse, a read the kit would not grant
// because its grants belong to another conversation, a read the
// catalogue's tool refused because the skill file is gone, renamed or
// no longer parses, with Err wrapping [agentskill.ErrSkillChanged],
// and, with [SkillGrant.Replayed] set, what a restart granted again and
// the read it will not grant again, with Err wrapping
// [ErrSkillGrantChanged] when the skill changed since the read and
// [ErrSkillGrantUnrecorded] when the session records no verdict of the
// read's grant. A grant widens what the agent may do, so a front that
// shows the user the policy in force wants to see it happen; and a
// front reloads the skills, [Kit.ReloadSkills], on a report wrapping
// agentskill.ErrSkillChanged or with [SkillGrant.FrontmatterChanged]
// set, since the model is told to discover the skills again and cannot.
func WithSkillGrantReport(fn func(SkillGrant)) Option {
	return func(s *settings) { s.skillGrant = fn }
}

// WithMemory renders the memory block for the given scopes, in order,
// as the [PartMemory] group of parts, offers the memory tools, and
// re-renders the block before every model call so the model sees the
// freshest state. [agentmemory.Usage], the paragraph that tells the
// model what the block is and which tool makes which change, follows
// the block as [PartMemoryUsage], since the tools are offered whenever
// the block is.
//
// A render [WithInstructionBudget] leaves no room for drops the block
// and the paragraph, and that request is not offered memory_save,
// memory_patch or memory_forget: the model would be writing over
// entries it was never shown, with no word on the tools. A write the
// model makes anyway in that run is refused, and so is one a policy
// held and a person approved, which runs in a Resume: the render kept
// for the call says the block was dropped, and after a restart the
// manifest recorded at the call does, where it shows no entry and lists
// one the block held among the omitted. memory_search stays
// offered, and [Kit.Omitted] lists every entry the drop left out. The
// block, and the writes, come back on the first render that fits.
//
// The block is recorded as one part per piece
// [agentmemory.RenderParts] returns, the title, each scope's heading,
// each entry and the summary line, so a write to one entry is recorded
// as that entry's part and the summary rather than the whole block.
//
// memory_save is given [agentmemory.WithRendered] with the render its
// own run's model was shown, ahead of [WithMemoryTools]: a save is based
// on the entry the block showed the model, so a write another session
// made after the render is reported by [agentmemory.LostUpdates] and the
// model is told, rather than silently discarded. The render is looked up
// by [agentturn.RunIDFromContext], so concurrent runs off one kit each
// save over their own. A call held for approval keeps its run's render,
// kept by a BeforeToolCall hook of the kit's ahead of whatever holds it,
// for the Resume that runs it; after a restart, or once the kit has
// dropped the run, a call is based on the manifest in force on its
// session's path where the model made it. A call in a run for which the
// kit has none of these is refused, and the model told to look at the
// entry again, rather than based on another run's render. A
// [WithMemoryTools] option of the same kind replaces the kit's.
//
// With a session, each render that differs from the manifest in force
// on the session's path is recorded under [agentmemory.ManifestNS], as
// [agentmemory.Manifest.RecordSince] the one in force: whole in a
// session that holds none, and a delta otherwise, in the session New
// opened, one on a run's context and a child's alike.
func WithMemory(store agentmemory.Store, scopes ...agentmemory.Scope) Option {
	return func(s *settings) { s.memStore, s.memScopes = store, scopes }
}

// WithMemoryReadScopes renders scopes the model may read and not write,
// such as project rules the product keeps in memory for the model to
// follow and never edit. A scope [WithMemory] also names is rendered in
// its place there and read-only; one it does not is rendered after its
// scopes. The memory tools are built over the writable scopes alone,
// with [agentmemory.WithReadScopes] naming these, so memory_search
// reaches what the block omitted from them and the writers refuse them.
//
// At least one scope must stay writable, since agentmemory's tools
// cannot be built over none; [New] fails otherwise. It has no effect
// without [WithMemory]. Several calls accumulate.
func WithMemoryReadScopes(scopes ...agentmemory.Scope) Option {
	return func(s *settings) { s.memRead = append(s.memRead, scopes...) }
}

// renderScopes is every scope the block renders: WithMemory's, in
// order, then the read scopes it does not name, each once.
func (s *settings) renderScopes() []agentmemory.Scope {
	out := append([]agentmemory.Scope(nil), s.memScopes...)
	for _, r := range s.memRead {
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// writableScopes is WithMemory's scopes less the read-only ones.
func (s *settings) writableScopes() []agentmemory.Scope {
	var out []agentmemory.Scope
	for _, sc := range s.memScopes {
		if !slices.Contains(s.memRead, sc) {
			out = append(out, sc)
		}
	}
	return out
}

// readScopes is the read-only scopes, each once.
func (s *settings) readScopes() []agentmemory.Scope {
	var out []agentmemory.Scope
	for _, r := range s.memRead {
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	return out
}

// WithMemoryRender passes options to [agentmemory.RenderParts], both
// for the first render and for the per-turn one.
func WithMemoryRender(opts ...agentmemory.RenderOption) Option {
	return func(s *settings) { s.memRender = append(s.memRender, opts...) }
}

// WithMemoryTools passes options to [agentmemory.Tools], after the
// kit's own [agentmemory.WithRendered] and, under
// [WithMemoryReadScopes], [agentmemory.WithReadScopes]. A read scope
// given here that [WithMemory] also names is an error from [New]; name
// it in [WithMemoryReadScopes] instead.
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
//
// The server's stderr goes to [WithMCPStderr], and nowhere without it;
// either way the kit keeps its last two kilobytes, and an error from
// [New] connecting to the server ends with them, since a server that
// fails at start says why there. The label a [Conflict], [WithToolFilter]
// and [Kit.Tools] give its tools is "mcp:#<n> <program>", the command's
// first word: its arguments are where a credential is put, and the
// label is logged.
//
// With [WithToolElicitor] set, the client is dialed with
// [mcpclient.WithElicitation] ahead of opts, so a question the server
// asks mid-call reaches that elicitor under the call that asked. Every
// server may then ask the user, a URL to visit among them; a product
// that trusts one server less passes that server an ElicitationHandler
// through [mcpclient.WithClientOptions], which takes precedence.
//
// The error's stderr tail is the server's own words and may carry what
// the server printed, a token in a failed request among them; a product
// that logs errors from New logs it.
//
// The server is dialed once, at New, and every run the kit serves calls
// it over that one connection, so whatever identity the connection
// carries is the kit's, not a conversation's or a user's. A server
// authorized by OAuth acts as whoever authorized it for every
// conversation: the token belongs to the connection, and the server
// refuses another user's token on it. A front serving several users
// with such a server gives each user a kit of their own, and keeps each
// user's grant with [mcpclient.StoreTokens]; see [WithMCPTransport].
// [Kit.AddMCP] connects a server after New, the same way.
func WithMCP(command string, opts ...mcpclient.Option) Option {
	return func(s *settings) {
		s.mcp = append(s.mcp, mcpDial{command: command, opts: opts})
	}
}

// WithMCPTransport connects to an MCP server over the given transport.
// Its label is "mcp:#<n>" and what the transport reaches, without the
// places credentials go: a *mcp.CommandTransport's program, an HTTP
// transport's scheme and host, or the transport's type. The command's stderr is
// the product's to set, since it built the command. [WithToolElicitor]
// binds elicitation as for [WithMCP]. The connection's identity is the
// kit's, as for WithMCP: a transport carrying one user's credentials
// serves every conversation the kit serves as that user.
//
// A server behind OAuth takes a *mcp.StreamableClientTransport whose
// OAuthHandler is the SDK's authorization-code handler. Its grant lives
// in memory unless [mcpclient.StoreTokens] keeps it in a
// [mcpclient.TokenStore], so a restart does not send the user to consent
// again. The store is keyed by the endpoint and a subject the host
// names, which is the user the kit is built for:
//
//	cfg := &auth.AuthorizationCodeHandlerConfig{ /* client, redirect, fetcher */ }
//	key := mcpclient.TokenKey{Endpoint: endpoint, Subject: userID}
//	if err := mcpclient.StoreTokens(ctx, cfg, store, key, logSaveError); err != nil {
//		return err
//	}
//	h, err := auth.NewAuthorizationCodeHandler(cfg)
//	kit, err := agentkit.New(ctx,
//		agentkit.WithMCPTransport(&mcp.StreamableClientTransport{Endpoint: endpoint, OAuthHandler: h}),
//		...)
//
// The handler, the fetcher and the store are the product's: the kit
// dials the transport it is given and adds nothing to its
// authorization.
func WithMCPTransport(t sdk.Transport, opts ...mcpclient.Option) Option {
	return func(s *settings) {
		s.mcp = append(s.mcp, mcpDial{transport: t, opts: opts})
	}
}

// WithMCPStderr sends the stderr of every server [WithMCP] starts to w,
// os.Stderr for a command-line product, a log for one with a screen of
// its own. The servers' writes are serialised, so w need not be safe
// for concurrent use. A write to w that fails is dropped rather than
// stopping the server. A later call replaces an earlier one.
func WithMCPStderr(w io.Writer) Option {
	return func(s *settings) { s.mcpStderr = w }
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
// servers', those [Kit.AddMCP] connected among them, and a name it
// repeats is a [Conflict] resolved the same way. Several providers are each their own source, in the order given,
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
// It runs inside the kit's own wrapper, so the skill tool's grant is
// made on what fn's tool returned: a wrapper whose result carries the
// catalogue's agentskill.Read in Details still grants, and one that
// returns a result without it, a replay that decodes a recording into
// its own type, does not. The filter sees the tool its source produced,
// not fn's. A fixed source's tools, those from [WithTools],
// [WithChildAgent], [WithDeferredTools], [WithSkills] and [WithMemory],
// are wrapped once, in [New], before the filter is asked about them, so
// fn is called for a tool the filter then drops; an MCP server's and a
// [WithToolProvider]'s are wrapped each turn, after the filter, since
// their lists are fetched each turn. A wrapper that keeps a tool's properties uses
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
// When the parent's run has a recorder, the kit's or one a front put on
// its context with [ContextWithRecorder], the child's run is recorded
// into it, live and linked to the parent's, because the kit binds
// childagent.WithObserver to that recorder, and the child runs under
// its [session.Recorder.ChildContext] through childagent.WithRunContext, so what the child's tools attribute to a
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
				all = append(all,
					childagent.WithObserver(k.observeChild),
					childagent.WithRunContext(k.childContext))
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
// The kit gives the engine three options ahead of opts:
//
//   - [agentpolicy.WithTools] with [Kit.LookupTool], the union as of the
//     current turn, so a call ahead of a confined command in one batch
//     is not held for a sibling the engine could not see. The union
//     does not exist when the engine is built, so a product cannot hand
//     it in; an agentpolicy.WithTools in opts replaces the kit's.
//   - [agentpolicy.WithObserver]: it records each verdict into the
//     run's recorder, the one [ContextWithRecorder] put on its context
//     or the kit's own, and hands it to [WithVerdictObserver]. The
//     engine keeps every observer it is given, so an
//     agentpolicy.WithObserver in opts runs beside the kit's, after it,
//     and the recording stays either way.
//   - [agentpolicy.WithHooks] with the [WithBeforeToolCall] hooks, which
//     the engine folds into its decision before the batch hold.
func WithPolicy(p agentpolicy.Policy, matchers map[string]agentpolicy.ToolMatcher, opts ...agentpolicy.Option) Option {
	return func(s *settings) {
		s.policy, s.matchers, s.policySet = p, matchers, true
		s.engineOpts = append(s.engineOpts, opts...)
	}
}

// WithEngine uses an engine the product built. It is [WithPolicy] for a
// product that needs the engine before the kit exists, and the two are
// mutually exclusive. The kit cannot give an engine it did not build
// options, so its verdicts are recorded only if the product's own
// observer records them, it reads siblings' tools only if the product
// passed agentpolicy.WithTools with [Kit.LookupTool], and the
// [WithBeforeToolCall] hooks are chained after it rather than folded
// into it; a product that wants them held with their siblings passes
// them to the engine with [agentpolicy.WithHooks] instead.
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
// [New] runs the same per-part pass over its own render, so
// [agentturn.Config.Instructions] is the text the guards leave, and the
// config entry a recorder settles at a run's start, before any hook has
// run, carries nothing a guard kept from the model. A guard that refuses
// a part there is an error from New. That pass has no observer, since
// there is no run to record under; the first turn's pass reports the
// same verdicts.
//
// A refusal names the part, "instructions/<id>", in New's error and in
// a turn's, so a line saved to memory and the same line in AGENTS.md are
// told apart.
//
// The chain's observer records each verdict that blocked or gave a
// reason into the run's recorder, when there is one, and hands every
// verdict to [WithVerdictObserver].
//
// [WithInstructionBudget] measures the parts before the guards run, so
// a guard that makes a part longer, a redaction whose placeholder is
// longer than the secret, can send instructions over the budget.
func WithGuards(gs ...guard.Guard) Option {
	return func(s *settings) { s.guards = append(s.guards, gs...) }
}

// WithVerdictObserver is told every verdict the policy engine the kit
// built and the guards reach, after the kit has recorded it. It is the
// product's observer, for a front that shows the policy at work; the
// kit binds the recording itself. It is the one way to see the guards'
// verdicts, since the kit builds their chain. The engine's verdicts
// also reach an agentpolicy.WithObserver passed to [WithPolicy], so a
// product that passes both is told each engine verdict twice.
//
// With a recorder, the kit's or one a front put on the run's context
// with [ContextWithRecorder], the kit writes each of the engine's
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
// Every agent built from the kit is attached to it, or prompted under
// [ContextWithRecorder] with a recorder of its own. A run that is
// neither is one the session does not record, and the kit writes none
// of its memory renders there: a record with no run around it would be
// read as another run's. Such a run's memory_save, held for approval
// and approved after a restart, is refused, since no session holds the
// render it was composed from; in the process that rendered it, it is
// based on that render as any run's is.
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
// and [Kit.AgentOptions] seeds an agent with it: [session.AgentOptions],
// the transcript with the items the filter kept from the model put
// back, which Context().Items leaves out, the model each reasoning item
// came from, so a request to another model leaves the earlier one's
// out, and the calls pending at the leaf. The recorder takes the kit's
// parts, and every agent built from the kit is attached to it or
// prompted under [ContextWithRecorder], as under [WithSession]. Under [WithSkillGrants], the grants the
// session's skill reads made are granted again, and under
// [WithCompaction] the fold backs off from the last fold that failed on
// the session's path.
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
// fold is recorded with it. Summary requests name the agent's model,
// [compact.WithModel] ahead of opts.
//
// The kit records the fold through a compact.WithOnFold of its own,
// ahead of opts, whether or not there is a session, since the recorder
// may arrive on a run's context, [ContextWithRecorder], after New.
// compact.WithOnFold adds a callback, so one in opts is called too,
// after the fold is recorded. [WithFoldObserver] is the kit's own way
// to hear of a fold, with or without a session.
//
// The summary request is sent under the agent's reasoning, the
// [WithReasoning] or [WithRequest] one, through a [compact.WithRequest]
// of the kit's ahead of opts: a thinking model left at its server's
// default reasons through the summary's cap and answers no text, which
// fails every fold. compact.WithRequest is one function, so one in opts
// replaces the kit's, and a product that passes one sets Reasoning in
// it itself.
//
// With a session [New] resumed, the last fold that failed on its path,
// [session.CompactOptions], is passed after opts, so a restart does not
// ask again for a summary that failed before it. Under [WithRecorder] or
// [ContextWithRecorder] the kit does not know the session when it
// builds the fold, and the product passes session.CompactOptions in opts.
func WithCompaction(budget int, opts ...compact.Option) Option {
	return func(s *settings) {
		s.compactSet, s.compactLocal = true, true
		s.compactOpts = append(s.compactOpts, compact.WithBudget(budget))
		s.compactOpts = append(s.compactOpts, opts...)
	}
}

// WithFoldObserver is told of each fold compaction makes, after the
// recorder has written it when a session is recorded. It hears a fold
// that failed as well as one that folded: since agentturn v0.0.13 a
// summary no smaller than what it folds, one cut short, or one with no
// text fails the fold quietly, with [compact.Fold] Err set and no
// Summary, and the transcript is sent whole. A front that says the
// model has forgotten a detail says it only for a fold whose Err is
// nil. It has no effect without [WithCompaction] or [WithCompactor]. A
// later call replaces an earlier one.
func WithFoldObserver(fn func(context.Context, compact.Fold)) Option {
	return func(s *settings) { s.foldObserver = fn }
}

// WithCompactionModel folds with a model other than the agent's, which
// is how a cheap model summarises for an expensive one.
func WithCompactionModel(m openresponses.Streamer) Option {
	return func(s *settings) { s.compactSet, s.compactModel = true, m }
}

// WithCompactor folds through a compactor the product built, a
// provider's compaction endpoint, when the transcript grows past budget
// tokens. It is [WithCompaction] with the fold made elsewhere, and
// takes the same budget and the same default: every request is sent
// under the agent's model name, [compact.WithModel] ahead of opts, so a
// compact.WithModel in opts still wins. The fold is recorded, and
// [WithFoldObserver] told, as under WithCompaction, and a
// compact.WithOnFold in opts is called after the recording as there.
//
// A compactor and a local summary are two ways to fold, so [New]
// refuses WithCompactor beside WithCompaction or
// [WithCompactionModel].
func WithCompactor(c compact.Compactor, budget int, opts ...compact.Option) Option {
	return func(s *settings) {
		s.compactSet, s.compactor = true, c
		s.compactOpts = append(s.compactOpts, compact.WithBudget(budget))
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

// WithBeforeToolCall adds a policy on each tool call. Decisions fold
// deny over ask over allow, as [agentturn.ChainBeforeToolCall]
// describes.
//
// With [WithPolicy] the hooks are folded into the engine's own decision
// through [agentpolicy.WithHooks], so a hook that asks about a call
// holds the call's siblings with it, and a hook that blocks a call the
// policy asked about leaves nothing held. The engine reads a call's
// siblings to decide whether to hold it, so a hook may be called for a
// call before the loop hands it that call, and more than once: it must
// decide a call the same way each time it is asked, reading the call
// from its [agentturn.ToolCallInfo] and not from the context, which
// then carries the call being decided rather than the sibling. A hook
// that asks a person is asked before the batch runs. Without a policy,
// or with [WithEngine], they are chained after the engine's hook.
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
// [agentturn.Config.ToolElicitor]. With a recorder, the kit's or the
// one on the run's context, the kit puts that recorder's
// [session.Recorder.Elicitor] around fn, so each question and its
// answer are written under the call that asked; by
// names who answers, in the session format's words, such as
// [agentpolicy.ByHuman]. Without the option the field stays nil and an
// elicitor on the prompt's context applies.
//
// An MCP client offers the server elicitation only when dialed with
// [mcpclient.WithElicitation], so with this option set the kit dials
// every server [WithMCP] and [WithMCPTransport] name with it, ahead of
// their own options.
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

// observeChild is the observer WithChildAgent gives a child: the Observe
// of the recorder the parent's run records into, or nothing without one.
func (k *Kit) observeChild(ctx context.Context, ev agentturn.Event) {
	if rec := k.recorderFor(ctx); rec != nil {
		rec.Observe(ctx, ev)
	}
}

// childContext is the run context WithChildAgent gives a child: the
// ChildContext of the recorder the parent's run records into, which
// puts the child's session ID on the context under the session
// package's key, and, with memory configured, the same ID under
// agentmemory's, since the memory journal reads its own key and neither
// package imports the other. Without a recorder it is ctx.
func (k *Kit) childContext(ctx context.Context, callID string) context.Context {
	rec := k.recorderFor(ctx)
	if rec == nil {
		return ctx
	}
	conv := k.conversation(ctx)
	ctx = rec.ChildContext(ctx, callID)
	if id := session.SessionIDFromContext(ctx); id != "" {
		ctx = context.WithValue(ctx, childConvKey{}, childConv{sid: id, conv: conv})
	}
	if k.memory {
		if id := session.SessionIDFromContext(ctx); id != "" {
			ctx = agentmemory.WithSession(ctx, id)
		}
	}
	return ctx
}
