package agentkit_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
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

// A memory_read call held for approval before a restart still runs when
// the resumed kit's own render fits the block: the conversation's last
// render, which the call was offered under, is read back from the
// session's record, so the resumed run is still offered memory_read.
// (#144 review F3; the front's scope and a run by hand, #147)
func TestAHeldMemoryReadRunsAfterARestart(t *testing.T) {
	// A front that scopes its conversations itself scopes every run, and
	// the conversation's render is kept under that scope. The resumed run
	// is the Control's, under the kit's recorder (RunContext), or the
	// agent's by hand, with no recorder on its context, which reads the
	// kit's own session as its renders are written there.
	for _, c := range []struct {
		scope  string
		byHand bool
	}{{"", false}, {"tenant-1", false}, {"", true}, {"tenant-1", true}} {
		scope := c.scope
		t.Run(fmt.Sprintf("scope %q by hand %v", c.scope, c.byHand), func(t *testing.T) {
			scoped := t.Context()
			if scope != "" {
				scoped = agentpolicy.ContextWithGrantScope(scoped, scope)
			}
			store := memStore(t, agentmemory.Entry{Scope: "user", Name: "tea", Content: "tea, not coffee"})
			var grown atomic.Bool
			part := func(context.Context) (agentkit.PartRender, error) {
				if grown.Load() {
					return agentkit.PartRender{Text: strings.Repeat("r", 2000)}, nil
				}
				return agentkit.PartRender{Text: "rules"}, nil
			}
			opts := func(model agentturn.Model, budget int64, extra ...agentkit.Option) []agentkit.Option {
				return append([]agentkit.Option{
					agentkit.WithModel(model, "m"),
					agentkit.WithInstructions("be brief"),
					agentkit.WithPart("rules", "product", part),
					agentkit.WithMemory(store, "user"),
					agentkit.WithInstructionBudget(budget),
				}, extra...)
			}
			whole, err := agentkit.New(t.Context(), opts(stubModel{}, 0)...)
			if err != nil {
				t.Fatal(err)
			}
			budget := int64(len(whole.Config().Instructions)) + 100
			whole.Close()

			sessions := agentsession.NewMemoryStore()
			build := func(model agentturn.Model, sess agentkit.Option) (*agentkit.Kit, *agentturn.Agent, *agentkit.Control) {
				kit, err := agentkit.New(scoped, opts(model, budget,
					agentkit.WithPolicy(agentpolicy.Policy{Ask: policyRules(t, agentmemory.ReadTool), Default: agentpolicy.Allow()}, nil),
					sess)...)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = kit.Close() })
				agent := agentturn.New(kit.Config(), kit.AgentOptions()...)
				t.Cleanup(kit.Attach(agent))
				ctl, err := kit.Control(agent)
				if err != nil {
					t.Fatal(err)
				}
				return kit, agent, ctl
			}
			answer := func(e *openresponses.Emitter) error {
				w, err := e.Message(openresponses.PhaseFinalAnswer)
				if err != nil {
					return err
				}
				if err := w.Text("noted"); err != nil {
					return err
				}
				return w.Close()
			}
			first, _, ctl := build(&scriptModel{turns: []func(*openresponses.Emitter) error{
				answer,
				callTurn(agentmemory.ReadTool, `{"scope":"user","name":"tea"}`),
			}}, agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}))
			// The rules grow: the first request drops the block, the second is
			// offered memory_read and the model calls it, held for approval.
			grown.Store(true)
			if _, err := ctl.Prompt(scoped, openresponses.UserText("hello")); err != nil {
				t.Fatal(err)
			}
			if end, err := ctl.Prompt(scoped, openresponses.UserText("what do I drink")); err != nil || end.Reason != agentturn.ReasonInputRequired {
				t.Fatalf("prompt: %v, %+v; want memory_read held", err, end)
			}

			// The rules shrink across the restart, so the resumed kit's own
			// render fits the block.
			grown.Store(false)
			model := &scriptModel{}
			resumed, agent, ctl := build(model, agentkit.WithResumedSession(sessions, first.SessionID()))
			if resumed.MemoryManifest().Dropped() {
				t.Fatal("the resumed kit's render dropped the block; the test would be vacuous")
			}
			end := &agentturn.RunEnd{Reason: agentturn.ReasonInputRequired, Pending: ctl.State().Pending}
			perms := ctl.Permissions(end)
			if len(perms) != 1 || perms[0].Call.Name != agentmemory.ReadTool {
				t.Fatalf("permissions after the restart = %+v, want memory_read", perms)
			}
			resume := ctl.Resume
			if c.byHand {
				resume = agent.Resume
			}
			if end, err := resume(scoped, agentturn.Approve(perms[0].Call.CallID).WithBy(agentpolicy.ByHuman)); err != nil || end.Reason != agentturn.ReasonDone {
				t.Fatalf("resume: %v, %+v; want done", err, end)
			}
			reqs := model.requests()
			if len(reqs) == 0 {
				t.Fatal("the resumed run sent no request")
			}
			in, _ := json.Marshal(reqs[0].Input)
			if !strings.Contains(string(in), "tea, not coffee") {
				t.Errorf("the held memory_read did not run after the restart:\n%s", in)
			}
		})
	}
}

// A memory_read call held for approval in a conversation a front
// records under ContextWithRecorder still runs after a restart when the
// resumed kit's own render fits the block: the conversation's last
// render is read back from its session's record at the first run under
// the recorder, keyed by the conversation's grant scope, the recorder's
// session or the scope the front names. (#147)
func TestAHeldMemoryReadRunsAfterARestartUnderAContextRecorder(t *testing.T) {
	for _, c := range []struct {
		name  string
		scope string
	}{{"the recorder's session", ""}, {"the front's scope", "a2a:caller-1"}} {
		t.Run(c.name, func(t *testing.T) {
			store := memStore(t, agentmemory.Entry{Scope: "user", Name: "tea", Content: "tea, not coffee"})
			var grown atomic.Bool
			part := func(context.Context) (agentkit.PartRender, error) {
				if grown.Load() {
					return agentkit.PartRender{Text: strings.Repeat("r", 2000)}, nil
				}
				return agentkit.PartRender{Text: "rules"}, nil
			}
			opts := func(model agentturn.Model, budget int64, extra ...agentkit.Option) []agentkit.Option {
				return append([]agentkit.Option{
					agentkit.WithModel(model, "m"),
					agentkit.WithInstructions("be brief"),
					agentkit.WithPart("rules", "product", part),
					agentkit.WithMemory(store, "user"),
					agentkit.WithInstructionBudget(budget),
				}, extra...)
			}
			whole, err := agentkit.New(t.Context(), opts(stubModel{}, 0)...)
			if err != nil {
				t.Fatal(err)
			}
			budget := int64(len(whole.Config().Instructions)) + 100
			whole.Close()

			build := func(model agentturn.Model) *agentkit.Kit {
				kit, err := agentkit.New(t.Context(), opts(model, budget,
					agentkit.WithPolicy(agentpolicy.Policy{Ask: policyRules(t, agentmemory.ReadTool), Default: agentpolicy.Allow()}, nil))...)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = kit.Close() })
				return kit
			}
			serve := func(kit *agentkit.Kit, rec *session.Recorder, seed ...agentturn.Option) (*agentturn.Agent, context.Context) {
				cfg := kit.Config()
				cfg.ToolRecorder = rec.RecordFunc()
				agent := agentturn.New(cfg, seed...)
				t.Cleanup(rec.Attach(agent))
				ctx := agentkit.ContextWithRecorder(session.ContextWithSessionID(t.Context(), rec.SessionID()), rec)
				if c.scope != "" {
					ctx = agentpolicy.ContextWithGrantScope(ctx, c.scope)
				}
				return agent, ctx
			}
			answer := func(e *openresponses.Emitter) error {
				w, err := e.Message(openresponses.PhaseFinalAnswer)
				if err != nil {
					return err
				}
				if err := w.Text("noted"); err != nil {
					return err
				}
				return w.Close()
			}
			sessions := agentsession.NewMemoryStore()
			first := build(&scriptModel{turns: []func(*openresponses.Emitter) error{
				answer,
				callTurn(agentmemory.ReadTool, `{"scope":"user","name":"tea"}`),
			}})
			conv, _, err := session.Start(t.Context(), sessions, agentsession.Header{CWD: t.TempDir()}, session.WithInstructionsParts(first.PartsFor))
			if err != nil {
				t.Fatal(err)
			}
			agent, ctx := serve(first, conv)
			// The rules grow: the first request drops the block, the
			// second is offered memory_read and the model calls it, held
			// for approval.
			grown.Store(true)
			if _, err := agent.Prompt(ctx, openresponses.UserText("hello")); err != nil {
				t.Fatal(err)
			}
			if end, err := agent.Prompt(ctx, openresponses.UserText("what do I drink")); err != nil || end.Reason != agentturn.ReasonInputRequired {
				t.Fatalf("prompt: %v, %+v; want memory_read held", err, end)
			}

			// The rules shrink across the restart, so the resumed kit's
			// own render fits the block.
			grown.Store(false)
			model := &scriptModel{}
			kit := build(model)
			if kit.MemoryManifest().Dropped() {
				t.Fatal("the resumed kit's render dropped the block; the test would be vacuous")
			}
			conv, sess, err := session.Resume(t.Context(), sessions, conv.SessionID(), session.WithInstructionsParts(kit.PartsFor))
			if err != nil {
				t.Fatal(err)
			}
			seed, err := session.AgentOptions(sess)
			if err != nil {
				t.Fatal(err)
			}
			agent, ctx = serve(kit, conv, seed...)
			pending := agent.State().Pending
			if len(pending) != 1 || pending[0].Call.Name != agentmemory.ReadTool {
				t.Fatalf("pending after the restart = %+v, want memory_read", pending)
			}
			if end, err := agent.Resume(ctx, agentturn.Approve(pending[0].Call.CallID).WithBy(agentpolicy.ByHuman)); err != nil || end.Reason != agentturn.ReasonDone {
				t.Fatalf("resume: %v, %+v; want done", err, end)
			}
			reqs := model.requests()
			if len(reqs) == 0 {
				t.Fatal("the resumed run sent no request")
			}
			in, _ := json.Marshal(reqs[0].Input)
			if !strings.Contains(string(in), "tea, not coffee") {
				t.Errorf("the held memory_read did not run after the restart:\n%s", in)
			}
		})
	}
}
