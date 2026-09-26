package agentkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

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
	"github.com/ChristopherDavenport/openresponses"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Kit is an assembled configuration and the things a front needs that
// an [agentturn.Config] cannot carry. It is built by [New], which does
// everything that can fail, and read by [Kit.Config], which is pure and
// may be called once per run.
//
// # Concurrency
//
// Every method is safe to call from any goroutine, and so is every
// config [Kit.Config] returns. The configs are not independent of each
// other: they share the kit's memory state, because they share the hook
// that renders it. Two runs off one kit each render memory for
// themselves and each build their own instructions, so neither sees a
// prompt the other assembled; what they share is [Kit.Parts] and
// [Kit.MemoryManifest], which describe the render that happened last,
// and the record of the manifest, which is written once per distinct
// render because one session records both runs.
//
// That is the right sharing for concurrent runs of one agent and the
// wrong sharing for two agents, which want two prompts, two manifests
// and usually two sessions. Give each agent its own kit.
type Kit struct {
	cfg     agentturn.Config
	parts   []Part
	order   []string
	omitted []Omission

	engine  *agentpolicy.Engine
	catalog *agentskill.Catalog
	rec     *session.Recorder
	sess    *agentsession.Session
	seed    agentturn.Transcript
	remotes []*mcpclient.Remote

	// closed is read by the per-turn tool provider, which a config
	// handed out before Close may still be called through.
	closed atomic.Bool

	// mu guards the parts and the memory manifest, which the per-turn
	// re-render replaces.
	mu       sync.Mutex
	manifest agentmemory.Manifest

	// recmu guards recorded, the hash of the manifest last written to
	// the session, and is held across the write. It is its own lock for
	// two reasons: the comparison, the write and the remembering have to
	// be one step, or two turns rendering different manifests can write
	// their annotations in the other order and leave the session's last
	// one describing a render that is no longer in force; and holding mu
	// across a store would block Kit.Parts on a front's own thread.
	recmu    sync.Mutex
	recorded string
}

// New assembles the kit. It does the work that can fail — discovering
// skills, reading the AGENTS.md chain, rendering memory, building the
// policy engine, starting or resuming the session, dialing every MCP
// server — and reports it once. A kit that dialed some servers before
// one failed closes the ones it opened before returning the error, so a
// failed New leaks nothing.
func New(ctx context.Context, opts ...Option) (*Kit, error) {
	var s settings
	for _, o := range opts {
		if o != nil {
			o(&s)
		}
	}
	if s.model == nil {
		return nil, fmt.Errorf("agentkit: %w; pass WithModel", agentturn.ErrNoModel)
	}
	if s.policySet && s.engine != nil {
		return nil, errors.New("agentkit: WithPolicy and WithEngine both set; the kit builds an engine or is given one")
	}
	if s.transform != nil && s.compactSet {
		return nil, errors.New("agentkit: WithTransform and compaction both set; Transform is one field and there is no chain for it")
	}
	// The two contradictions in the skill-tool options. Not configuring
	// skills at all leaves both of these inert instead, because a
	// product that adds WithSkills behind a flag and the rest
	// unconditionally is writing ordinary code, not a mistake.
	if s.skillToolWith && s.skillToolWithout {
		return nil, errors.New("agentkit: WithSkillTool and WithoutSkillTool both set; the catalogue's tool is offered or it is not")
	}
	if s.skillGrants && s.skillToolWithout {
		return nil, errors.New("agentkit: WithSkillGrants and WithoutSkillTool both set; a grant happens when the model reads a skill through the catalogue's tool, so withholding that tool withholds every grant")
	}

	k := &Kit{}
	fail := func(err error) (*Kit, error) {
		return nil, errors.Join(err, k.Close())
	}

	engine, err := buildEngine(&s)
	if err != nil {
		return nil, err
	}
	k.engine = engine

	if err := k.assemble(ctx, &s); err != nil {
		return fail(err)
	}
	if err := k.dial(ctx, &s); err != nil {
		return fail(err)
	}
	if err := k.openSession(ctx, &s); err != nil {
		return fail(err)
	}
	if err := k.build(ctx, &s); err != nil {
		return fail(err)
	}
	return k, nil
}

// share turns the bytes left of a budget into the limit a part is
// rendered under: zero when there is no budget, which leaves the layer
// its own bound, the remainder when there is room, and a negative
// number when there is not, which drops the part rather than sending a
// block the budget said there was no room for. Zero has to mean "no
// bound" and not "no room", so a budget exactly spent must not fall
// through to it.
func share(budget, avail int64) int64 {
	switch {
	case budget <= 0:
		return 0
	case avail > 0:
		return avail
	default:
		return -1
	}
}

// offersSkillTool reports whether the catalogue's tool is offered: yes
// when skills are configured, unless WithSkillTool or WithoutSkillTool
// said otherwise. The same answer decides whether the usage paragraph
// joins the catalogue part, since it describes that tool. New refuses
// both options at once, so the order of the two tests here does not
// decide anything.
func offersSkillTool(s *settings) bool {
	if s.skillToolWithout {
		return false
	}
	if s.skillToolWith {
		return true
	}
	return len(s.skillDirs)+len(s.skillSources) > 0
}

func buildEngine(s *settings) (*agentpolicy.Engine, error) {
	if s.engine != nil {
		return s.engine, nil
	}
	if !s.policySet {
		return nil, nil
	}
	e, err := agentpolicy.Build(s.policy, s.matchers, s.engineOpts...)
	if err != nil {
		return nil, fmt.Errorf("agentkit: building the policy engine: %w", err)
	}
	return e, nil
}

// assemble builds the instruction parts, in the order the kit
// documents and under the budget the caller set. The parts are rendered
// in a different order than they are joined: see docs/ordering.md.
func (k *Kit) assemble(ctx context.Context, s *settings) error {
	parts := map[string]Part{}
	configured := map[string]bool{}

	if s.instructions != "" {
		parts[PartProduct] = Part{ID: PartProduct, Text: s.instructions, Source: SourceProduct}
		configured[PartProduct] = true
	}

	if len(s.skillDirs)+len(s.skillSources) > 0 {
		cat, part, om, err := skillPart(s, offersSkillTool(s))
		if err != nil {
			return err
		}
		k.catalog = cat
		parts[PartSkills] = part
		k.omitted = append(k.omitted, om...)
		if s.skillGrants {
			k.omitted = append(k.omitted, skillRuleProblems(cat)...)
		}
		configured[PartSkills] = true
	}

	order := s.order
	if order == nil {
		order = DefaultOrder
	}
	if s.memStore != nil {
		configured[PartMemory] = true
	}
	if s.agentsMDSet {
		configured[agentsmd.PartID] = true
	}
	if err := checkOrder(order, configured); err != nil {
		return err
	}

	// The budget, when there is one, is spent on the parts that can be
	// bounded. The product's prompt and the skill catalogue cannot: no
	// exported call bounds either, and cutting text the product wrote
	// is not the kit's to do. So they are charged first, and what is
	// left is what the two bounded layers share.
	avail := int64(0)
	if s.budget > 0 {
		fixed := int64(len(parts[PartProduct].Text) + len(parts[PartSkills].Text))
		seps := int64(len(Separator) * max(0, len(configured)-1))
		avail = s.budget - fixed - seps
		if avail < 0 {
			return fmt.Errorf(
				"agentkit: the instruction budget of %d bytes is under the %d bytes of the parts the kit cannot bound (the product prompt and the skill catalogue) plus %d of separators",
				s.budget, fixed, seps)
		}
	}

	if s.agentsMDSet {
		part, om, err := agentsMDPart(s, share(s.budget, avail))
		if err != nil {
			return err
		}
		parts[agentsmd.PartID] = part
		k.omitted = append(k.omitted, om...)
		if s.budget > 0 {
			avail -= int64(len(part.Text))
		}
	}
	if s.memStore != nil {
		part, man, om, err := memoryPart(ctx, s, share(s.budget, avail))
		if err != nil {
			return err
		}
		parts[PartMemory] = part
		k.manifest = man
		k.omitted = append(k.omitted, om...)
	}

	text, kept := join(order, parts)
	k.parts, k.order = kept, order
	k.cfg.Instructions = text
	return nil
}

// dial connects to every MCP server the caller named.
func (k *Kit) dial(ctx context.Context, s *settings) error {
	for i, d := range s.mcp {
		t := d.transport
		if t == nil {
			fields := strings.Fields(d.command)
			if len(fields) == 0 {
				return errors.New("agentkit: WithMCP was given an empty command")
			}
			// Not CommandContext: ctx bounds New, and a server whose
			// process died the moment New returned would offer its
			// tools to exactly no turns.
			t = &sdk.CommandTransport{Command: exec.Command(fields[0], fields[1:]...)}
		}
		remote, err := mcpclient.Connect(ctx, t, d.opts...)
		if err != nil {
			return fmt.Errorf("agentkit: connecting to MCP server %s: %w", d.label(i), err)
		}
		k.remotes = append(k.remotes, remote)
	}
	return nil
}

// label names one server in an error. The position is part of it
// because nothing else is guaranteed to differ: two servers may be the
// same command with different environments, or two transports of one
// type, and a conflict between them that blamed the same string twice
// would not tell an operator which of the two to prefix.
func (d mcpDial) label(i int) string {
	what := d.command
	if what == "" {
		what = fmt.Sprintf("%T", d.transport)
	}
	return fmt.Sprintf("#%d %s", i+1, what)
}

// openSession starts or resumes the session and keeps the recorder.
func (k *Kit) openSession(ctx context.Context, s *settings) error {
	if !s.sessionSet {
		return nil
	}
	var (
		rec  *session.Recorder
		sess *agentsession.Session
		err  error
	)
	if s.sessionResume {
		rec, sess, err = session.Resume(ctx, s.sessionStore, s.sessionID, s.sessionOpts...)
	} else {
		rec, sess, err = session.Start(ctx, s.sessionStore, s.sessionHeader, s.sessionOpts...)
	}
	if err != nil {
		return fmt.Errorf("agentkit: opening the session: %w", err)
	}
	k.rec, k.sess = rec, sess

	// The items in force at the leaf are what an agent continuing this
	// conversation must be seeded with. Reading them can fail, and New
	// is where the failures are reported, so it is read here and not in
	// the accessor.
	sctx, err := sess.Context()
	if err != nil {
		return fmt.Errorf("agentkit: reading the session's context: %w", err)
	}
	k.seed = sctx.Items
	return nil
}

// build fills the rest of the config: the tool set, then the hooks.
func (k *Kit) build(ctx context.Context, s *settings) error {
	if err := k.buildTools(ctx, s); err != nil {
		return err
	}
	k.buildHooks(s)

	k.cfg.Name, k.cfg.Description = s.name, s.description
	k.cfg.Model, k.cfg.ModelName = s.model, s.modelName
	k.cfg.Request, k.cfg.RequestExtra = s.request, s.requestExtra
	k.cfg.Reasoning, k.cfg.Text = s.reasoning, s.text
	k.cfg.MaxTurns = s.maxTurns
	k.cfg.Retry = s.retry
	k.cfg.ToolExecution, k.cfg.MaxParallelTools = s.toolExec, s.maxParallel
	k.cfg.Filter = s.filter
	k.cfg.AfterToolCall = s.afterToolCall
	return nil
}

// buildTools unions the four sources into one namespace, refuses a
// duplicate name found now, and puts the policy's filter in front of
// the result.
func (k *Kit) buildTools(ctx context.Context, s *settings) error {
	ts := &toolSet{onConflict: s.conflict, filter: s.toolFilter}
	if len(s.tools) > 0 {
		ts.sources = append(ts.sources, source{name: "WithTools", tools: s.tools})
	}
	// Their own source, so a collision with a product tool says which
	// of the two to rename rather than blaming one name twice.
	for i, d := range s.deferTools {
		if tools := d.fn(k); len(tools) > 0 {
			ts.sources = append(ts.sources, source{name: d.label(i), tools: tools})
		}
	}
	if k.catalog != nil && offersSkillTool(s) {
		tool := k.catalog.Tool(s.skillToolOpt...)
		if s.skillGrants && k.engine != nil {
			tool = &grantingTool{
				Tool:   tool,
				cat:    k.catalog,
				engine: k.engine,
				source: s.skillSource,
				report: s.skillGrant,
			}
		}
		ts.sources = append(ts.sources, source{name: "WithSkills", tools: []agenttool.Tool{tool}})
	}
	if s.memStore != nil {
		ts.sources = append(ts.sources, source{
			name:  "WithMemory",
			tools: agentmemory.Tools(s.memStore, s.memScopes, s.memTools...),
		})
	}
	for i, remote := range k.remotes {
		label := "mcp:" + s.mcp[i].label(i)
		ts.sources = append(ts.sources, source{name: label, live: func(context.Context) []agenttool.Tool {
			// A closed remote still holds the list it last fetched, and
			// offering the model a tool whose session is gone is worse
			// than offering it nothing: the call reaches the loop and
			// fails there.
			if k.closed.Load() {
				return nil
			}
			return remote.Tools()
		}})
	}
	for i, fn := range s.toolProvide {
		ts.sources = append(ts.sources, source{
			name: fmt.Sprintf("WithToolProvider #%d", i+1),
			live: fn,
		})
	}
	if len(ts.sources) == 0 {
		// Nothing to union and nothing for a policy to filter, so the
		// field stays nil and the loop's own empty set applies.
		return nil
	}

	// A collision the kit can see now is an error, because the two
	// tools are both there and the caller can fix it. A collision that
	// only appears later, when a remote changes its list, is reported
	// through WithToolConflict and the later tool dropped: a provider
	// cannot fail a turn.
	if _, conflicts := ts.resolve(ctx); len(conflicts) > 0 {
		errs := make([]error, len(conflicts))
		for i, c := range conflicts {
			errs[i] = c
		}
		return errors.Join(errs...)
	}

	provider := ts.provider()
	if k.engine != nil {
		provider = k.engine.ToolProvider(provider)
	}
	k.cfg.ToolProvider = provider
	return nil
}

// buildHooks fills the contested fields, in the order the package
// documents. Each is a call to the Chain function agentturn exports for
// that field, which is what a product would write instead.
func (k *Kit) buildHooks(s *settings) {
	// BeforeModelCall: memory re-renders the instructions, then the
	// guards see the text memory injected, then the product's own.
	var before []func(context.Context, *openresponses.Request) error
	if s.memStore != nil {
		before = append(before, k.rerenderMemory(s))
	}
	if len(s.guards) > 0 {
		before = append(before, guard.BeforeModelCall(s.guards...))
	}
	before = append(before, s.beforeModelCall...)
	k.cfg.BeforeModelCall = chain1(before, agentturn.ChainBeforeModelCall)

	// BeforeToolCall: the engine, then the product's own policies.
	var tool []func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error)
	if k.engine != nil {
		tool = append(tool, k.engine.BeforeToolCall())
	}
	tool = append(tool, s.beforeToolCall...)
	k.cfg.BeforeToolCall = chain1(tool, agentturn.ChainBeforeToolCall)

	// OutputGuard: the guards, then the product's own.
	var out []func(context.Context, agentturn.OutputInfo) (*openresponses.Message, error)
	if len(s.guards) > 0 {
		out = append(out, guard.OutputGuard(s.guards...))
	}
	out = append(out, s.outputGuard...)
	k.cfg.OutputGuard = chain1(out, agentturn.ChainOutputGuard)

	// ShouldStopAfterTurn: the policy's guards, then the product's.
	var stop []func(context.Context, agentturn.TurnInfo) (bool, error)
	if len(s.guards) > 0 {
		stop = append(stop, guard.ShouldStopAfterTurn(s.guards...))
	}
	stop = append(stop, s.shouldStop...)
	k.cfg.ShouldStopAfterTurn = chain1(stop, agentturn.ChainShouldStopAfterTurn)

	// BeforeTurn: the product's alone; the kit contests nothing here.
	k.cfg.BeforeTurn = chain1(s.beforeTurn, agentturn.ChainBeforeTurn)

	// Transform: compaction, with the fold bound to the recorder when
	// there is one. There is no chain for this field, so the product's
	// own Transform and compaction are mutually exclusive; New refuses
	// both.
	switch {
	case s.transform != nil:
		k.cfg.Transform = s.transform
	case s.compactSet:
		opts := s.compactOpts
		if k.rec != nil {
			opts = append(append([]compact.Option(nil), opts...), compact.WithOnFold(k.rec.Fold))
		}
		var t *compact.Transform
		if s.compactor != nil {
			t = compact.New(s.compactor, opts...)
		} else {
			model := s.compactModel
			if model == nil {
				model = s.model
			}
			// The agent's model name is the default and the caller's
			// options come after it, so WithModel in WithCompaction
			// still wins.
			local := append([]compact.Option{compact.WithModel(s.modelName)}, opts...)
			t = compact.NewLocal(model, local...)
		}
		k.cfg.Transform = t.Transform
	}
}

// chain1 returns nil for no hooks, the one hook for one, and the
// package's own chain for more, so a config field a product did not
// contest stays nil and the loop's own default applies.
func chain1[T any](fns []T, chain func(...T) T) T {
	var zero T
	switch len(fns) {
	case 0:
		return zero
	case 1:
		return fns[0]
	default:
		return chain(fns...)
	}
}

// rerenderMemory is the BeforeModelCall hook that puts the freshest
// memory in front of the model each turn. It re-renders the block,
// rebuilds the instructions from the parts with the new one in place,
// and, when a session is recorded, writes the manifest under
// [agentmemory.ManifestNS] the first time each render appears.
//
// It is a BeforeModelCall and not a Transform because a Transform
// cannot reach the instructions and what it injects is not recorded.
func (k *Kit) rerenderMemory(s *settings) func(context.Context, *openresponses.Request) error {
	return func(ctx context.Context, req *openresponses.Request) error {
		// The same render, the same bound and the same drop as the
		// first one, so the block cannot grow past the budget between
		// turns and cannot come back at its floor after the budget
		// dropped it.
		part, man, _, err := memoryPart(ctx, s, k.memoryShare(s))
		if err != nil {
			return err
		}

		k.mu.Lock()
		parts := make([]Part, 0, len(k.parts)+1)
		var replaced bool
		for _, p := range k.parts {
			if p.ID != PartMemory {
				parts = append(parts, p)
				continue
			}
			replaced = true
			if part.Text != "" {
				parts = append(parts, part)
			}
		}
		if !replaced && part.Text != "" {
			// The first render came out empty and was not joined, so
			// the part has no position to be replaced at. Put it back
			// where the order says it goes.
			parts = insertByOrder(parts, part, k.order)
		}
		k.parts = parts
		k.manifest = man
		k.mu.Unlock()

		req.Instructions = agentsession.JoinInstructions(parts)
		return k.record(ctx, man)
	}
}

// record writes the memory manifest to the session the first time each
// render appears, and does nothing when there is no session.
//
// The comparison, the write and the remembering are one critical
// section. Two turns that rendered different manifests must not write
// their annotations in the other order, which would leave the session's
// last one describing a render that is no longer in force; and a write
// that failed must be attempted again next turn rather than remembered
// as done.
func (k *Kit) record(ctx context.Context, man agentmemory.Manifest) error {
	if k.rec == nil {
		return nil
	}
	hash := man.Hash()

	k.recmu.Lock()
	defer k.recmu.Unlock()
	if hash == k.recorded {
		return nil
	}
	ns, data := man.Record()
	if err := k.rec.Annotate(ctx, ns, json.RawMessage(data)); err != nil {
		return fmt.Errorf("agentkit: recording the memory manifest: %w", err)
	}
	k.recorded = hash
	return nil
}

// memoryShare is what is left of the budget for the memory block this
// turn: the budget less every other part and the separator each of them
// costs. The other parts do not change after New, so this is the same
// number every turn unless the caller's budget is zero, in which case
// there is no bound and the layer keeps its own.
func (k *Kit) memoryShare(s *settings) int64 {
	if s.budget <= 0 {
		return 0
	}
	k.mu.Lock()
	var other int64
	for _, p := range k.parts {
		if p.ID != PartMemory {
			other += int64(len(p.Text)) + int64(len(Separator))
		}
	}
	k.mu.Unlock()
	return share(s.budget, s.budget-other)
}

// insertByOrder puts a part back at the position the order gives it,
// among the parts that are there.
func insertByOrder(parts []Part, p Part, order []string) []Part {
	rank := map[string]int{}
	for i, id := range order {
		rank[id] = i
	}
	at := len(parts)
	for i, existing := range parts {
		if rank[existing.ID] > rank[p.ID] {
			at = i
			break
		}
	}
	return append(parts[:at:at], append([]Part{p}, parts[at:]...)...)
}

// Config is the assembled configuration. It is a plain
// [agentturn.Config]: every field a product could have set by hand,
// with the same values, by calling the same exported functions. It is
// pure and may be called once per run.
func (k *Kit) Config() agentturn.Config { return k.cfg }

// Parts are the instructions as named parts, in the order they were
// joined. Their texts joined with [Separator] are
// [agentturn.Config.Instructions], so they may be handed to
// [agentsession.ConfigFromRequestParts] as they are. The memory part is
// replaced on each model call, so a caller that wants the parts of a
// particular request reads them from the request.
//
// A configured session does not record them on its own. The recorder
// writes its config entries through [agentsession.ConfigFromRequest],
// which takes the joined string, so a product that wants
// instructions_parts in the session hands these to
// [agentsession.ConfigFromRequestParts] itself.
func (k *Kit) Parts() []Part {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]Part, len(k.parts))
	copy(out, k.parts)
	return out
}

// Omitted is everything the layers considered for the instructions and
// left out: the files the AGENTS.md budget dropped and the names a
// preferred file shadowed, the memory entries that did not fit, the
// skills that would not load and the ones the catalogue will not offer.
// One list, because a product wants one and a session's
// instructions_omitted is one.
func (k *Kit) Omitted() []Omission {
	out := make([]Omission, len(k.omitted))
	copy(out, k.omitted)
	return out
}

// OmittedParts is [Kit.Omitted] in the shape the session format
// records.
func (k *Kit) OmittedParts() []agentsession.OmittedPart {
	out := make([]agentsession.OmittedPart, 0, len(k.omitted))
	for _, o := range k.omitted {
		out = append(out, o.OmittedPart())
	}
	return out
}

// Engine is the policy engine, or nil when no policy was configured. A
// front reads [agentpolicy.Engine.Deferred] and calls
// [agentpolicy.Engine.Release] through it.
func (k *Kit) Engine() *agentpolicy.Engine { return k.engine }

// Catalog is the skill catalogue, or nil when no skills were
// configured.
func (k *Kit) Catalog() *agentskill.Catalog { return k.catalog }

// Recorder is the session recorder, or nil when no session was
// configured. Attach it to the agent with
// [session.Recorder.Attach], or use [Kit.Attach].
func (k *Kit) Recorder() *session.Recorder { return k.rec }

// Session is the session [New] started or resumed, or nil. After a
// resume its Context().Items is what the agent should be seeded with.
func (k *Kit) Session() *agentsession.Session { return k.sess }

// Transcript is the conversation in force at the session's leaf, and
// what an agent continuing it must be seeded with. It is empty for a
// session [WithSession] started and for no session at all, so a caller
// need not branch on which:
//
//	agent := agentturn.New(kit.Config(), agentturn.WithTranscript(kit.Transcript()))
func (k *Kit) Transcript() agentturn.Transcript {
	return append(agentturn.Transcript(nil), k.seed...)
}

// SessionID is the ID of the session, or "" when there is none.
func (k *Kit) SessionID() string {
	if k.rec == nil {
		return ""
	}
	return k.rec.SessionID()
}

// Attach subscribes the recorder to the agent and returns the
// unsubscribe. It returns a no-op when there is no session, so a caller
// need not branch on one.
func (k *Kit) Attach(a *agentturn.Agent) func() {
	if k.rec == nil {
		return func() {}
	}
	return k.rec.Attach(a)
}

// MemoryManifest is what the last render put in the memory block, and
// the hash a product compares to record the render only when it moved.
// The kit records it itself when a session is configured.
func (k *Kit) MemoryManifest() agentmemory.Manifest {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.manifest
}

// Close releases what [New] opened, in reverse order, joining the
// errors: the MCP clients. The stores a caller passed in — the memory
// store, the session store — stay the caller's to sync, release and
// close, since the kit did not open them.
//
// A config handed out before Close keeps working and stops offering
// the closed servers' tools, so a run that outlives the kit is offered
// what it can still reach. Close is safe to call twice.
func (k *Kit) Close() error {
	k.closed.Store(true)
	var errs []error
	for i := len(k.remotes) - 1; i >= 0; i-- {
		if err := k.remotes[i].Close(); err != nil {
			errs = append(errs, err)
		}
	}
	k.remotes = nil
	return errors.Join(errs...)
}
