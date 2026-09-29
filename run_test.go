package agentkit_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agentsmd"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// scriptModel answers each call with the next turn of a script: a tool
// call, or a message that ends the run. It records every request it was
// sent, which is how these tests see what the assembled config actually
// put on the wire.
type scriptModel struct {
	turns []func(*openresponses.Emitter) error

	mu   sync.Mutex
	sent []openresponses.Request
	n    int
}

func (m *scriptModel) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.mu.Lock()
	m.sent = append(m.sent, req)
	turn := m.n
	m.n++
	m.mu.Unlock()

	e := openresponses.NewEmitter(sink, openresponses.NewResponse(req))
	if err := e.Start(); err != nil {
		return err
	}
	if turn < len(m.turns) {
		if err := m.turns[turn](e); err != nil {
			return err
		}
	} else {
		w, err := e.Message(openresponses.PhaseFinalAnswer)
		if err != nil {
			return err
		}
		if err := w.Text("done"); err != nil {
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
	}
	return e.Complete()
}

func (m *scriptModel) requests() []openresponses.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]openresponses.Request(nil), m.sent...)
}

func callTurn(name, args string) func(*openresponses.Emitter) error {
	return func(e *openresponses.Emitter) error {
		w, err := e.FunctionCall("call-"+name, name)
		if err != nil {
			return err
		}
		if err := w.Arguments(args); err != nil {
			return err
		}
		return w.Close()
	}
}

// The proof that the assembly is an assembly: a run driven by the
// config the kit built sends the composed instructions, offers the
// unioned tools, and records the whole thing in the session.
func TestAnAssembledConfigDrivesARun(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), "the repository says tabs")
	skills := skillDir(t, filepath.Join(root, "skills"), "digging", "how to dig", "dig with care")
	store := memStore(t, agentmemory.Entry{
		Scope: "user", Name: "likes-tea", Content: "tea, not coffee",
	})
	sessions := agentsession.NewMemoryStore()

	model := &scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn("skill", `{"name":"digging"}`),
	}}

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "test-model"),
		agentkit.WithInstructions("Be brief."),
		agentkit.WithSkills(skills),
		agentkit.WithMemory(store, "user"),
		agentkit.WithAgentsMD(root, agentsmd.Options{Root: root}),
		agentkit.WithPolicy(agentpolicy.FullAuto(agentpolicy.Tools{
			Read: []string{agentskill.ToolName},
		}), nil),
		agentkit.WithSession(sessions, agentsession.Header{CWD: root}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	agent := agentturn.New(kit.Config())
	unsubscribe := kit.Attach(agent)
	defer unsubscribe()

	end, err := agent.Prompt(t.Context(), openresponses.UserText("dig a hole"))
	if err != nil {
		t.Fatal(err)
	}
	if end.Err != nil {
		t.Fatalf("run ended with %v", end.Err)
	}
	if end.Reason != agentturn.ReasonDone {
		t.Fatalf("reason = %q, want done", end.Reason)
	}

	reqs := model.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	for _, fragment := range []string{"Be brief.", "digging", "tea, not coffee", "the repository says tabs"} {
		if !strings.Contains(reqs[0].Instructions, fragment) {
			t.Errorf("the request does not carry %q", fragment)
		}
	}
	if got := len(reqs[0].Tools); got < 4 {
		t.Errorf("tools on the request = %d, want the skill tool and the memory tools", got)
	}

	// The model asked for the skill and got its body, so the composed
	// tool set is the one the loop dispatched through.
	var served bool
	for _, item := range agentTranscript(t, sessions, kit.SessionID()) {
		if out, ok := item.(*openresponses.FunctionCallOutput); ok {
			if strings.Contains(outputText(out), "dig with care") {
				served = true
			}
		}
	}
	if !served {
		t.Error("the skill tool did not serve the skill's body into the transcript")
	}
}

// The parts the kit hands out are the parts of the request that was
// sent, which is the equality the session format checks before it will
// record them.
func TestThePartsRecordTheRequestThatWasSent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), "spaces, not tabs")
	store := memStore(t, agentmemory.Entry{Scope: "user", Name: "a", Content: "remember this"})
	model := &scriptModel{}

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "test-model"),
		agentkit.WithInstructions("Be brief."),
		agentkit.WithMemory(store, "user"),
		agentkit.WithAgentsMD(root, agentsmd.Options{Root: root}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	agent := agentturn.New(kit.Config())
	end, err := agent.Prompt(t.Context(), openresponses.UserText("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if end.Err != nil {
		t.Fatal(end.Err)
	}

	req := model.requests()[0]
	entry, err := agentsession.ConfigFromRequestParts(req, kit.Parts()...)
	if err != nil {
		t.Fatalf("the parts do not describe the request that was sent: %v", err)
	}
	if got := groupIDs(entry.InstructionsParts); strings.Join(got, ",") != "product,memory,agentsmd" {
		t.Fatalf("parts = %v, want the product, the memory group and the agentsmd part", got)
	}
}

// agentTranscript is the items recorded in the session, which is the
// only place a test can see what the loop appended.
func agentTranscript(t *testing.T, store agentsession.Store, id string) openresponses.Items {
	t.Helper()
	s, err := store.Open(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	return ctx.Items
}

func outputText(out *openresponses.FunctionCallOutput) string {
	if out.Output.Text != "" {
		return out.Output.Text
	}
	b, err := json.Marshal(out.Output.Parts)
	if err != nil {
		return ""
	}
	return string(b)
}

// The policy engine is on the run, not just on the config: a tool the
// split does not name stops the run for an answer, and the front reads
// the pending verdict off the engine the kit exposes.
func TestThePolicyEngineGatesARealRun(t *testing.T) {
	root := t.TempDir()
	skills := skillDir(t, filepath.Join(root, "skills"), "digging", "how to dig", "dig with care")

	model := &scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn("skill", `{"name":"digging"}`),
	}}
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "test-model"),
		agentkit.WithSkills(skills),
		// The split names no tool, so every call asks.
		agentkit.WithPolicy(agentpolicy.FullAuto(agentpolicy.Tools{}), nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	agent := agentturn.New(kit.Config())
	end, err := agent.Prompt(t.Context(), openresponses.UserText("dig a hole"))
	if err != nil {
		t.Fatal(err)
	}
	if end.Reason != agentturn.ReasonInputRequired {
		t.Fatalf("reason = %q, want input_required", end.Reason)
	}
	if len(end.Pending) != 1 {
		t.Fatalf("pending = %d, want the one call the policy held", len(end.Pending))
	}
	if _, ok := kit.Engine().Deferred(end.RunID, end.Pending[0].Call.CallID); !ok {
		t.Fatal("the engine holds no verdict for the held call; a front could not ask about it")
	}

	// Releasing the hold through the engine finishes the run.
	answers, err := kit.Engine().Release(t.Context(), end, agentturn.Approve(end.Pending[0].Call.CallID).WithBy(agentpolicy.ByHuman))
	if err != nil {
		t.Fatal(err)
	}
	end, err = agent.Resume(t.Context(), answers...)
	if err != nil {
		t.Fatal(err)
	}
	if end.Reason != agentturn.ReasonDone {
		t.Fatalf("reason after the release = %q, want done", end.Reason)
	}
}

// A resumed session gives back the conversation to continue, so the
// second agent picks up where the first left off.
func TestAResumedSessionSeedsTheNextAgent(t *testing.T) {
	sessions := agentsession.NewMemoryStore()
	model := &scriptModel{}

	first, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "test-model"),
		agentkit.WithInstructions("Be brief."),
		agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := first.Transcript(); len(got) != 0 {
		t.Fatalf("a new session seeded %d items, want none", len(got))
	}

	agent := agentturn.New(first.Config())
	unsubscribe := first.Attach(agent)
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("hello")); err != nil {
		t.Fatal(err)
	}
	unsubscribe()
	id := first.SessionID()
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "test-model"),
		agentkit.WithInstructions("Be brief."),
		agentkit.WithResumedSession(sessions, id),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	if second.SessionID() != id {
		t.Fatalf("session id = %q, want %q", second.SessionID(), id)
	}
	seed := second.Transcript()
	if len(seed) == 0 {
		t.Fatal("the resumed session seeded nothing; the next agent would start over")
	}
	resumed := agentturn.New(second.Config(), agentturn.WithTranscript(seed))
	if _, err := resumed.Prompt(t.Context(), openresponses.UserText("and again")); err != nil {
		t.Fatal(err)
	}
	// The second run's request carries the first run's conversation.
	last := model.requests()[len(model.requests())-1]
	if len(last.Input) <= len(seed) {
		t.Fatalf("the request carried %d items, want more than the %d it was seeded with", len(last.Input), len(seed))
	}
}

// noteRecord is what a tool writes while it runs: a Recordable Details
// value the session files beside the call.
type noteRecord struct {
	Note string `json:"note"`
}

func (noteRecord) RecordNS() string { return "test:note" }

// A record a tool writes while it runs reaches the session, because the
// kit sets Config.ToolRecorder to the recorder's RecordFunc. Without a
// session the field is left alone.
func TestAToolsRecordLandsInTheSession(t *testing.T) {
	sessions := agentsession.NewMemoryStore()
	noting := agenttool.New("note", "writes a record while it runs",
		func(ctx context.Context, _ agenttool.NoArgs) (string, error) {
			if err := agenttool.WriteRecord(ctx, noteRecord{Note: "dug"}); err != nil {
				return "", err
			}
			return "noted", nil
		})
	model := &scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn("note", `{}`),
	}}

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "test-model"),
		agentkit.WithTools(noting),
		agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	if kit.Config().ToolRecorder == nil {
		t.Fatal("a kit with a session leaves Config.ToolRecorder nil")
	}

	agent := agentturn.New(kit.Config())
	defer kit.Attach(agent)()
	end, err := agent.Prompt(t.Context(), openresponses.UserText("take a note"))
	if err != nil {
		t.Fatal(err)
	}
	if end.Err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("run ended %q with %v", end.Reason, end.Err)
	}

	s, err := sessions.Open(t.Context(), kit.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range s.Entries() {
		c, ok := e.(*agentsession.CustomEntry)
		if !ok || c.NS != "test:note" {
			continue
		}
		found = true
		if string(c.Data) != `{"note":"dug"}` {
			t.Errorf("record data = %s", c.Data)
		}
	}
	if !found {
		t.Error("the tool's record is not in the session")
	}

	plain, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "test-model"),
		agentkit.WithTools(noting),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if plain.Config().ToolRecorder != nil {
		t.Error("a kit without a session sets Config.ToolRecorder")
	}
}

// twoCalls is a turn that calls two tools in one response, one batch.
func twoCalls(first, firstArgs, second, secondArgs string) func(*openresponses.Emitter) error {
	return func(e *openresponses.Emitter) error {
		for _, c := range [][2]string{{first, firstArgs}, {second, secondArgs}} {
			w, err := e.FunctionCall("call-"+c[0], c[0])
			if err != nil {
				return err
			}
			if err := w.Arguments(c[1]); err != nil {
				return err
			}
			if err := w.Close(); err != nil {
				return err
			}
		}
		return nil
	}
}

// Under AutoEdit a confined command runs unasked, and so does a read
// beside it in one batch: the engine the kit builds is handed the
// union, so it reads the sibling's confinement before the loop hands it
// the sibling's call, where it read it as unconfined and held the read
// with nobody asked. (#18)
func TestACallBesideAConfinedCommandIsNotHeld(t *testing.T) {
	var mu sync.Mutex
	var ran []string
	note := func(name string) {
		mu.Lock()
		ran = append(ran, name)
		mu.Unlock()
	}
	read := agenttool.New("read", "read a file",
		func(context.Context, struct {
			Path string `json:"path"`
		}) (string, error) {
			note("read")
			return "module example", nil
		})
	bash := agenttool.New("bash", "run a command in the sandbox",
		func(context.Context, struct {
			Command string `json:"command"`
		}) (string, error) {
			note("bash")
			return "go.mod", nil
		},
		agenttool.WithConfined(func(context.Context, json.RawMessage) (bool, string) { return true, "sandbox" }))
	model := &scriptModel{turns: []func(*openresponses.Emitter) error{
		twoCalls("read", `{"path":"go.mod"}`, "bash", `{"command":"ls"}`),
	}}

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "test-model"),
		agentkit.WithTools(read, bash),
		agentkit.WithPolicy(agentpolicy.AutoEdit(agentpolicy.Tools{Read: []string{"read"}, Execute: []string{"bash"}}), nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	if got, ok := kit.LookupTool("bash"); !ok || got.Name() != "bash" {
		t.Fatal("LookupTool does not find a tool in the union")
	}
	end, err := agentturn.New(kit.Config()).Prompt(t.Context(), openresponses.UserText("look around"))
	if err != nil {
		t.Fatal(err)
	}
	if end.Reason != agentturn.ReasonDone || len(end.Pending) != 0 {
		t.Fatalf("reason = %q with %d pending, want done with both calls run", end.Reason, len(end.Pending))
	}
	mu.Lock()
	defer mu.Unlock()
	slices.Sort(ran)
	if strings.Join(ran, ",") != "bash,read" {
		t.Fatalf("ran %v, want both", ran)
	}
}

// A call the policy held before a restart is seeded as held, so the
// resumed agent can approve it; seeded from the transcript alone it
// would be unknown, and an approval of it would be refused as a call
// that may have run.
func TestAResumedSessionSeedsTheCallsPendingThere(t *testing.T) {
	sessions := agentsession.NewMemoryStore()
	var ran atomic.Int32
	bash := agenttool.New("bash", "run a command",
		func(context.Context, struct {
			Command string `json:"command"`
		}) (string, error) {
			ran.Add(1)
			return "ok", nil
		})
	build := func(model agentturn.Model, session agentkit.Option) *agentkit.Kit {
		kit, err := agentkit.New(t.Context(),
			agentkit.WithModel(model, "test-model"),
			agentkit.WithTools(bash),
			agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{Execute: []string{"bash"}}), nil),
			session,
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = kit.Close() })
		return kit
	}

	first := build(&scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn("bash", `{"command":"make"}`),
	}}, agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}))
	if got := first.AgentOptions(); len(got) == 0 {
		t.Fatal("a new session gave no agent options")
	}
	agent := agentturn.New(first.Config(), first.AgentOptions()...)
	unsubscribe := first.Attach(agent)
	end, err := agent.Prompt(t.Context(), openresponses.UserText("build it"))
	if err != nil {
		t.Fatal(err)
	}
	unsubscribe()
	if end.Reason != agentturn.ReasonInputRequired {
		t.Fatalf("reason = %q, want the call held", end.Reason)
	}

	second := build(&scriptModel{}, agentkit.WithResumedSession(sessions, first.SessionID()))
	resumed := agentturn.New(second.Config(), second.AgentOptions()...)
	defer second.Attach(resumed)()
	pending := resumed.State().Pending
	if len(pending) != 1 || pending[0].Reason != agentturn.PendingDeferred {
		t.Fatalf("pending = %+v, want the held call", pending)
	}
	if _, err := resumed.Resume(t.Context(),
		agentturn.Approve(pending[0].Call.CallID).WithBy(agentpolicy.ByHuman)); err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 1 {
		t.Fatalf("the approved call ran %d times, want once", ran.Load())
	}

	if got := (&agentkit.Kit{}).AgentOptions(); got != nil {
		t.Fatalf("a kit without a session gave %d agent options, want none", len(got))
	}
}
