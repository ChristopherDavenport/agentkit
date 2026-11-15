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
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

type callerKey struct{}

// byPrompt answers the first request of a run whose prompt it has a turn
// for with that turn, and every other request with "done". It keeps
// every request, so concurrent runs' requests are told apart by their
// prompt.
type byPrompt struct {
	turns map[string]func(*openresponses.Emitter) error

	mu   sync.Mutex
	sent []openresponses.Request
}

func (m *byPrompt) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	m.mu.Lock()
	m.sent = append(m.sent, req)
	m.mu.Unlock()
	e := openresponses.NewEmitter(sink, openresponses.NewResponse(req))
	if err := e.Start(); err != nil {
		return err
	}
	if turn, ok := m.turns[promptOf(req)]; ok && len(req.Input) == 1 {
		if err := turn(e); err != nil {
			return err
		}
		return e.Complete()
	}
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
	return e.Complete()
}

// requestsFor is the requests of the run prompted with prompt.
func (m *byPrompt) requestsFor(prompt string) []openresponses.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []openresponses.Request
	for _, r := range m.sent {
		if promptOf(r) == prompt {
			out = append(out, r)
		}
	}
	return out
}

func promptOf(req openresponses.Request) string {
	data, _ := json.Marshal(req.Input)
	for _, p := range []string{"from a caller", "from the person"} {
		if strings.Contains(string(data), p) {
			return p
		}
	}
	return ""
}

// One conversation's catalogue leaves a skill out that another's,
// running at the same time off the same kit, lists: the skill's name and
// description never reach the filtered run's requests, and its skill
// tool refuses the name as unknown, naming only the skills left. (#129)
func TestASkillFilterLeavesASkillOutOfOneConversation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	skillDir(t, root, "digging", "how to dig", "dig with care")
	skillDir(t, root, "secret-recipe", "PERSONS-OWN-NOTES", "the person's own")
	model := &byPrompt{turns: map[string]func(*openresponses.Emitter) error{
		"from a caller": callTurn(agentskill.ToolName, `{"name":"secret-recipe"}`),
	}}
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "m"),
		agentkit.WithSkills(root),
		agentkit.WithMemory(memStore(t, agentmemory.Entry{Scope: "user", Name: "a", Content: "remember"}), "user"),
		agentkit.WithSkillFilter(func(ctx context.Context, s agentskill.Skill) bool {
			return ctx.Value(callerKey{}) == nil || s.Name != "secret-recipe"
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	var wg sync.WaitGroup
	for _, run := range []struct {
		prompt string
		ctx    context.Context
	}{
		{"from a caller", context.WithValue(t.Context(), callerKey{}, true)},
		{"from the person", t.Context()},
	} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			agent := agentturn.New(kit.Config())
			if _, err := agent.Prompt(run.ctx, openresponses.UserText(run.prompt)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	caller := model.requestsFor("from a caller")
	if len(caller) != 2 {
		t.Fatalf("the caller's run sent %d requests, want 2", len(caller))
	}
	for i, req := range caller {
		for _, hidden := range []string{"secret-recipe", "PERSONS-OWN-NOTES"} {
			if strings.Contains(req.Instructions, hidden) {
				t.Errorf("the caller's request %d carries %q", i, hidden)
			}
		}
		if !strings.Contains(req.Instructions, "<name>\ndigging\n</name>") {
			t.Errorf("the caller's request %d does not list the skill left", i)
		}
	}
	out, _ := json.Marshal(caller[1].Input)
	if !strings.Contains(string(out), `unknown skill \"secret-recipe\"; available skills: digging`) {
		t.Errorf("the skill tool did not refuse the hidden skill as unknown:\n%s", out)
	}
	if strings.Contains(string(out), "the person's own") {
		t.Errorf("the skill tool served the hidden skill:\n%s", out)
	}

	person := model.requestsFor("from the person")
	if len(person) == 0 || !strings.Contains(person[0].Instructions, "PERSONS-OWN-NOTES") {
		t.Errorf("the person's request does not list the skill the caller's left out")
	}
}
