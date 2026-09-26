package agentkit_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agentsmd"
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
	if len(entry.InstructionsParts) != 3 {
		t.Fatalf("parts = %d, want the product, memory and agentsmd parts", len(entry.InstructionsParts))
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
	answers, err := kit.Engine().Release(t.Context(), end, agentturn.Approve(end.Pending[0].Call.CallID))
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
