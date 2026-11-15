package agentkit_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// longFacts are entries whose content is longer than the line an index
// shows of each, so an entry's part and its index entry part differ.
func longFacts(n int) []agentmemory.Entry {
	var out []agentmemory.Entry
	for i := range n {
		out = append(out, agentmemory.Entry{
			Scope:   "user",
			Name:    fmt.Sprintf("fact-%02d", i),
			Content: fmt.Sprintf("fact %d\n\n", i) + strings.Repeat("detail ", 60),
		})
	}
	return out
}

// An entry of a scope rendered as an index is in the block, as its index
// entry part: it is recorded as a part, and not also as an omitted
// element, though agentmemory's manifest lists it under Omitted with
// OmitIndex. (agentmemory RFC 0001 §8.1)
func TestAnIndexEntryIsAPartAndNotAnOmission(t *testing.T) {
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithMemory(memStore(t, longFacts(3)...), "user"),
		agentkit.WithMemoryRender(agentmemory.WithScopeIndex("user")),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	for _, o := range kit.Omitted() {
		if o.Source == agentkit.SourceMemory {
			t.Errorf("omitted %+v, want the index entry recorded as a part alone", o)
		}
	}
	ids := map[string]bool{}
	for _, p := range kit.Parts() {
		ids[p.ID] = true
	}
	for i := range 3 {
		if id := agentmemory.PartID("user", fmt.Sprintf("fact-%02d", i)); !ids[id] {
			t.Errorf("no part %s", id)
		}
	}
}

// An entry of an index scope left out over the budget is sized by the
// part it would have added, its index entry part, and not its whole
// entry's. (agentmemory RFC 0001 §8.1, EntryPartFor)
func TestAnIndexEntryOverTheBudgetIsSizedAsItsIndexPart(t *testing.T) {
	const prompt = "Be brief."
	store := memStore(t, longFacts(20)...)
	usage := agentmemory.Usage(agentmemory.WithIndexScopes("user"))
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithInstructions(prompt),
		agentkit.WithMemory(store, "user"),
		agentkit.WithMemoryRender(agentmemory.WithScopeIndex("user")),
		agentkit.WithInstructionBudget(int64(len(prompt)+2*len(agentkit.Separator)+len(usage))+600),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	n := 0
	for _, o := range kit.Omitted() {
		if o.Source != agentkit.SourceMemory {
			continue
		}
		if o.Reason != agentmemory.OmitBudget {
			t.Errorf("omitted %+v, want only entries left out over the budget", o)
			continue
		}
		n++
		e, err := store.Get(t.Context(), "user", strings.TrimPrefix(o.What, "memory/user/"))
		if err != nil {
			t.Fatal(err)
		}
		want := int64(len(agentmemory.EntryPartFor(*e, store.MaxEntryBytes(), agentmemory.WithScopeIndex("user")).Text) + len(agentsession.PartSeparator))
		if o.Size != want {
			t.Errorf("omitted %s has size %d, want %d, its index entry part's", o.What, o.Size, want)
		}
	}
	if n == 0 {
		t.Fatalf("no entry was left out over the budget: %+v", kit.Omitted())
	}
}

// Under an index scope memory_read is offered beside memory_search and
// the usage paragraph names it; it reads an entry the block lists by its
// heading alone. (agentmemory RFC 0001 §6.11)
func TestMemoryReadIsOfferedAndNamed(t *testing.T) {
	store := memStore(t, longFacts(2)...)
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithMemory(store, "user"),
		agentkit.WithMemoryRender(agentmemory.WithScopeIndex("user")),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	read, ok := kit.LookupTool(agentmemory.ReadTool)
	if !ok {
		t.Fatalf("%s is not offered", agentmemory.ReadTool)
	}
	usage := agentmemory.Usage(agentmemory.WithIndexScopes("user"))
	parts := kit.Parts()
	if last := parts[len(parts)-1]; last.ID != agentkit.PartMemoryUsage || last.Text != usage {
		t.Errorf("usage part = %q, want agentmemory.Usage with the tools' options", last.Text)
	}
	res, err := read.Execute(t.Context(), agenttool.Call{ID: "r", Args: json.RawMessage(`{"name":"fact-01"}`)})
	if err != nil || !strings.Contains(res.Output.Text, "detail detail") {
		t.Fatalf("memory_read = %q, %v; want the entry whole", res.Output.Text, err)
	}
	// A save whose render the kit no longer has is told to read the
	// entry with memory_read, which this kit offers in every request.
	save, _ := kit.LookupTool(agentmemory.SaveTool)
	ctx := agentturn.ContextWithRunID(t.Context(), "a-run-that-never-rendered")
	if _, err := save.Execute(ctx, agenttool.Call{ID: "s", Args: json.RawMessage(`{"scope":"user","name":"fact-01","content":"x"}`)}); err == nil || !strings.Contains(err.Error(), agentmemory.ReadTool) {
		t.Errorf("the save = %v, want a refusal naming %s", err, agentmemory.ReadTool)
	}
}

// A save and then a forget of one entry in one batch are the run's own:
// the forget is held to what the save left the entry as, not to the
// block's hash from before the save, because both run on the one tool
// set that keeps the session's own writes.
func TestASaveThenAForgetInOneBatchIsTheRunsOwn(t *testing.T) {
	store := memStore(t, agentmemory.Entry{Scope: "user", Name: "a", Content: "old"})
	model := &scriptModel{turns: []func(*openresponses.Emitter) error{
		func(e *openresponses.Emitter) error {
			for _, c := range []struct{ id, name, args string }{
				{"call-save", agentmemory.SaveTool, `{"scope":"user","name":"a","content":"new"}`},
				{"call-forget", agentmemory.ForgetTool, `{"scope":"user","name":"a"}`},
			} {
				w, err := e.FunctionCall(c.id, c.name)
				if err != nil {
					return err
				}
				if err := w.Arguments(c.args); err != nil {
					return err
				}
				if err := w.Close(); err != nil {
					return err
				}
			}
			return nil
		},
	}}
	sessions := agentsession.NewMemoryStore()
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "m"),
		agentkit.WithMemory(store, "user"),
		agentkit.WithSession(sessions, agentsession.Header{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	cfg := kit.Config()
	cfg.ToolExecution = agentturn.ExecSequential
	agent := agentturn.New(cfg, kit.AgentOptions()...)
	defer kit.Attach(agent)()
	ctl, err := kit.Control(agent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ctl.Prompt(t.Context(), openresponses.UserText("save then forget")); err != nil {
		t.Fatal(err)
	}
	if e, err := store.Get(t.Context(), "user", "a"); err == nil {
		t.Errorf("a = %q, want it forgotten after the run's own save", e.Content)
	}
	reqs := model.requests()
	input, _ := json.Marshal(reqs[len(reqs)-1].Input)
	if strings.Contains(string(input), "changed after the memory block was rendered") {
		t.Errorf("the forget was refused as another's change:\n%s", input)
	}
}

// A forget is held to the block its own run rendered, not to whichever
// run rendered last: an entry another session changed after this run's
// render is not forgotten, though a later run's render shows the change.
func TestAForgetIsHeldToItsOwnRunsRender(t *testing.T) {
	store := memStore(t, agentmemory.Entry{Scope: "user", Name: "a", Content: "one"})
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithMemory(store, "user"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	cfg := kit.Config()
	render := func(run string) {
		t.Helper()
		ctx := agentturn.ContextWithRunID(t.Context(), run)
		if err := cfg.BeforeModelCall(ctx, &openresponses.Request{Instructions: cfg.Instructions}); err != nil {
			t.Fatal(err)
		}
	}
	render("run-a")
	if _, err := store.Put(t.Context(), agentmemory.Entry{Scope: "user", Name: "a", Content: "two"}); err != nil {
		t.Fatal(err)
	}
	render("run-b")
	forget, ok := kit.LookupTool(agentmemory.ForgetTool)
	if !ok {
		t.Fatal("no memory_forget")
	}
	ctx := agentturn.ContextWithRunID(t.Context(), "run-a")
	_, err = forget.Execute(ctx, agenttool.Call{ID: "f", Args: json.RawMessage(`{"scope":"user","name":"a"}`)})
	if err == nil {
		t.Error("run-a's forget went through on run-b's render")
	}
	if e, err := store.Get(t.Context(), "user", "a"); err != nil || e.Content != "two" {
		t.Fatalf("a = %+v, %v; want it kept", e, err)
	}
}

// Without an index scope memory_read is offered only in a request whose
// memory block the budget dropped, as memory_search is the reader by
// words: a request that sends the block sends neither memory_read nor a
// word on it, so it costs no more than before. (agentmemory RFC 0001
// §6.11)
func TestMemoryReadIsOfferedOnlyWhenTheBlockIsDropped(t *testing.T) {
	const prompt = "be brief"
	for _, c := range []struct {
		name    string
		budget  int64
		dropped bool
	}{
		{"sent", 0, false},
		{"dropped", int64(len(prompt) + len(agentkit.Separator) + 40), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			model := &scriptModel{}
			kit, err := agentkit.New(t.Context(),
				agentkit.WithModel(model, "m"),
				agentkit.WithInstructions(prompt),
				agentkit.WithMemory(memStore(t, agentmemory.Entry{Scope: "user", Name: "a", Content: "remember " + strings.Repeat("x", 400)}), "user"),
				agentkit.WithInstructionBudget(c.budget),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer kit.Close()
			if _, err := agentturn.New(kit.Config()).Prompt(t.Context(), openresponses.UserText("hi")); err != nil {
				t.Fatal(err)
			}
			req := model.requests()[0]
			offered := false
			for _, tool := range req.Tools {
				if f, ok := tool.(*openresponses.FunctionTool); ok && f.Name == agentmemory.ReadTool {
					offered = true
				}
			}
			if offered != c.dropped {
				t.Errorf("%s offered: %v, want %v", agentmemory.ReadTool, offered, c.dropped)
			}
			if !c.dropped && strings.Contains(req.Instructions, agentmemory.ReadTool) {
				t.Errorf("the request names %s without offering it", agentmemory.ReadTool)
			}
		})
	}
}

// slowStore holds a write of the entry named slow until release is
// closed, and says on entered when one is held.
type slowStore struct {
	*agentmemory.MemStore
	entered chan struct{}
	release chan struct{}
}

func (s *slowStore) Put(ctx context.Context, e agentmemory.Entry, opts ...agentmemory.PutOption) (*agentmemory.Change, error) {
	if e.Name == "slow" {
		close(s.entered)
		<-s.release
	}
	return s.MemStore.Put(ctx, e, opts...)
}

// A memory write waits on another only for the read of its render, not
// for the other's store write: one conversation's slow store write does
// not hold up another's save.
func TestAMemoryWriteDoesNotWaitOutAnothersStoreWrite(t *testing.T) {
	store := &slowStore{MemStore: agentmemory.NewMemStore(), entered: make(chan struct{}), release: make(chan struct{})}
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithMemory(store, "user"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	cfg := kit.Config()
	for _, run := range []string{"run-a", "run-b"} {
		if err := cfg.BeforeModelCall(agentturn.ContextWithRunID(t.Context(), run), &openresponses.Request{Instructions: cfg.Instructions}); err != nil {
			t.Fatal(err)
		}
	}
	save, _ := kit.LookupTool(agentmemory.SaveTool)
	slowDone := make(chan error, 1)
	go func() {
		_, err := save.Execute(agentturn.ContextWithRunID(t.Context(), "run-a"), agenttool.Call{ID: "a", Args: json.RawMessage(`{"scope":"user","name":"slow","content":"x"}`)})
		slowDone <- err
	}()
	<-store.entered
	fastDone := make(chan error, 1)
	go func() {
		_, err := save.Execute(agentturn.ContextWithRunID(t.Context(), "run-b"), agenttool.Call{ID: "b", Args: json.RawMessage(`{"scope":"user","name":"fast","content":"y"}`)})
		fastDone <- err
	}()
	waited := false
	select {
	case err := <-fastDone:
		if err != nil {
			t.Errorf("the second save = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("the second save waited out the first's store write")
		waited = true
	}
	close(store.release)
	if err := <-slowDone; err != nil {
		t.Errorf("the first save = %v", err)
	}
	if waited {
		<-fastDone
	}
}

// A patch in a run whose render the kit no longer has runs on no render,
// held to the stored text it anchors on, and not on whichever
// conversation rendered last: a drop in that conversation's block does
// not refuse it. (agentmemory RFC 0001 §6.8)
func TestAPatchWithNoKnownRenderIsNotHeldToAnothersDrop(t *testing.T) {
	const prompt = "be brief"
	store := memStore(t, agentmemory.Entry{Scope: "user", Name: "a", Content: "old text " + strings.Repeat("x", 400)})
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithInstructions(prompt),
		agentkit.WithMemory(store, "user"),
		agentkit.WithInstructionBudget(int64(len(prompt)+len(agentkit.Separator)+40)),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	if !kit.MemoryManifest().Dropped() {
		t.Fatal("the last render did not drop the block")
	}
	patch, _ := kit.LookupTool(agentmemory.PatchTool)
	ctx := agentturn.ContextWithRunID(t.Context(), "a-run-that-never-rendered")
	if _, err := patch.Execute(ctx, agenttool.Call{ID: "p", Args: json.RawMessage(`{"scope":"user","name":"a","old_text":"old text","new_text":"new text"}`)}); err != nil {
		t.Fatalf("the patch = %v, want it run on no render", err)
	}
	if e, _ := store.Get(t.Context(), "user", "a"); !strings.HasPrefix(e.Content, "new text") {
		t.Fatalf("a = %q, want it patched", e.Content)
	}
}
