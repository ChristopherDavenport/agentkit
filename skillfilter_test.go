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
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agenttool"
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

// The skill tool's file claim is the filtered catalogue's: a call from a
// conversation whose catalogue leaves a skill out, naming that skill,
// claims the call alone, as a call naming no skill does, so no policy
// decides on, and no refusal carries, the hidden skill's file. The
// person's conversation still claims the file. (#129)
func TestASkillFilterHidesTheSkillsFileFromTheClaim(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	skillDir(t, root, "digging", "how to dig", "dig with care")
	skillDir(t, root, "secret-recipe", "PERSONS-OWN-NOTES", "the person's own")
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithSkills(root),
		agentkit.WithSkillTool(agentskill.WithFileClaim(func(_ context.Context, s *agentskill.Skill, file string) ([]agenttool.FactCall, error) {
			args, _ := json.Marshal(map[string]string{"path": filepath.Join(filepath.Dir(s.Location), file)})
			return []agenttool.FactCall{{Tool: "read", Args: args}}, nil
		})),
		agentkit.WithSkillFilter(func(ctx context.Context, s agentskill.Skill) bool {
			return ctx.Value(callerKey{}) == nil || s.Name != "secret-recipe"
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	tool, ok := kit.LookupTool(agentskill.ToolName)
	if !ok {
		t.Fatal("no skill tool")
	}
	args := json.RawMessage(`{"name":"secret-recipe"}`)
	claim := func(ctx context.Context) string {
		t.Helper()
		facts, ok, err := agenttool.FactsOf(ctx, tool, args)
		if err != nil || !ok {
			t.Fatalf("facts = %+v, %v, %v; want a claim", facts, ok, err)
		}
		out, _ := json.Marshal(facts)
		return string(out)
	}
	if got := claim(context.WithValue(t.Context(), callerKey{}, true)); strings.Contains(got, "secret-recipe/") {
		t.Errorf("the caller's claim names the hidden skill's file: %s", got)
	}
	if got := claim(t.Context()); !strings.Contains(got, "secret-recipe/") {
		t.Errorf("the person's claim does not name the skill's file: %s", got)
	}
}

// A product that rewrites the instructions it starts a conversation's
// runs with, as a server does for a caller from Kit.Parts less the
// memory block, keeps its rewrite, and the catalogue in it is still the
// filtered one: a kit with no memory block to render does not send the
// rewrite past the filter. (#129)
func TestASkillFilterReachesARewrittenInstructions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	skillDir(t, root, "digging", "how to dig", "dig with care")
	skillDir(t, root, "secret-recipe", "PERSONS-OWN-NOTES", "the person's own")
	model := &byPrompt{}
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "m"),
		agentkit.WithInstructions("Be brief."),
		agentkit.WithSkills(root),
		agentkit.WithSkillFilter(func(ctx context.Context, s agentskill.Skill) bool {
			return ctx.Value(callerKey{}) == nil || s.Name != "secret-recipe"
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	cfg := kit.Config()
	cfg.Instructions = agentsession.JoinInstructions(kit.Parts()) + agentkit.Separator + "You answer a caller."
	ctx := context.WithValue(t.Context(), callerKey{}, true)
	if _, err := agentturn.New(cfg).Prompt(ctx, openresponses.UserText("from a caller")); err != nil {
		t.Fatal(err)
	}
	reqs := model.requestsFor("from a caller")
	if len(reqs) == 0 {
		t.Fatal("no request")
	}
	got := reqs[0].Instructions
	for _, hidden := range []string{"secret-recipe", "PERSONS-OWN-NOTES"} {
		if strings.Contains(got, hidden) {
			t.Errorf("the rewritten request carries %q:\n%s", hidden, got)
		}
	}
	for _, kept := range []string{"You answer a caller.", "Be brief.", "<name>\ndigging\n</name>"} {
		if !strings.Contains(got, kept) {
			t.Errorf("the rewritten request lost %q:\n%s", kept, got)
		}
	}
}

// A filter that leaves a conversation no skill sends no catalogue and no
// usage paragraph, in the kit's own instructions and in a product's
// rewrite of them: an empty block would cost every request tokens and
// point the model at a tool with nothing to serve. (#129)
func TestASkillFilterThatLeavesNothingSendsNoCatalogue(t *testing.T) {
	root := filepath.Join(t.TempDir(), "skills")
	skillDir(t, root, "secret-recipe", "PERSONS-OWN-NOTES", "the person's own")
	model := &byPrompt{}
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "m"),
		agentkit.WithInstructions("Be brief."),
		agentkit.WithSkills(root),
		agentkit.WithSkillFilter(func(ctx context.Context, s agentskill.Skill) bool {
			return ctx.Value(callerKey{}) == nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	ctx := context.WithValue(t.Context(), callerKey{}, true)
	rewritten := kit.Config()
	rewritten.Instructions += agentkit.Separator + "You answer a caller."
	for _, c := range []struct {
		prompt string
		cfg    agentturn.Config
		want   string
	}{
		{"from a caller", kit.Config(), "Be brief."},
		{"from the person", rewritten, "Be brief." + agentkit.Separator + "You answer a caller."},
	} {
		if _, err := agentturn.New(c.cfg).Prompt(ctx, openresponses.UserText(c.prompt)); err != nil {
			t.Fatal(err)
		}
		reqs := model.requestsFor(c.prompt)
		if len(reqs) == 0 {
			t.Fatal("no request")
		}
		if got := reqs[0].Instructions; got != c.want {
			t.Errorf("%s: instructions = %q, want %q", c.prompt, got, c.want)
		}
	}
}
