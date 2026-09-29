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
	order   []string
	omitted []Omission
	origins []ToolOrigin

	engine  *agentpolicy.Engine
	catalog *agentskill.Catalog
	grants  *skillGrants
	rec     *session.Recorder
	sess    *agentsession.Session
	seed    agentturn.Transcript
	remotes []*mcpclient.Remote

	// ownRec is true when the kit opened the recorder, and so is the one
	// to attach it; false under WithRecorder, whose owner attaches it.
	ownRec bool
	// memory is true when WithMemory is configured, which a child agent
	// reads to decide whether to bridge its session ID to agentmemory's
	// context key.
	memory bool

	// closed is read by the per-turn tool provider, which a config
	// handed out before Close may still be called through.
	closed atomic.Bool

	// mu guards the parts, the memory manifest and the memory
	// omissions, which the per-turn hook replaces. base is the parts as
	// the layers rendered them; parts is what the last request was sent,
	// base with each part as the guards left it. They are the same slice
	// until a guard runs.
	mu         sync.Mutex
	base       []Part
	parts      []Part
	manifest   agentmemory.Manifest
	memOmitted []Omission

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
	if s.recorder != nil && s.sessionSet {
		return nil, errors.New("agentkit: WithRecorder and a session option both set; the kit records into the recorder it is given or opens its own")
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

	k := &Kit{memory: s.memStore != nil}
	fail := func(err error) (*Kit, error) {
		return nil, errors.Join(err, k.Close())
	}

	engine, err := k.buildEngine(&s)
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

// buildEngine builds the engine WithPolicy asked for, with the kit's
// observer ahead of the caller's options. The engine keeps every
// observer, so one the caller passes there runs after the kit's.
func (k *Kit) buildEngine(s *settings) (*agentpolicy.Engine, error) {
	if s.engine != nil {
		return s.engine, nil
	}
	if !s.policySet {
		return nil, nil
	}
	var opts []agentpolicy.Option
	if s.sessionSet || s.recorder != nil || s.verdicts != nil {
		opts = append(opts, agentpolicy.WithObserver(k.observeVerdicts(s, false)))
	}
	opts = append(opts, s.engineOpts...)
	e, err := agentpolicy.Build(s.policy, s.matchers, opts...)
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
		k.memOmitted = om
	}

	text, kept := join(order, parts)
	k.base, k.parts, k.order = kept, kept, order
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

// openSession starts or resumes the session and keeps the recorder,
// or keeps the recorder WithRecorder gave it and opens nothing.
func (k *Kit) openSession(ctx context.Context, s *settings) error {
	if s.recorder != nil {
		k.rec = s.recorder
		return nil
	}
	if !s.sessionSet {
		return nil
	}
	var (
		rec  *session.Recorder
		sess *agentsession.Session
		err  error
	)
	// The kit's parts first, so a WithInstructionsParts the caller
	// passed replaces them.
	opts := append([]session.Option{session.WithInstructionsParts(k.PartsFor)}, s.sessionOpts...)
	if s.sessionResume {
		rec, sess, err = session.Resume(ctx, s.sessionStore, s.sessionID, opts...)
	} else {
		rec, sess, err = session.Start(ctx, s.sessionStore, s.sessionHeader, opts...)
	}
	if err != nil {
		return fmt.Errorf("agentkit: opening the session: %w", err)
	}
	k.rec, k.sess, k.ownRec = rec, sess, true

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

	// ToolRecorder: the session's, when there is one, so a record a
	// tool writes with agenttool.WriteRecord while it runs lands in the
	// session beside its call. Without a session it stays nil, and the
	// loop then honours a recorder the product installed on the
	// prompt's context.
	if k.rec != nil {
		k.cfg.ToolRecorder = k.rec.RecordFunc()
	}

	// ToolElicitor: the product's, around the recorder's when there is
	// one, so a question and its answer are written under the call.
	if s.elicitor != nil {
		k.cfg.ToolElicitor = s.elicitor
		if k.rec != nil {
			k.cfg.ToolElicitor = k.rec.Elicitor(s.elicitBy, s.elicitor)
		}
	}
	return nil
}

// buildTools unions the four sources into one namespace, refuses a
// duplicate name found now, and puts the policy's filter in front of
// the result.
func (k *Kit) buildTools(ctx context.Context, s *settings) error {
	ts := &toolSet{onConflict: s.conflict, filter: s.toolFilter, wrap: s.toolWrap}
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
		src := source{name: "WithSkills", tools: []agenttool.Tool{k.catalog.Tool(s.skillToolOpt...)}}
		if s.skillGrants && k.engine != nil {
			k.grants = &skillGrants{
				cat:    k.catalog,
				engine: k.engine,
				source: s.skillSource,
				report: s.skillGrant,
			}
			src.own = k.grants.wrap
		}
		ts.sources = append(ts.sources, src)
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

	ts.prepare()

	// A collision the kit can see now is an error, because the two
	// tools are both there and the caller can fix it. A collision that
	// only appears later, when a remote changes its list, is reported
	// through WithToolConflict and the later tool dropped: a provider
	// cannot fail a turn.
	_, origins, conflicts := ts.resolve(ctx)
	if len(conflicts) > 0 {
		errs := make([]error, len(conflicts))
		for i, c := range conflicts {
			errs[i] = c
		}
		return errors.Join(errs...)
	}
	k.origins = origins

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
	var guards guard.Chain
	if len(s.guards) > 0 {
		guards = guard.Chain{Guards: s.guards}
		if k.rec != nil || s.verdicts != nil {
			guards.Observer = k.observeVerdicts(s, true)
		}
	}

	// BeforeModelCall: the instructions, memory re-rendered and each
	// part guarded, then the guards over the whole request, then the
	// product's own.
	var before []func(context.Context, *openresponses.Request) error
	if s.memStore != nil || (len(s.guards) > 0 && len(k.base) > 0) {
		before = append(before, k.instructions(s, guards))
	}
	if len(s.guards) > 0 {
		before = append(before, guards.BeforeModelCall())
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
		out = append(out, guards.OutputGuard())
	}
	out = append(out, s.outputGuard...)
	k.cfg.OutputGuard = chain1(out, agentturn.ChainOutputGuard)

	// ShouldStopAfterTurn: the policy's guards, then the product's.
	var stop []func(context.Context, agentturn.TurnInfo) (bool, error)
	if len(s.guards) > 0 {
		stop = append(stop, guards.ShouldStopAfterTurn())
	}
	stop = append(stop, s.shouldStop...)
	k.cfg.ShouldStopAfterTurn = chain1(stop, agentturn.ChainShouldStopAfterTurn)

	// BeforeTurn: the skill grants' scope, then the product's own.
	var turn []func(context.Context, agentturn.TurnStartInfo) (openresponses.Items, error)
	if s.skillGrantScope && k.grants != nil {
		turn = append(turn, k.revokeOnRunStart)
	}
	turn = append(turn, s.beforeTurn...)
	k.cfg.BeforeTurn = chain1(turn, agentturn.ChainBeforeTurn)

	// Transform: the product's, then compaction, with the fold bound to
	// the recorder when there is one, so the fold is over what the
	// product shaped.
	var transforms []func(context.Context, agentturn.Transcript) (agentturn.Transcript, error)
	if s.transform != nil {
		transforms = append(transforms, s.transform)
	}
	if s.compactSet {
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
		transforms = append(transforms, t.Transform)
	}
	k.cfg.Transform = chain1(transforms, agentturn.ChainTransform)
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

// instructions is the BeforeModelCall hook that builds the request's
// instructions from the parts: it re-renders the memory block, when
// there is one, so the model sees the freshest memory each turn, runs
// the input guards over each part, when there are any, so a rewrite
// lands in the part it belongs to, and joins what they left. When a
// session is recorded, the manifest is written under
// [agentmemory.ManifestNS] the first time each render appears.
//
// It is a BeforeModelCall and not a Transform because a Transform
// cannot reach the instructions and what it injects is not recorded.
func (k *Kit) instructions(s *settings, guards guard.Chain) func(context.Context, *openresponses.Request) error {
	return func(ctx context.Context, req *openresponses.Request) error {
		k.mu.Lock()
		base := k.base
		k.mu.Unlock()

		// Without memory the parts never change, so a request whose
		// instructions are not their join is one a product rewrote,
		// through Agent.SetConfig or a hook of its own, and the guards
		// see it whole, after this, as they would without the kit.
		if s.memStore == nil && req.Instructions != agentsession.JoinInstructions(base) {
			return nil
		}

		var (
			man    agentmemory.Manifest
			memOm  []Omission
			render = s.memStore != nil
		)
		if render {
			// The same render, the same bound and the same drop as the
			// first one, so the block cannot grow past the budget
			// between turns and cannot come back at its floor after the
			// budget dropped it.
			part, m, om, err := memoryPart(ctx, s, k.memoryShare(s))
			if err != nil {
				return err
			}
			base, man, memOm = withPart(base, part, k.order), m, om
			// Recorded before the guards run: the manifest says what the
			// render put in the block, which a guard that blocks the
			// request does not change.
			if err := k.record(ctx, man); err != nil {
				return err
			}
		}

		sent := base
		if len(guards.Guards) > 0 {
			var err error
			if sent, err = guardParts(ctx, guards, base); err != nil {
				return err
			}
		}

		k.mu.Lock()
		k.base, k.parts = base, sent
		if render {
			k.manifest, k.memOmitted = man, memOm
		}
		k.mu.Unlock()

		req.Instructions = agentsession.JoinInstructions(sent)
		return nil
	}
}

// withPart returns parts with p in place of the part of its ID, or
// inserted where the order puts it when the first render came out empty
// and was not joined. An empty p is dropped rather than joined. parts
// is not modified.
func withPart(parts []Part, p Part, order []string) []Part {
	out := make([]Part, 0, len(parts)+1)
	var replaced bool
	for _, existing := range parts {
		if existing.ID != p.ID {
			out = append(out, existing)
			continue
		}
		replaced = true
		if p.Text != "" {
			out = append(out, p)
		}
	}
	if !replaced && p.Text != "" {
		out = insertByOrder(out, p, order)
	}
	return out
}

// guardParts runs the input guards over each part on its own, as an
// [guard.Input] carrying the part's text and no items, and returns the
// parts as the guards left them. A part a guard emptied is dropped, so
// the join of what is returned is still the instructions sent. Each
// verdict reaches the chain's observer with Subject naming the part,
// "instructions/<id>", since the chain itself does not know it.
func guardParts(ctx context.Context, c guard.Chain, parts []Part) ([]Part, error) {
	out := make([]Part, 0, len(parts))
	for _, p := range parts {
		pc := c
		if obs := c.Observer; obs != nil {
			subject := "instructions/" + p.ID
			pc.Observer = func(ctx context.Context, v agentpolicy.Verdict) {
				v.Subject = subject
				obs(ctx, v)
			}
		}
		req := openresponses.Request{Instructions: p.Text}
		if err := pc.BeforeModelCall()(ctx, &req); err != nil {
			return nil, err
		}
		if req.Instructions == "" {
			continue
		}
		p.Text = req.Instructions
		out = append(out, p)
	}
	return out, nil
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
	if _, err := k.rec.Annotate(ctx, ns, json.RawMessage(data)); err != nil {
		return fmt.Errorf("agentkit: recording the memory manifest: %w", err)
	}
	k.recorded = hash
	return nil
}

// observeVerdicts is the observer the kit gives the engine it builds
// and the guard chain: it writes each verdict to the session, when
// there is one, under [agentpolicy.VerdictNS], and hands it to the
// product's observer. A guard's bare allow is not written, as a hook's
// allow with no reason is not: guards run on every part and every
// message, and their silence would outweigh the run.
//
// An observer cannot fail the decision it observes, so a write that
// fails is lost, as it is for any observer; the recorder's own writes
// for the run fail the run first.
func (k *Kit) observeVerdicts(s *settings, guards bool) func(context.Context, agentpolicy.Verdict) {
	product := s.verdicts
	return func(ctx context.Context, v agentpolicy.Verdict) {
		if k.rec != nil && (!guards || v.Action != agentturn.Allow || v.Reason != "") {
			ns, data := v.Record()
			_, _ = k.rec.Annotate(ctx, ns, json.RawMessage(data))
		}
		if product != nil {
			product(ctx, v)
		}
	}
}

// revokeOnRunStart is the BeforeTurn hook WithSkillGrantScope installs:
// on the first turn of a run that a new user message started, it
// revokes every grant a skill's read made, so a grant lasts until the
// next message.
//
// Every run's first turn is turn 1, a Resume's and a Continue's as
// well, and neither is a new message: a Resume after an approval is the
// same task going on, and revoking there would take a grant away in the
// middle of it. TurnStartInfo does not say what started the run, so the
// test is the transcript's: a run from a new message has that message
// last when its first turn starts, a Resume has the answered calls'
// outputs, and a Continue has whatever the last run left.
func (k *Kit) revokeOnRunStart(ctx context.Context, info agentturn.TurnStartInfo) (openresponses.Items, error) {
	if info.Turn == 1 && endsWithUserMessage(info.Transcript) {
		k.RevokeSkillGrants(ctx)
	}
	return nil, nil
}

// endsWithUserMessage reports whether the last item of tr is a message
// from the user, hidden or not.
func endsWithUserMessage(tr agentturn.Transcript) bool {
	if len(tr) == 0 {
		return false
	}
	item, _ := agentturn.Unhide(tr[len(tr)-1])
	m, ok := item.(*openresponses.Message)
	return ok && m.Role == openresponses.RoleUser
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
	for _, p := range k.base {
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
// joined, as the last request was sent them: after the memory block
// was re-rendered and after the input guards ran over each part, so a
// part [guard.Redact] rewrote holds the rewritten text. Their texts
// joined with [Separator] are the instructions that request carried,
// and before the first request they are [agentturn.Config.Instructions],
// so they may be handed to [agentsession.ConfigFromRequestParts] as
// they are.
//
// A guard that rewrites the joined instructions in its pass over the
// whole request, or a product hook after the kit's, leaves the parts
// describing the text before it, and [Kit.PartsFor] then says so.
func (k *Kit) Parts() []Part {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]Part, len(k.parts))
	copy(out, k.parts)
	return out
}

// PartsFor returns the parts req's instructions are composed of and
// what the layers left out, or nil and nil when [Kit.Parts] do not join
// to req.Instructions: a request a hook after the kit's rewrote, or one
// built from a render other than the last. Its signature is the one
// [session.WithInstructionsParts] takes, and a session the kit opens is
// given it, so the recorder's config entries carry instructions_parts
// and instructions_omitted. A recorder opened elsewhere, the one
// [WithRecorder] is given, takes it the same way:
//
//	var kit *agentkit.Kit
//	rec, _, err := session.Start(ctx, store, h,
//		session.WithInstructionsParts(func(req openresponses.Request) ([]agentsession.InstructionPart, []agentsession.OmittedPart) {
//			return kit.PartsFor(req)
//		}))
//	kit, err = agentkit.New(ctx, agentkit.WithRecorder(rec), ...)
//
// Concurrent runs off one kit share the last render, so a request from
// the other run's render gets nil and is recorded as a string, which is
// what the recorder does with parts that do not join.
func (k *Kit) PartsFor(req openresponses.Request) ([]agentsession.InstructionPart, []agentsession.OmittedPart) {
	k.mu.Lock()
	parts := k.parts
	if agentsession.JoinInstructions(parts) != req.Instructions {
		// The configuration's own instructions, which the recorder
		// settles at a run's start before any hook has run, are the
		// layers' join.
		if parts = k.base; agentsession.JoinInstructions(parts) != req.Instructions {
			k.mu.Unlock()
			return nil, nil
		}
	}
	out := make([]Part, len(parts))
	copy(out, parts)
	// The omissions under the same lock, so they are the same render's.
	omitted := make([]agentsession.OmittedPart, 0, len(k.omitted)+len(k.memOmitted))
	for _, o := range k.omitted {
		omitted = append(omitted, o.OmittedPart())
	}
	for _, o := range k.memOmitted {
		omitted = append(omitted, o.OmittedPart())
	}
	k.mu.Unlock()
	return out, omitted
}

// Omitted is everything the layers considered for the instructions and
// left out: the files the AGENTS.md budget dropped and the names a
// preferred file shadowed, the memory entries the last render did not
// fit, the skills that would not load and the ones the catalogue will
// not offer. One list, because a product wants one and a session's
// instructions_omitted is one.
func (k *Kit) Omitted() []Omission {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]Omission, 0, len(k.omitted)+len(k.memOmitted))
	out = append(out, k.omitted...)
	return append(out, k.memOmitted...)
}

// OmittedParts is [Kit.Omitted] in the shape the session format
// records.
func (k *Kit) OmittedParts() []agentsession.OmittedPart {
	omitted := k.Omitted()
	out := make([]agentsession.OmittedPart, 0, len(omitted))
	for _, o := range omitted {
		out = append(out, o.OmittedPart())
	}
	return out
}

// Tools lists the tools in the union at [New], after [WithToolFilter]
// and [WithToolWrap] and before the policy's filter, each with the
// source it came from. It is how a product names the tools a library
// made, the skill tool, the memory tools, an MCP server's, a child's,
// to a policy whose default asks, since the engine is built before the
// union exists:
//
//	var allow []agentpolicy.Rule
//	for _, t := range kit.Tools() {
//		if t.Source != "WithTools" {
//			allow = append(allow, agentpolicy.Rule{Tool: t.Name, Source: libraries})
//		}
//	}
//
// The kit does not allow them itself: whether a library's tool runs
// unasked is the product's decision. An MCP server's list and a
// provider's are fetched each turn, so a tool they add after New is
// not here.
func (k *Kit) Tools() []ToolOrigin {
	return append([]ToolOrigin(nil), k.origins...)
}

// RevokeSkillGrants revokes every grant a skill's read made through
// [WithSkillGrants] and returns the number of rules the engine removed.
// A front calls it when a grant should end, the end of a run or of a
// conversation; [WithSkillGrantScope] calls it when each run starts. A
// skill read again is granted again. It is zero and does nothing
// without skill grants.
func (k *Kit) RevokeSkillGrants(ctx context.Context) int {
	if k.grants == nil {
		return 0
	}
	return k.grants.revoke(ctx)
}

// Engine is the policy engine, or nil when no policy was configured. A
// front reads [agentpolicy.Engine.Deferred] and calls
// [agentpolicy.Engine.Release] through it.
func (k *Kit) Engine() *agentpolicy.Engine { return k.engine }

// Catalog is the skill catalogue, or nil when no skills were
// configured.
func (k *Kit) Catalog() *agentskill.Catalog { return k.catalog }

// Recorder is the session recorder, or nil when no session was
// configured: the one the kit opened, or the one [WithRecorder] gave
// it. Attach one the kit opened to the agent with
// [session.Recorder.Attach], or use [Kit.Attach].
func (k *Kit) Recorder() *session.Recorder { return k.rec }

// Session is the session [New] started or resumed, or nil, as it is
// under [WithRecorder]. After a resume its Context().Items is what the
// agent should be seeded with.
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
// need not branch on one, and under [WithRecorder], whose recorder its
// owner attaches: a second subscription would write every event twice.
func (k *Kit) Attach(a *agentturn.Agent) func() {
	if k.rec == nil || !k.ownRec {
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
