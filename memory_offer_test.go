package agentkit_test

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// toolChanges is each config entry of the session after its first that
// changes the tools, as the names it adds and removes.
func toolChanges(t *testing.T, sess *agentsession.Session) [][2][]string {
	t.Helper()
	var out [][2][]string
	first := true
	for _, e := range sess.Entries() {
		c, ok := e.(*agentsession.ConfigEntry)
		if !ok {
			continue
		}
		if first {
			first = false
			continue
		}
		if c.Replace {
			t.Fatalf("a config entry replaced the configuration: %+v", c)
		}
		if len(c.ToolsAdded) == 0 && len(c.ToolsRemoved) == 0 {
			continue
		}
		var added []string
		for _, tool := range c.ToolsAdded {
			if f, ok := tool.(*openresponses.FunctionTool); ok {
				added = append(added, f.Name)
			}
		}
		out = append(out, [2][]string{added, c.ToolsRemoved})
	}
	return out
}

func offeredNames(req openresponses.Request) []string {
	var names []string
	for _, tool := range req.Tools {
		if f, ok := tool.(*openresponses.FunctionTool); ok {
			names = append(names, f.Name)
		}
	}
	return names
}

// A kit with memory and no index scope offers memory_read only once a
// render has dropped the block, so the configuration the recorder
// writes at a run's start is the first request's: a session that sends
// the block records no tools_removed for memory_read on its first
// request, as v0.0.19 did in every memory session. A block the budget
// drops and then shows again records those two changes and no other: the
// request that first drops it offers memory_search alone, and the next
// one, which shows it, offers the writes again and not memory_read.
// (#144, #138's invariant)
func TestAMemorySessionsFirstRequestRemovesNoTool(t *testing.T) {
	const prompt = "be brief"
	var big atomic.Bool
	part := func(context.Context) (agentkit.PartRender, error) {
		if big.Load() {
			return agentkit.PartRender{Text: strings.Repeat("r", 2000)}, nil
		}
		return agentkit.PartRender{Text: "rules"}, nil
	}
	opts := func(model agentturn.Model, budget int64) []agentkit.Option {
		return []agentkit.Option{
			agentkit.WithModel(model, "m"),
			agentkit.WithInstructions(prompt),
			agentkit.WithPart("rules", "product", part),
			agentkit.WithMemory(memStore(t, agentmemory.Entry{Scope: "user", Name: "tea", Content: "tea, not coffee"}), "user"),
			agentkit.WithInstructionBudget(budget),
			agentkit.WithSession(agentsession.NewMemoryStore(), agentsession.Header{}),
		}
	}
	whole, err := agentkit.New(t.Context(), opts(stubModel{}, 0)...)
	if err != nil {
		t.Fatal(err)
	}
	budget := int64(len(whole.Config().Instructions)) + 100
	whole.Close()

	model := &scriptModel{}
	kit, err := agentkit.New(t.Context(), opts(model, budget)...)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	agent := agentturn.New(kit.Config(), kit.AgentOptions()...)
	defer kit.Attach(agent)()
	prompt1 := func(text string) {
		t.Helper()
		if _, err := agent.Prompt(t.Context(), openresponses.UserText(text)); err != nil {
			t.Fatal(err)
		}
	}
	prompt1("one")
	if got := toolChanges(t, kit.Session()); len(got) != 0 {
		t.Fatalf("the first request changed the configuration's tools: %v", got)
	}

	big.Store(true)
	prompt1("two")
	big.Store(false)
	prompt1("three")
	prompt1("four")

	reqs := model.requests()
	if len(reqs) != 4 {
		t.Fatalf("requests = %d, want 4", len(reqs))
	}
	for i, want := range []struct{ read, save bool }{{false, true}, {false, false}, {false, true}, {false, true}} {
		names := offeredNames(reqs[i])
		if got := slices.Contains(names, agentmemory.ReadTool); got != want.read {
			t.Errorf("request %d offers %s: %v, want %v (%v)", i+1, agentmemory.ReadTool, got, want.read, names)
		}
		if got := slices.Contains(names, agentmemory.SaveTool); got != want.save {
			t.Errorf("request %d offers %s: %v, want %v (%v)", i+1, agentmemory.SaveTool, got, want.save, names)
		}
	}
	got := toolChanges(t, kit.Session())
	if len(got) != 2 {
		t.Fatalf("tool changes = %v, want the drop and the show", got)
	}
	if len(got[0][0]) != 0 || !slices.Contains(got[0][1], agentmemory.SaveTool) || slices.Contains(got[0][1], agentmemory.ReadTool) {
		t.Errorf("the drop recorded %v, want the writes removed and nothing else", got[0])
	}
	if len(got[1][1]) != 0 || !slices.Contains(got[1][0], agentmemory.SaveTool) {
		t.Errorf("the show recorded %v, want the writes added and nothing removed", got[1])
	}
}

// The same for a child composed with ChildParts(PartMemory) whose tools
// hold agentmemory's memory_read: its first request, which shows the
// block, records no tools_removed in the child's session. (#144)
func TestAChildsFirstRequestRemovesNoMemoryTool(t *testing.T) {
	store := memStore(t, agentmemory.Entry{Scope: "user", Name: "likes-tea", Content: "tea, not coffee"})
	sessions := agentsession.NewMemoryStore()
	child := &scriptModel{}
	parent := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurn("explore", `{"input":"look"}`)}}
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(parent, "test-model"),
		agentkit.WithInstructions("Be the parent."),
		agentkit.WithMemory(store, "user"),
		agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}),
		agentkit.WithChildAgent(agentturn.Config{
			Name:         "explore",
			Description:  "delegate",
			Instructions: "Explore.",
			Model:        child,
			ModelName:    "test-model",
			Tools:        agentmemory.Tools(store, []agentmemory.Scope{"user"}, agentmemory.WithRead()),
		}, agentkit.ChildParts(agentkit.PartMemory)),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	agent := agentturn.New(kit.Config())
	defer kit.Attach(agent)()
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("go")); err != nil {
		t.Fatal(err)
	}
	reqs := child.requests()
	if len(reqs) == 0 {
		t.Fatal("the child sent no request")
	}
	if names := offeredNames(reqs[0]); slices.Contains(names, agentmemory.ReadTool) || !slices.Contains(names, agentmemory.SaveTool) {
		t.Fatalf("the child's first request offers %v, want the writes and not %s", names, agentmemory.ReadTool)
	}
	s := openSession(t, sessions, agentsession.SubsessionID(kit.SessionID(), "call-explore"))
	if got := toolChanges(t, s); len(got) != 0 {
		t.Fatalf("the child's first request changed its configuration's tools: %v", got)
	}
}

// Whether the last render dropped the block is each conversation's own:
// an A2A caller's render on the person's kit, which fits, does not take
// memory_read from the person's next request, whose block is still
// dropped, and the person's record does not flip memory_read per
// interleaved turn. (#144 review F2)
func TestMemoryReadFollowsEachConversationsOwnRender(t *testing.T) {
	var grown atomic.Bool
	part := func(ctx context.Context) (agentkit.PartRender, error) {
		if ctx.Value(callerKey{}) != nil || !grown.Load() {
			return agentkit.PartRender{Text: "rules"}, nil
		}
		return agentkit.PartRender{Text: strings.Repeat("r", 2000)}, nil
	}
	opts := func(model agentturn.Model, budget int64) []agentkit.Option {
		return []agentkit.Option{
			agentkit.WithModel(model, "m"),
			agentkit.WithInstructions("be brief"),
			agentkit.WithPart("rules", "product", part),
			agentkit.WithMemory(memStore(t, agentmemory.Entry{Scope: "user", Name: "tea", Content: "tea, not coffee"}), "user"),
			agentkit.WithInstructionBudget(budget),
			agentkit.WithSession(agentsession.NewMemoryStore(), agentsession.Header{}),
		}
	}
	whole, err := agentkit.New(t.Context(), opts(stubModel{}, 0)...)
	if err != nil {
		t.Fatal(err)
	}
	budget := int64(len(whole.Config().Instructions)) + 100
	whole.Close()
	model := &scriptModel{}
	kit, err := agentkit.New(t.Context(), opts(model, budget)...)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	// The person's rules grow past the budget after New: the person's
	// block is dropped from the first request on, a caller's still fits.
	grown.Store(true)
	person := agentturn.New(kit.Config(), kit.AgentOptions()...)
	defer kit.Attach(person)()
	caller := agentturn.New(kit.Config())
	callerCtx := agentpolicy.ContextWithGrantScope(context.WithValue(t.Context(), callerKey{}, true), "a2a:caller-1")

	for _, step := range []struct {
		prompt string
		agent  *agentturn.Agent
		ctx    context.Context
	}{
		{"person one", person, t.Context()},
		{"caller one", caller, callerCtx},
		{"person two", person, t.Context()},
		{"caller two", caller, callerCtx},
		{"person three", person, t.Context()},
	} {
		if _, err := step.agent.Prompt(step.ctx, openresponses.UserText(step.prompt)); err != nil {
			t.Fatal(err)
		}
	}
	// The request that first drops the block offers memory_search
	// alone; every later one of the person's offers memory_read.
	reqs := model.requests()
	if len(reqs) != 5 {
		t.Fatalf("requests = %d, want 5", len(reqs))
	}
	for _, c := range []struct {
		prompt string
		i      int
		read   bool
	}{{"person one", 0, false}, {"person two", 2, true}, {"person three", 4, true}} {
		names := offeredNames(reqs[c.i])
		if slices.Contains(names, agentmemory.ReadTool) != c.read || slices.Contains(names, agentmemory.SaveTool) {
			t.Errorf("%s offers %v, want %s %v and no writes: the person's block is dropped", c.prompt, names, agentmemory.ReadTool, c.read)
		}
	}
	if slices.Contains(offeredNames(reqs[3]), agentmemory.ReadTool) {
		t.Errorf("the caller, whose block fits, was offered %s", agentmemory.ReadTool)
	}
	// The drop, and memory_read on the next request: nothing flips
	// with the caller's turns.
	if got := toolChanges(t, kit.Session()); len(got) != 2 {
		t.Errorf("the person's record changed the tools %d times, want the drop and memory_read once: %v", len(got), got)
	}
}
