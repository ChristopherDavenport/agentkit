package agentkit_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

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
	usage := agentmemory.Usage(agentmemory.WithRead())
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

// memory_read is offered beside memory_search, whether the block is
// sent or dropped, and the usage paragraph names it; under an index
// scope it reads an entry the block lists by its heading alone.
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
	usage := agentmemory.Usage(agentmemory.WithRead(), agentmemory.WithIndexScopes("user"))
	parts := kit.Parts()
	if last := parts[len(parts)-1]; last.ID != agentkit.PartMemoryUsage || last.Text != usage {
		t.Errorf("usage part = %q, want agentmemory.Usage with the tools' options", last.Text)
	}
	res, err := read.Execute(t.Context(), agenttool.Call{ID: "r", Args: json.RawMessage(`{"name":"fact-01"}`)})
	if err != nil || !strings.Contains(res.Output.Text, "detail detail") {
		t.Fatalf("memory_read = %q, %v; want the entry whole", res.Output.Text, err)
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
