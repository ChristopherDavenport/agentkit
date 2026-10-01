package agentkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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
// prompt the other assembled, and each run's memory_save is based on
// that run's own render. What they share is [Kit.Parts] and
// [Kit.MemoryManifest], which describe the render that happened last,
// and the record of the manifest, which is written once per distinct
// render because one session records both runs. A front serving one kit
// to many conversations, each its own session, prompts each run under
// [ContextWithRecorder] so what the kit records lands in the right one.
//
// That is the right sharing for concurrent runs of one agent and the
// wrong sharing for two agents, which want two prompts, two manifests
// and usually two sessions. Give each agent its own kit.
//
// # Handoffs
//
// A handoff between two agents is [agentturn.Agent.SetConfig] from one
// kit's config to the other's, over one transcript. To record both
// agents' runs in one session with each one's instructions as parts,
// either the host opens the recorder with a parts function that asks
// both kits, [PartsFrom], and builds each kit under [WithRecorder]:
//
//	var triage, billing *agentkit.Kit
//	rec, _, err := session.Start(ctx, store, h,
//		session.WithInstructionsParts(agentkit.PartsFrom(&triage, &billing)))
//	triage, err = agentkit.New(ctx, agentkit.WithRecorder(rec), ...)
//	billing, err = agentkit.New(ctx, agentkit.WithRecorder(rec), ...)
//
// or the receiver's kit opens a session of its own with [WithSession]
// on a header whose Base is the sender's leaf, a fork, and the agent is
// seeded with its [Kit.AgentOptions]. A kit built under
// WithRecorder(sender.Recorder()) alone records the receiver's
// instructions as one string, since that recorder's parts function is
// the sender's.
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
	seedOps []agentturn.Option
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

	// union is the tools the provider last returned, by name, before the
	// policy's filter: what [Kit.LookupTool] reads, and through it the
	// engine the kit builds, which reads a call's siblings' tools for
	// the batch hold before the loop hands it their calls.
	union atomic.Pointer[map[string]agenttool.Tool]

	// mu guards the parts, the memory manifest and the memory
	// omissions, which the per-turn hook replaces. base is the parts as
	// the layers rendered them; parts is what the last request was sent,
	// base with each part as the guards left it; first is the parts of
	// the configuration's own instructions, as the guards left New's
	// render, which never changes. They are the same slice until a
	// guard runs or memory re-renders.
	mu    sync.Mutex
	base  []Part
	parts []Part
	first []Part
	// firstOmitted is the memory omissions of New's render, which go
	// with first.
	firstOmitted []Omission
	manifest     agentmemory.Manifest
	memOmitted   []Omission
	// rendered is each run's last render, keyed by the run's ID, which is
	// what memory_save is based on for a call in that run: the block its
	// model composed from, not whichever run rendered last. saveBase is
	// the render a memory_save call was composed from, keyed by the call's
	// ID, kept by the kit's BeforeToolCall hook when the call is decided,
	// so a save held for approval, by any engine or hook, and run by a
	// Resume, a run of its own that has not rendered, is still based on
	// it.
	rendered bounded[agentmemory.Manifest]
	saveBase bounded[agentmemory.Manifest]

	// recmu guards recorded, the manifest last written to each session
	// and the entry that holds it, keyed by the session's ID, and is held
	// across the write. A later write may be a delta on it,
	// agentmemory.Manifest.RecordSince; see Kit.record. It is keyed by session because one recorder writes several:
	// a kit under WithRecorder inside a parent's child agent annotates a
	// new child session on each call, and each must carry its own
	// manifest. It is its own lock for two reasons: the comparison, the
	// write and the remembering have to be one step, or two turns
	// rendering different manifests can write their annotations in the
	// other order and leave the session's last one describing a render
	// that is no longer in force; and holding mu across a store would
	// block Kit.Parts on a front's own thread.
	recmu    sync.Mutex
	recorded map[string]recordedManifest
}

// recordedManifest is a manifest the kit wrote to a session, or found in
// force there, and the ID of the entry that holds it.
type recordedManifest struct {
	man   agentmemory.Manifest
	entry string
}

// recordedSessions bounds the sessions the kit remembers a manifest for.
// Past it the memory is dropped and every session's next render is
// written again, which costs an entry and loses nothing.
const recordedSessions = 1024

// renderedRuns bounds the runs, and the memory_save calls, the kit
// remembers a render for. Past it the oldest is dropped, and a save
// whose render went with it is based on the manifest the session's path
// had in force at the call, or refused when there is none.
const renderedRuns = 1024

// bounded is a map that keeps its last max keys, dropping the oldest
// put first. The zero value holds renderedRuns.
type bounded[V any] struct {
	m     map[string]boundedEntry[V]
	order []boundedKey
	seq   uint64
}

type boundedEntry[V any] struct {
	v   V
	seq uint64
}

type boundedKey struct {
	k   string
	seq uint64
}

func (b *bounded[V]) get(k string) (V, bool) {
	e, ok := b.m[k]
	return e.v, ok
}

func (b *bounded[V]) put(k string, v V) {
	if b.m == nil {
		b.m = map[string]boundedEntry[V]{}
	}
	b.seq++
	b.m[k] = boundedEntry[V]{v: v, seq: b.seq}
	b.order = append(b.order, boundedKey{k: k, seq: b.seq})
	// order holds a mark per put, and a key put again or deleted leaves
	// a stale mark behind, which eviction passes over. Every live key has
	// its latest mark in order, so the loop ends before order does.
	for len(b.m) > renderedRuns {
		old := b.order[0]
		b.order = b.order[1:]
		if e, ok := b.m[old.k]; ok && e.seq == old.seq {
			delete(b.m, old.k)
		}
	}
	if len(b.order) > 2*renderedRuns {
		live := b.order[:0]
		for _, o := range b.order {
			if e, ok := b.m[o.k]; ok && e.seq == o.seq {
				live = append(live, o)
			}
		}
		b.order = live
	}
}

func (b *bounded[V]) delete(k string) { delete(b.m, k) }

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
	if s.compactor != nil && (s.compactLocal || s.compactModel != nil) {
		return nil, errors.New("agentkit: WithCompactor beside WithCompaction or WithCompactionModel; the kit folds through a compactor or with a local summary")
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
	if k.grants != nil && k.sess != nil {
		k.grants.gmu.Lock()
		k.grants.regrant(ctx, k.sess, k.grants.scoped)
		k.grants.gmu.Unlock()
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
// options ahead of the caller's: the tool lookup, the observer, and the
// product's BeforeToolCall hooks. The lookup is one field, so the
// caller's agentpolicy.WithTools replaces it; the engine keeps every
// observer and every hook, so the caller's run after the kit's.
func (k *Kit) buildEngine(s *settings) (*agentpolicy.Engine, error) {
	if s.engine != nil {
		return s.engine, nil
	}
	if !s.policySet {
		return nil, nil
	}
	// The union does not exist yet, so the engine is handed the kit's
	// lookup, which reads it as of the current turn.
	// The observer is bound whether or not the kit has a session: a run
	// served under a recorder on its context, ContextWithRecorder, is
	// recorded there.
	opts := []agentpolicy.Option{
		agentpolicy.WithTools(k.LookupTool),
		agentpolicy.WithObserver(k.observeVerdicts(s, false)),
	}
	// The product's hooks are folded into the engine's decision, before
	// the batch hold, rather than chained after it: a hook that asks
	// about a call then holds its siblings, where chained after the
	// engine it let them run before anyone answered.
	opts = append(opts, agentpolicy.WithHooks(s.beforeToolCall...))
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
	parts := map[string][]Part{}
	configured := map[string]bool{}

	if s.instructions != "" {
		parts[PartProduct] = single(Part{ID: PartProduct, Text: s.instructions, Source: SourceProduct})
		configured[PartProduct] = true
	}

	if len(s.skillDirs)+len(s.skillSources) > 0 {
		cat, part, om, err := skillPart(s, offersSkillTool(s))
		if err != nil {
			return err
		}
		if cat != nil {
			k.catalog = cat
			parts[PartSkills] = single(part)
			k.omitted = append(k.omitted, om...)
			if s.skillGrants {
				k.omitted = append(k.omitted, skillRuleProblems(cat)...)
			}
			configured[PartSkills] = true
		}
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
		fixed := int64(len(agentsession.JoinInstructions(parts[PartProduct])) + len(agentsession.JoinInstructions(parts[PartSkills])))
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
		parts[agentsmd.PartID] = single(part)
		k.omitted = append(k.omitted, om...)
		if s.budget > 0 {
			avail -= int64(len(part.Text))
		}
	}
	if s.memStore != nil {
		group, man, om, err := memoryPart(ctx, s, share(s.budget, avail))
		if err != nil {
			return err
		}
		parts[PartMemory] = group
		k.manifest = man
		k.memOmitted, k.firstOmitted = om, om
	}

	text, kept := join(order, parts)
	sent := kept
	if len(s.guards) > 0 && len(kept) > 0 {
		// The configuration's instructions are what the recorder settles
		// a run's first config entry from, before any hook has run, so
		// they are guarded here as each turn's are: a secret Redact keeps
		// from the model is kept from the record too. No observer: there
		// is no run to record a verdict under, and the first turn's pass
		// reports the same verdicts.
		var err error
		if sent, err = guardParts(ctx, guard.Chain{Guards: s.guards}, kept); err != nil {
			return fmt.Errorf("agentkit: an input guard refused the instructions: %w", err)
		}
		text = agentsession.JoinInstructions(sent)
	}
	k.base, k.parts, k.first, k.order = kept, sent, sent, order
	k.cfg.Instructions = text
	return nil
}

// dial connects to every MCP server the caller named.
func (k *Kit) dial(ctx context.Context, s *settings) error {
	// Each server's stderr is copied on a goroutine of its own, so the
	// product's writer is shared behind one lock.
	var stderr io.Writer
	if s.mcpStderr != nil {
		stderr = &lockedWriter{w: s.mcpStderr}
	}
	for i, d := range s.mcp {
		t := d.transport
		var tail *stderrTail
		if t == nil {
			fields := strings.Fields(d.command)
			if len(fields) == 0 {
				return errors.New("agentkit: WithMCP was given an empty command")
			}
			// Not CommandContext: ctx bounds New, and a server whose
			// process died the moment New returned would offer its
			// tools to exactly no turns.
			cmd := exec.Command(fields[0], fields[1:]...)
			// The server's diagnostics go to WithMCPStderr, and the
			// last of them are kept for the error below, since a server
			// that fails at start says why on stderr and the SDK reports
			// only that the connection closed. The pipe that copies them
			// is waited for a bounded time after the process exits, so
			// a grandchild holding it open cannot hang Close.
			tail = &stderrTail{w: stderr}
			cmd.Stderr, cmd.WaitDelay = tail, mcpWaitDelay
			t = &sdk.CommandTransport{Command: cmd}
		}
		opts := d.opts
		if s.elicitor != nil {
			// The client offers MCP's elicitation only when asked, and
			// the product has said who answers; the product's own
			// options follow and may still override it.
			opts = append([]mcpclient.Option{mcpclient.WithElicitation()}, opts...)
		}
		remote, err := mcpclient.Connect(ctx, t, opts...)
		if err != nil {
			err = fmt.Errorf("agentkit: connecting to MCP server %s: %w", d.label(i), err)
			if said := tail.String(); said != "" {
				err = fmt.Errorf("%w; its stderr ended: %s", err, said)
			}
			return err
		}
		k.remotes = append(k.remotes, remote)
	}
	return nil
}

// mcpWaitDelay bounds how long closing a server the kit started waits
// for its stderr to drain after the process exits.
const mcpWaitDelay = 5 * time.Second

// stderrTailBytes is how much of a server's stderr the kit keeps for
// an error from New.
const stderrTailBytes = 2048

// lockedWriter serialises writes to w.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// stderrTail is a command's stderr: it passes every write on to w, when
// there is one, and keeps the last few kilobytes. A write to w that
// fails is dropped, never returned, since an error here would stop the
// copy and leave the server blocked on a full pipe.
type stderrTail struct {
	w    io.Writer
	mu   sync.Mutex
	last []byte
}

func (t *stderrTail) Write(p []byte) (int, error) {
	if t.w != nil {
		_, _ = t.w.Write(p)
	}
	t.mu.Lock()
	t.last = append(t.last, p...)
	if over := len(t.last) - stderrTailBytes; over > 0 {
		t.last = append(t.last[:0], t.last[over:]...)
	}
	t.mu.Unlock()
	return len(p), nil
}

// String is what the server last wrote, trimmed; "" on a nil tail.
func (t *stderrTail) String() string {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.last))
}

// label names one server in an error. The position is part of it
// because nothing else is guaranteed to differ: two servers may be the
// same command with different environments, or two transports of one
// type, and a conflict between them that blamed the same string twice
// would not tell an operator which of the two to prefix.
func (d mcpDial) label(i int) string {
	what := transportLabel(d.transport)
	if fields := strings.Fields(d.command); len(fields) > 0 {
		what = fields[0]
	}
	return fmt.Sprintf("#%d %s", i+1, what)
}

// transportLabel names a transport by what it reaches, and no more:
// a command by its program, and an HTTP transport by its scheme and
// host. The label is logged, in a Conflict, in Kit.Tools and in an
// error from New, and a command's arguments or a URL's user, path and
// query are where credentials are put. Any other transport is named by
// its type.
func transportLabel(t sdk.Transport) string {
	switch t := t.(type) {
	case *sdk.CommandTransport:
		if t.Command != nil && len(t.Command.Args) > 0 {
			return t.Command.Args[0]
		}
	case *sdk.StreamableClientTransport:
		if e := endpointLabel(t.Endpoint); e != "" {
			return e
		}
	case *sdk.SSEClientTransport:
		if e := endpointLabel(t.Endpoint); e != "" {
			return e
		}
	}
	return fmt.Sprintf("%T", t)
}

func endpointLabel(endpoint string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
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

	// The items in force at the leaf, and the calls pending there, are
	// what an agent continuing this conversation must be seeded with.
	// Reading them can fail, and New is where the failures are
	// reported, so they are read here and not in the accessors.
	sctx, err := sess.Context()
	if err != nil {
		return fmt.Errorf("agentkit: reading the session's context: %w", err)
	}
	k.seed = sctx.Items
	if k.seedOps, err = session.AgentOptions(sess); err != nil {
		return fmt.Errorf("agentkit: reading the session's pending calls: %w", err)
	}
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

	// ToolElicitor: the product's, around the recorder of the run when
	// there is one, so a question and its answer are written under the
	// call.
	if s.elicitor != nil {
		by, fn := s.elicitBy, s.elicitor
		k.cfg.ToolElicitor = func(ctx context.Context, q agenttool.Elicitation) (agenttool.Answer, error) {
			if rec := k.recorderFor(ctx); rec != nil {
				return rec.Elicitor(by, fn)(ctx, q)
			}
			return fn(ctx, q)
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
				conv:   k.conversation,
				scoped: s.skillGrantScope,
			}
			if s.engine == nil {
				k.grants.observe = k.observeVerdicts(s, false)
			}
			if k.rec != nil {
				k.grants.owner, k.grants.ownerRec, k.grants.bound = k.rec.SessionID(), k.rec, true
			}
			src.own = k.grants.wrap
		}
		ts.sources = append(ts.sources, src)
	}
	if s.memStore != nil {
		tools, err := k.memoryTools(s)
		if err != nil {
			return err
		}
		ts.sources = append(ts.sources, source{name: "WithMemory", tools: tools})
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
	tools, origins, conflicts := ts.resolve(ctx)
	if len(conflicts) > 0 {
		errs := make([]error, len(conflicts))
		for i, c := range conflicts {
			errs[i] = c
		}
		return errors.Join(errs...)
	}
	k.origins = origins
	k.remember(tools)

	union := ts.provider()
	provider := func(ctx context.Context) []agenttool.Tool {
		tools := union(ctx)
		k.remember(tools)
		return tools
	}
	if k.engine != nil {
		provider = k.engine.ToolProvider(provider)
	}
	k.cfg.ToolProvider = provider
	return nil
}

// memoryTools builds agentmemory's tools over the writable scopes, with
// memory_save based on the render its run's model was shown and the
// read scopes named read-only, ahead of the product's own options.
// agentmemory.Tools panics on scopes it cannot build over, and New is
// the call documented to fail, so the panic is returned as an error.
func (k *Kit) memoryTools(s *settings) (tools []agenttool.Tool, err error) {
	writable := s.writableScopes()
	if len(writable) == 0 {
		return nil, errors.New("agentkit: WithMemory has no writable scope; agentmemory's tools need one, so leave at least one scope out of WithMemoryReadScopes")
	}
	var rest []agentmemory.ToolOption
	if read := s.readScopes(); len(read) > 0 {
		rest = append(rest, agentmemory.WithReadScopes(read...))
	}
	rest = append(rest, s.memTools...)
	defer func() {
		if r := recover(); r != nil {
			tools, err = nil, fmt.Errorf("agentkit: building the memory tools: %v", r)
			if strings.Contains(fmt.Sprint(r), "both writable and read-only") {
				err = fmt.Errorf("%w; name a scope the model may only read in WithMemoryReadScopes, not in WithMemoryTools", err)
			}
		}
	}()
	based := func(rendered func() agentmemory.Manifest) []agenttool.Tool {
		return agentmemory.Tools(s.memStore, writable,
			append([]agentmemory.ToolOption{agentmemory.WithRendered(rendered)}, rest...)...)
	}
	tools = based(k.MemoryManifest)
	for i, t := range tools {
		if t.Name() == agentmemory.SaveTool {
			tools[i] = k.saveAsRendered(t, based)
		}
	}
	return tools, nil
}

// saveAsRendered runs a memory_save call against a memory_save based on
// the render the call was composed from, so two runs off one kit each
// save over the block their model read. The render another run made in
// between may already hold a write a third session made after this
// run's render, and a save based on it discards that write with nothing
// reported. The base is, in order: the render kept for the call when it
// was decided, [Kit.keepSaveBase], which is how a save held for approval
// and run by a Resume, a run that has not rendered, keeps its base; the
// render of the call's run, [agentturn.RunIDFromContext]; and the
// manifest in force on the path of the run's session where the model
// made the call, folded from its [agentmemory.ManifestNS] records, which
// is what is left after a restart or once the bounded maps have dropped
// the run. A call in a run that finds none of them is refused, with a
// result telling the model to look again, rather than based on whichever
// run rendered last; a call outside any run is based on [Kit.MemoryManifest].
// agentmemory.WithRendered takes no context, so the tool is built per
// call over the render; building one is a few allocations and touches
// no store.
func (k *Kit) saveAsRendered(t agenttool.Tool, based func(func() agentmemory.Manifest) []agenttool.Tool) agenttool.Tool {
	return agenttool.Wrap(t, func(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
		run := agentturn.RunIDFromContext(ctx)
		k.mu.Lock()
		key := saveKey(ctx, call.ID)
		man, ok := k.saveBase.get(key)
		if ok {
			k.saveBase.delete(key)
		} else {
			man, ok = k.rendered.get(run)
		}
		k.mu.Unlock()
		if !ok {
			man, ok = k.manifestAtCall(ctx, call.ID)
		}
		if !ok {
			if run != "" {
				return agenttool.Result{}, errSaveBase
			}
			return t.Execute(ctx, call)
		}
		for _, own := range based(func() agentmemory.Manifest { return man }) {
			if own.Name() == agentmemory.SaveTool {
				return own.Execute(ctx, call)
			}
		}
		return t.Execute(ctx, call)
	})
}

// errSaveBase is what a memory_save gets when the kit no longer knows
// the block its call was composed from. Saving over another render could
// discard a write the model never saw with nothing reported, so the
// model is told to look at the entry again.
var errSaveBase = errors.New("the memory block this save was composed from is no longer known, so the save could overwrite a change you have not seen; search for the entry with " + agentmemory.SearchTool + " and save again")

// manifestAtCall is the manifest in force on the path of the run's
// session at the memory_save call callID, folded from the path's
// manifest records, and false when the run has no session the kit can
// read, the call is not on its path, or no whole manifest precedes it.
func (k *Kit) manifestAtCall(ctx context.Context, callID string) (agentmemory.Manifest, bool) {
	rec := k.recorderFor(ctx)
	if rec == nil || callID == "" {
		return agentmemory.Manifest{}, false
	}
	sess := k.sessionOf(ctx, rec, runSessionID(ctx, rec))
	if sess == nil {
		return agentmemory.Manifest{}, false
	}
	return foldManifests(sess.Path(sess.Leaf()), callID)
}

// foldManifests folds the manifest records on path, up to the function
// call callID when it is not "", and returns what is in force there:
// false when there is no record, the last one does not fold onto what
// came before, or callID is not on the path.
func foldManifests(path []agentsession.Entry, callID string) (agentmemory.Manifest, bool) {
	var (
		man   agentmemory.Manifest
		valid bool
	)
	for _, e := range path {
		switch e := e.(type) {
		case *agentsession.CustomEntry:
			if e.NS != agentmemory.ManifestNS {
				continue
			}
			next, err := agentmemory.ApplyManifestRecord(man, e.Data)
			man, valid = next, err == nil
		case *agentsession.ItemEntry:
			if callID == "" {
				continue
			}
			if fc, ok := e.Item.(*openresponses.FunctionCall); ok && fc.CallID == callID {
				return man, valid
			}
		}
	}
	return man, valid && callID == ""
}

// buildHooks fills the contested fields, in the order the package
// documents. Each is a call to the Chain function agentturn exports for
// that field, which is what a product would write instead.
func (k *Kit) buildHooks(s *settings) {
	var guards guard.Chain
	if len(s.guards) > 0 {
		guards = guard.Chain{Guards: s.guards, Observer: k.observeVerdicts(s, true)}
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

	// BeforeToolCall: the memory_save base and the skill grants' guard,
	// which decide nothing and so go first, ahead of whatever may hold or
	// allow the call; then the engine,
	// with the product's own policies folded into it when the kit built
	// it, and chained after it otherwise.
	var tool []func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error)
	if s.memStore != nil {
		tool = append(tool, k.keepSaveBase)
	}
	if k.grants != nil {
		tool = append(tool, k.grants.guard)
	}
	if k.engine != nil {
		tool = append(tool, k.engine.BeforeToolCall())
	}
	if !s.policySet {
		tool = append(tool, s.beforeToolCall...)
	}
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
		turn = append(turn, k.revokeOnUserMessage)
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
		// The agent's model name is the default and the caller's options
		// come after it, so a compact.WithModel the product passed still
		// wins. A compactor needs it as much as a local summary: its
		// request names the model, and one that names none is refused by
		// a provider that requires it. compact.WithOnFold adds a callback,
		// so the kit's goes first and a fold is recorded before a
		// compact.WithOnFold of the product's hears of it.
		opts := append([]compact.Option{compact.WithModel(s.modelName), compact.WithOnFold(k.onFold(s))}, s.compactOpts...)
		var t *compact.Transform
		if s.compactor != nil {
			t = compact.New(s.compactor, opts...)
		} else {
			model := s.compactModel
			if model == nil {
				model = s.model
			}
			t = compact.NewLocal(model, opts...)
		}
		transforms = append(transforms, t.Transform)
	}
	k.cfg.Transform = chain1(transforms, agentturn.ChainTransform)
}

// onFold is the fold callback the kit gives compaction: the Fold of the
// run's recorder, [Kit.recorderFor], then the product's
// [WithFoldObserver]; the observer hears of a fold only once it is
// recorded. It is given whether or not the kit has a session, since a
// run served under [ContextWithRecorder] has one on its context, and
// ahead of the product's options: compact.WithOnFold adds a callback,
// so a compact.WithOnFold the product passed runs after it.
func (k *Kit) onFold(s *settings) func(context.Context, compact.Fold) error {
	observe := s.foldObserver
	return func(ctx context.Context, f compact.Fold) error {
		if rec := k.recorderFor(ctx); rec != nil {
			if err := rec.Fold(ctx, f); err != nil {
				return err
			}
		}
		if observe != nil {
			observe(ctx, f)
		}
		return nil
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
		base, first := k.base, k.first
		k.mu.Unlock()

		// Without memory the parts never change, so a request whose
		// instructions are neither the configuration's nor the layers'
		// join is one a product rewrote, through Agent.SetConfig or a
		// hook of its own, and the guards see it whole, after this, as
		// they would without the kit.
		if s.memStore == nil && req.Instructions != agentsession.JoinInstructions(first) &&
			req.Instructions != agentsession.JoinInstructions(base) {
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
			group, m, om, err := memoryPart(ctx, s, k.memoryShare(s))
			if err != nil {
				return err
			}
			base, man, memOm = withGroup(base, PartMemory, group, k.order), m, om
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
			if id := agentturn.RunIDFromContext(ctx); id != "" {
				k.rendered.put(id, man)
			}
		}
		k.mu.Unlock()

		req.Instructions = agentsession.JoinInstructions(sent)
		return nil
	}
}

// withGroup returns parts with the parts of group id replaced by g, in
// the position the first of them held, or inserted where the order puts
// the group when the first render came out empty and was not joined.
// An empty g drops the group. parts is not modified.
func withGroup(parts []Part, id string, g []Part, order []string) []Part {
	out := make([]Part, 0, len(parts)+len(g))
	var replaced bool
	for _, existing := range parts {
		if group(existing.ID) != id {
			out = append(out, existing)
			continue
		}
		if !replaced {
			replaced = true
			out = append(out, g...)
		}
	}
	if !replaced && len(g) > 0 {
		out = insertByOrder(out, id, g, order)
	}
	return out
}

// guardParts runs the input guards over each part on its own, as an
// [guard.Input] carrying the part's text and no items, and returns the
// parts as the guards left them. A part a guard emptied is dropped, so
// the join of what is returned is still the instructions sent. Each
// verdict reaches the chain's observer with Subject naming the part,
// "instructions/<id>", since the chain itself does not know it, and a
// refusal is returned under the same name, so an error from New, where
// no observer runs, says which part a guard refused: one memory entry,
// the AGENTS.md chain or the product's prompt.
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
			return nil, fmt.Errorf("instructions/%s: %w", p.ID, err)
		}
		if req.Instructions == "" {
			continue
		}
		p.Text = req.Instructions
		out = append(out, p)
	}
	return out, nil
}

// record writes the memory manifest to the session the annotation lands
// in, the one the run on ctx writes or the recorder's own, whenever it
// differs from the manifest in force on that session's path, and does
// nothing when there is no session.
//
// The path is read through the recorder's store, [session.Recorder.Store],
// whose Open hands back the live session it holds, so any recorder's
// session is read the same way: the one New opened, one on the context,
// [ContextWithRecorder], a child's. What is in force is the kit's own
// last record when that is still last on the path, and otherwise the
// fold of the path's records, which is what another kit of a handoff, a
// Rebase or a restart left. A render that hashes as what is in force is
// not written; any other is written as a delta on it,
// [agentmemory.Manifest.RecordSince], so a write to one entry of a large
// memory records that entry, and whole only when nothing folds there.
// When the store cannot open the session the write is whole, and is
// skipped only while the render has not moved and the last manifest any
// kit in the process wrote to that session through that recorder is this
// kit's.
//
// The comparison, the write and the remembering are one critical
// section. Two turns that rendered different manifests must not write
// their annotations in the other order, which would leave the session's
// last one describing a render that is no longer in force; and a write
// that failed must be attempted again next turn rather than remembered
// as done.
func (k *Kit) record(ctx context.Context, man agentmemory.Manifest) error {
	rec := k.recorderFor(ctx)
	if rec == nil {
		return nil
	}
	hash := man.Hash()
	sid := runSessionID(ctx, rec)

	k.recmu.Lock()
	defer k.recmu.Unlock()
	prev, seen := k.recorded[sid]
	var (
		inForce agentmemory.Manifest
		folds   bool
	)
	sess := k.sessionOf(ctx, rec, sid)
	if sess != nil {
		path := sess.Path(sess.Leaf())
		last := lastManifest(path)
		switch {
		case last == "":
		case seen && last == prev.entry:
			inForce, folds = prev.man, true
		default:
			inForce, folds = foldManifests(path, "")
		}
		if folds && inForce.Hash() == hash {
			k.rememberManifest(sid, recordedManifest{man: man, entry: last})
			return nil
		}
	} else if seen && hash == prev.man.Hash() && lastWritten.get(rec, sid) == prev.entry {
		return nil
	}
	ns, data := man.Record()
	if folds {
		ns, data = man.RecordSince(inForce)
	}
	entry, err := rec.Annotate(ctx, ns, json.RawMessage(data))
	if err != nil {
		return fmt.Errorf("agentkit: recording the memory manifest: %w", err)
	}
	k.rememberManifest(sid, recordedManifest{man: man, entry: entry})
	lastWritten.put(rec, sid, entry)
	return nil
}

// rememberManifest keeps the manifest last recorded in session sid. The caller
// holds recmu.
func (k *Kit) rememberManifest(sid string, r recordedManifest) {
	if k.recorded == nil || len(k.recorded) >= recordedSessions {
		k.recorded = map[string]recordedManifest{}
	}
	k.recorded[sid] = r
}

// lastManifest returns the ID of the last memory manifest record on
// path, "" for none.
func lastManifest(path []agentsession.Entry) string {
	for i := len(path) - 1; i >= 0; i-- {
		if c, ok := path[i].(*agentsession.CustomEntry); ok && c.NS == agentmemory.ManifestNS {
			return c.ID
		}
	}
	return ""
}

// runSessionID is the session a run on ctx writes through rec: the
// child's, for a run under the recorder's ChildContext, as
// WithChildAgent's runs are, and the recorder's own otherwise.
func runSessionID(ctx context.Context, rec *session.Recorder) string {
	if sid := session.SessionIDFromContext(ctx); sid != "" {
		return sid
	}
	return rec.SessionID()
}

// sessionOf returns the live session sid that rec writes, or nil when
// its store will not open it. It is the one New opened when that is the
// session, and otherwise what the recorder's store holds, which is the
// session the recorder appends to: Open on a session a store holds
// hands back the session it holds, as the recorder's own reopening
// relies on.
func (k *Kit) sessionOf(ctx context.Context, rec *session.Recorder, sid string) *agentsession.Session {
	if k.sess != nil && rec == k.rec && sid == k.sess.ID() {
		return k.sess
	}
	st := rec.Store()
	if st == nil {
		return nil
	}
	sess, err := st.Open(context.WithoutCancel(ctx), sid)
	if err != nil {
		return nil
	}
	return sess
}

// lastWritten is the manifest record any kit in the process last wrote
// to a session through a recorder, for the sessions whose store will not
// open them. A kit that cannot read the path still sees that another
// kit, the other agent of a handoff, wrote since its own record, and
// writes again.
var lastWritten = &writtenBy{}

type writtenBy struct {
	mu sync.Mutex
	m  map[writtenKey]string
}

type writtenKey struct {
	rec *session.Recorder
	sid string
}

func (w *writtenBy) get(rec *session.Recorder, sid string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.m[writtenKey{rec, sid}]
}

func (w *writtenBy) put(rec *session.Recorder, sid, entry string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.m == nil || len(w.m) >= recordedSessions {
		w.m = map[writtenKey]string{}
	}
	w.m[writtenKey{rec, sid}] = entry
}

// recorderKey is the context key of [ContextWithRecorder].
type recorderKey struct{}

// ContextWithRecorder returns ctx carrying rec as the recorder of the
// conversation a run prompted with it belongs to. A front that serves
// one kit to many conversations, each recorded as a session of its own,
// prompts each run with it, and everything the kit records for that run
// goes to rec rather than to the kit's own recorder: the engine's and
// the guards' verdicts, the memory manifest, a fold, a tool's question
// and its answer, and a child agent's run. Without it they go to the
// kit's recorder, or nowhere when the kit has none.
//
// It is what the RecorderFor of agentturn/front/a2a's WithRecorderFor
// returns, beside pointing the agent's ToolRecorder at rec:
//
//	fronta2a.WithRecorderFor(func(ctx context.Context, contextID string, a *agentturn.Agent) (context.Context, func(), error) {
//		rec, err := openRecorder(ctx, store, contextID) // session.WithInstructionsParts(kit.PartsFor)
//		if err != nil {
//			return nil, nil, err
//		}
//		cfg := a.Config()
//		cfg.ToolRecorder = rec.RecordFunc()
//		if err := a.SetConfig(cfg); err != nil {
//			return nil, nil, err
//		}
//		ctx = session.ContextWithSessionID(ctx, rec.SessionID())
//		detach, id := rec.Attach(a), rec.SessionID()
//		return agentkit.ContextWithRecorder(ctx, rec), func() {
//			detach()
//			// The store holds an opened session, a lock and the whole
//			// session in memory, until it is released; the next message
//			// resumes it.
//			if r, ok := store.(interface{ Release(string) error }); ok {
//				_ = r.Release(id)
//			}
//		}, nil
//	})
//
// The engine, the guards' chain and the per-turn hooks are bound once,
// at [New], so a recorder a front sets with SetConfig cannot reach them;
// the context is the one thing every one of them is handed. An engine
// the product built, [WithEngine], has no observer of the kit's, so its
// verdicts follow the recorder on the context only if the product's own
// agentpolicy.WithObserver reads it with [RecorderFromContext]. A nil
// rec returns ctx as it is.
//
// Two things a kit serves stay the kit's and do not follow the
// conversation: a skill grant, which is a rule set on the kit's one
// engine and belongs to one conversation, [ErrSkillGrantConversation];
// and an MCP server's connection, dialed once at New, whose identity
// every conversation shares, [WithMCP]. A front that needs either per
// conversation or per user gives each its own kit.
func ContextWithRecorder(ctx context.Context, rec *session.Recorder) context.Context {
	if rec == nil {
		return ctx
	}
	return context.WithValue(ctx, recorderKey{}, rec)
}

// RecorderFromContext returns the recorder [ContextWithRecorder] put on
// ctx, or nil.
func RecorderFromContext(ctx context.Context) *session.Recorder {
	rec, _ := ctx.Value(recorderKey{}).(*session.Recorder)
	return rec
}

// conversation names the conversation a run on ctx belongs to, for the
// skill grants: the session its recorder writes, [Kit.recorderFor], or
// "" for a run recorded nowhere.
func (k *Kit) conversation(ctx context.Context) string {
	if rec := k.recorderFor(ctx); rec != nil {
		return rec.SessionID()
	}
	return ""
}

// recorderFor is the recorder a run on ctx records into: the one on the
// context, or the kit's own, or nil.
func (k *Kit) recorderFor(ctx context.Context) *session.Recorder {
	if rec := RecorderFromContext(ctx); rec != nil {
		return rec
	}
	return k.rec
}

// observeVerdicts is the observer the kit gives the engine it builds
// and the guard chain: it writes each verdict to the run's session, when
// there is one, [Kit.recorderFor], under [agentpolicy.VerdictNS], and
// hands it to the product's observer. A guard's bare allow is not written, as a hook's
// allow with no reason is not: guards run on every part and every
// message, and their silence would outweigh the run.
//
// An observer cannot fail the decision it observes, so a write that
// fails is lost, as it is for any observer; the recorder's own writes
// for the run fail the run first.
func (k *Kit) observeVerdicts(s *settings, guards bool) func(context.Context, agentpolicy.Verdict) {
	product := s.verdicts
	return func(ctx context.Context, v agentpolicy.Verdict) {
		if replaying(ctx) {
			return
		}
		if rec := k.recorderFor(ctx); rec != nil && (!guards || v.Action != agentturn.Allow || v.Reason != "") {
			ns, data := v.Record()
			_, _ = rec.Annotate(ctx, ns, json.RawMessage(data))
		}
		if product != nil {
			product(ctx, v)
		}
	}
}

// keepSaveBase is the BeforeToolCall hook the kit puts first whenever
// memory is configured: for a memory_save call decided in a run that
// rendered, it keeps that run's render as the call's base, and decides
// nothing. Whoever then holds the call, the kit's engine, one the product
// built, or a hook of the product's, the base is kept for the Resume that
// runs it. A Resume decides a call again before any render, and that
// decision keeps nothing, so the render the held call was decided under
// stands.
func (k *Kit) keepSaveBase(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
	if info.Call == nil || info.Call.Name != agentmemory.SaveTool || info.Call.CallID == "" {
		return nil, nil
	}
	run := info.RunID
	if run == "" {
		run = agentturn.RunIDFromContext(ctx)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if man, ok := k.rendered.get(run); ok {
		k.saveBase.put(saveKey(ctx, info.Call.CallID), man)
	}
	return nil, nil
}

// saveKey keys a memory_save call's base by the session on the context
// and the call's ID. A call ID names one call in one conversation only:
// two conversations a kit serves, whose provider numbers its calls, both
// have a call_0, and each run of a conversation served under a recorder
// of its own carries that session's ID.
func saveKey(ctx context.Context, callID string) string {
	return session.SessionIDFromContext(ctx) + "\x00" + callID
}

// revokeOnUserMessage is the BeforeTurn hook WithSkillGrantScope
// installs: on any turn whose new input holds a message from the user,
// in the conversation the grants belong to, it revokes every grant a
// skill's read made, so a grant lasts until the next message, however
// that message arrived. A message in another conversation the kit serves
// ends nothing, since nothing was granted there.
//
// TurnStartInfo does not say what the turn's new input is, so the test
// is the transcript's tail. A prompt puts the user's message last, or
// followed by a developer or system note sent with it; a follow-up
// appends one after the run's answer and the run goes on; a steer
// appends one between turns, after the batch's outputs. A Resume's first
// turn has the answered calls' outputs last, and every other turn the
// model's own output, so an approval and the task it continues keep the
// grant.
func (k *Kit) revokeOnUserMessage(ctx context.Context, info agentturn.TurnStartInfo) (openresponses.Items, error) {
	if newUserMessage(info.Transcript) && k.grants.owns(ctx) {
		k.RevokeSkillGrants(ctx)
	}
	return nil, nil
}

// newUserMessage reports whether the tail of tr, the items after the
// last one the model or a tool produced, holds a message from the user,
// hidden or not. What the model or a tool produces ends the tail: an
// assistant message, a function call, a reasoning item, a call's
// output. Everything else is passed over, since a product may send it
// beside the user's message or deliver it after a steer: a developer or
// system message, an item reference, a compaction, an item type this
// version does not know.
//
// An output delivered after a steer reads as a Resume's and ends the
// tail, so the steer before it does not revoke on that turn;
// TurnStartInfo does not say what arrived, which is agentturn's to add.
func newUserMessage(tr agentturn.Transcript) bool {
	for i := len(tr) - 1; i >= 0; i-- {
		item, _ := agentturn.Unhide(tr[i])
		switch it := item.(type) {
		case *openresponses.Message:
			switch it.Role {
			case openresponses.RoleUser:
				return true
			case openresponses.RoleAssistant:
				return false
			}
		case *openresponses.FunctionCall, *openresponses.FunctionCallOutput, *openresponses.ReasoningItem:
			return false
		}
	}
	return false
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
		if group(p.ID) != PartMemory {
			other += int64(len(p.Text)) + int64(len(Separator))
		}
	}
	k.mu.Unlock()
	return share(s.budget, s.budget-other)
}

// insertByOrder puts a group of parts back at the position the order
// gives id, among the parts that are there.
func insertByOrder(parts []Part, id string, g []Part, order []string) []Part {
	rank := map[string]int{}
	for i, o := range order {
		rank[o] = i
	}
	at := len(parts)
	for i, existing := range parts {
		if rank[group(existing.ID)] > rank[id] {
			at = i
			break
		}
	}
	return append(parts[:at:at], append(append([]Part(nil), g...), parts[at:]...)...)
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
// to req.Instructions: a request a hook after the kit's rewrote, one
// built from a render other than the last, or another agent's. Its
// signature is the one [session.WithInstructionsParts] takes, and a
// session the kit opens is given it, so the recorder's config entries
// carry instructions_parts and instructions_omitted. A recorder opened
// elsewhere, the one [WithRecorder] is given, takes it the same way:
//
//	var kit *agentkit.Kit
//	rec, _, err := session.Start(ctx, store, h,
//		session.WithInstructionsParts(func(ctx context.Context, req openresponses.Request) ([]agentsession.InstructionPart, []agentsession.OmittedPart) {
//			return kit.PartsFor(ctx, req)
//		}))
//	kit, err = agentkit.New(ctx, agentkit.WithRecorder(rec), ...)
//
// The recorder asks for every session it writes, a child run's among
// them, and a request whose instructions are not this kit's gets nil,
// so the child keeps the string unless [PartsFrom] names its kit too.
// Concurrent runs off one kit share the last render, so a request from
// the other run's render gets nil and is recorded as a string, which is
// what the recorder does with parts that do not join. It is safe on a
// nil kit, which composes nothing.
func (k *Kit) PartsFor(_ context.Context, req openresponses.Request) ([]agentsession.InstructionPart, []agentsession.OmittedPart) {
	if k == nil {
		return nil, nil
	}
	k.mu.Lock()
	parts, memOmitted := k.parts, k.memOmitted
	if agentsession.JoinInstructions(parts) != req.Instructions {
		// The configuration's own instructions, which the recorder
		// settles at a run's start before any hook has run, are New's
		// render as the guards left it. Never the layers' unguarded
		// render: that is text a guard kept from the model.
		if parts = k.first; agentsession.JoinInstructions(parts) != req.Instructions {
			k.mu.Unlock()
			return nil, nil
		}
		memOmitted = k.firstOmitted
	}
	out := make([]Part, len(parts))
	copy(out, parts)
	// The omissions under the same lock, so they are the same render's.
	omitted := make([]agentsession.OmittedPart, 0, len(k.omitted)+len(memOmitted))
	for _, o := range k.omitted {
		omitted = append(omitted, o.OmittedPart())
	}
	for _, o := range memOmitted {
		omitted = append(omitted, o.OmittedPart())
	}
	k.mu.Unlock()
	return out, omitted
}

// PartsFrom returns a parts function for a recorder several kits share:
// each kit is asked in turn, and the first whose [Kit.PartsFor] joins to
// the request answers. It is how one session records a handoff between
// two agents, or a parent and a child kit built under [WithRecorder],
// with each agent's instructions as its own parts.
//
// It takes the kits' variables, not the kits: the recorder is opened
// before the kits that record into it are built, so the kits do not
// exist when this is called. Each variable is read on every request,
// and one still nil is passed over.
func PartsFrom(kits ...**Kit) func(context.Context, openresponses.Request) ([]agentsession.InstructionPart, []agentsession.OmittedPart) {
	kits = append([]**Kit(nil), kits...)
	return func(ctx context.Context, req openresponses.Request) ([]agentsession.InstructionPart, []agentsession.OmittedPart) {
		for _, kp := range kits {
			if kp == nil {
				continue
			}
			if parts, omitted := (*kp).PartsFor(ctx, req); parts != nil {
				return parts, omitted
			}
		}
		return nil, nil
	}
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

// remember keeps the union the provider returned for [Kit.LookupTool].
func (k *Kit) remember(tools []agenttool.Tool) {
	byName := make(map[string]agenttool.Tool, len(tools))
	for _, t := range tools {
		byName[t.Name()] = t
	}
	k.union.Store(&byName)
}

// LookupTool returns the tool of the given name in the union the kit
// offered last, before the policy's filter: the tool the loop runs for
// a call of that name. It has the signature [agentpolicy.WithTools]
// takes, and the engine [WithPolicy] builds is given it, so the batch
// hold reads a sibling's confinement before the loop hands the engine
// that sibling's call, and [agentpolicy.Engine.Answers] finds the tool
// of a call cut off in a seeded transcript. A product that builds its
// own engine for [WithEngine] passes it the same way:
//
//	var kit *agentkit.Kit
//	engine, err := agentpolicy.Build(p, m, agentpolicy.WithTools(func(name string) (agenttool.Tool, bool) {
//		return kit.LookupTool(name)
//	}))
//	kit, err = agentkit.New(ctx, agentkit.WithEngine(engine), ...)
//
// The union is the kit's, not a run's: concurrent runs off one kit whose
// tool lists differ, through a provider that answers per context or a
// server whose list changes between them, read whichever list was
// offered last. A product whose runs see different tools of one name
// gives each run its own kit.
//
// It is safe on a nil kit, which has no tools.
func (k *Kit) LookupTool(name string) (agenttool.Tool, bool) {
	if k == nil {
		return nil, false
	}
	m := k.union.Load()
	if m == nil {
		return nil, false
	}
	t, ok := (*m)[name]
	return t, ok
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

// RegrantSkills grants again, under [WithSkillGrants], what the skill
// reads on sess's path granted and nothing revoked, as [New] does for a
// session it resumes, and binds the kit's grants to sess. It is for a
// front that resumes a conversation itself, with session.Resume under
// [ContextWithRecorder], so a call held before a restart is approved
// with the tools its task had. The grants are made silently and
// reported with [SkillGrant.Replayed] set. It refuses, with
// [ErrSkillGrantConversation], a kit whose grants already belong to
// another session, and does nothing without skill grants.
func (k *Kit) RegrantSkills(ctx context.Context, sess *agentsession.Session) error {
	if k.grants == nil || sess == nil {
		return nil
	}
	g := k.grants
	g.gmu.Lock()
	defer g.gmu.Unlock()
	g.mu.Lock()
	if !g.bound {
		g.owner, g.ownerRec, g.bound = sess.ID(), RecorderFromContext(ctx), true
	}
	ok := !g.shared && g.owner == sess.ID()
	g.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: session %q", ErrSkillGrantConversation, sess.ID())
	}
	g.regrant(ctx, sess, g.scoped)
	return nil
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
// what an agent continuing it must be seeded with. It is empty for a new
// session [WithSession] started, the prefix up to the header's Base for
// a fork WithSession started, and empty for no session at all or under
// [WithRecorder]. [Kit.AgentOptions] carries it with the calls pending
// at the leaf, which an agent resuming a session needs as well.
func (k *Kit) Transcript() agentturn.Transcript {
	return append(agentturn.Transcript(nil), k.seed...)
}

// AgentOptions are the options that seed an agent with the session at
// its leaf, [session.AgentOptions]: the transcript [Kit.Transcript]
// returns and the calls pending there, so an approval after a restart
// runs a call again only when its tool says it may. The skill grants
// the task had are the engine's, and [New] made them again, under
// [WithSkillGrants], from the session's records. It is nil for no
// session and under [WithRecorder], whose owner seeds the agent, so a
// caller need not branch:
//
//	agent := agentturn.New(kit.Config(), kit.AgentOptions()...)
func (k *Kit) AgentOptions() []agentturn.Option {
	return append([]agentturn.Option(nil), k.seedOps...)
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

// MemoryManifest is what the last render put in the memory block, of
// whichever run rendered last, and the hash a product compares to record
// the render only when it moved. The kit records it itself when a
// session is configured. memory_save is based on its own run's render,
// and on this only for a call made outside any run.
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
