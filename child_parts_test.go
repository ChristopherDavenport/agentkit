package agentkit_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// partsOf is every instructions part a session's config entries carry
// with its text, in order.
func partsOf(s *agentsession.Session) []agentsession.InstructionPart {
	var out []agentsession.InstructionPart
	for _, c := range configEntries(s) {
		for _, p := range c.InstructionsParts {
			if p.Text != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

func hasLabel(t *agentsession.Trust, label string) bool {
	return t != nil && slices.Contains(t.Reads, label)
}

// A child given the conversation's memory through the kit records it as
// the memory's parts, each with its source and trust, so the child's
// path holds the untrusted label a memory entry carries, not one string
// that drops it. (agentkit#122; agentsession RFC 0001 0.21, Instructions
// as parts)
func TestChildAgentRecordsTheMemoryPartWithItsTrust(t *testing.T) {
	store := memStore(t, agentmemory.Entry{Scope: "user", Name: "likes-tea", Content: "tea, not coffee"})
	sessions := agentsession.NewMemoryStore()
	child := &scriptModel{}
	parent := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurn("explore", `{"input":"look"}`)}}

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(parent, "test-model"),
		agentkit.WithInstructions("Be the parent."),
		agentkit.WithMemory(store, "user"),
		agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}),
		agentkit.WithChild(agentturn.Config{
			Name:         "explore",
			Description:  "delegate",
			Instructions: "Explore.",
			Model:        child,
			ModelName:    "test-model",
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
	got := reqs[0].Instructions
	if !strings.HasPrefix(got, "Explore."+agentkit.Separator) || !strings.Contains(got, "tea, not coffee") {
		t.Fatalf("the child's instructions = %q, want its own prompt and then the memory block", got)
	}
	if strings.Contains(got, "Be the parent.") {
		t.Fatalf("the child's instructions carry the parent's prompt, which it did not select: %q", got)
	}

	s := openSession(t, sessions, agentsession.SubsessionID(kit.SessionID(), "call-explore"))
	parts := partsOf(s)
	if len(parts) == 0 {
		t.Fatal("the child's session records its instructions as one string, no parts")
	}
	var own, entry bool
	for _, p := range parts {
		switch {
		case p.ID == agentkit.PartProduct && p.Source == agentkit.SourceProduct && p.Text == "Explore.":
			own = true
		case p.Source == agentkit.SourceMemory && strings.Contains(p.Text, "tea, not coffee"):
			entry = true
			if !hasLabel(p.Trust, "untrusted") {
				t.Errorf("the memory entry part %q has trust %+v, want untrusted", p.ID, p.Trust)
			}
		}
	}
	if !own || !entry {
		t.Fatalf("the child's parts %+v lack its own prompt (%v) or the memory entry (%v)", parts, own, entry)
	}
	if !hasLabel(agentsession.PathTrust(s.Path(s.Leaf())), "untrusted") {
		t.Fatal("the child's path trust does not hold the memory's untrusted label")
	}
}

// ChildConfig is the manual path: the config it returns joins the
// child's prompt and the selected parts, PartsFor gives those parts for
// the text the recorder settles at the run's start and for each
// request's re-render, and a memory write between two of the child's
// requests reaches the second.
func TestChildConfigRendersEachRequestAndPartsForKnowsIt(t *testing.T) {
	store := memStore(t, agentmemory.Entry{Scope: "user", Name: "likes-tea", Content: "tea, not coffee"})
	write := agenttool.New("note", "write a note",
		func(ctx context.Context, _ agenttool.NoArgs) (string, error) {
			_, err := store.Put(ctx, agentmemory.Entry{Scope: "user", Name: "likes-cake", Content: "cake, always"})
			return "ok", err
		})
	child := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurn("note", `{}`)}}
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "test-model"),
		agentkit.WithInstructions("Be the parent."),
		agentkit.WithMemory(store, "user"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	cfg, err := kit.ChildConfig(agentturn.Config{
		Name: "explore", Instructions: "Explore.", Model: child, ModelName: "test-model",
		Tools: []agenttool.Tool{write},
	}, agentkit.PartMemory)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := kit.PartsFor(t.Context(), openresponses.Request{Instructions: cfg.Instructions})
	if len(first) == 0 || first[0].ID != agentkit.PartProduct || first[0].Text != "Explore." {
		t.Fatalf("PartsFor(the child's settled text) = %+v, want the child's prompt first", first)
	}
	for _, p := range first[1:] {
		if p.Source != agentkit.SourceMemory {
			t.Fatalf("PartsFor(the child's settled text) holds %q from %q, want the memory group alone after the prompt", p.ID, p.Source)
		}
	}

	if _, err := agentturn.New(cfg).Prompt(t.Context(), openresponses.UserText("go")); err != nil {
		t.Fatal(err)
	}
	reqs := child.requests()
	if len(reqs) != 2 {
		t.Fatalf("child requests = %d, want 2", len(reqs))
	}
	if strings.Contains(reqs[0].Instructions, "cake, always") || !strings.Contains(reqs[1].Instructions, "cake, always") {
		t.Fatalf("the memory written between the child's requests did not reach the second: %q then %q", reqs[0].Instructions, reqs[1].Instructions)
	}
	last, _ := kit.PartsFor(t.Context(), openresponses.Request{Instructions: reqs[1].Instructions})
	if agentsession.JoinInstructions(last) != reqs[1].Instructions {
		t.Fatalf("PartsFor(the child's last request) = %+v, want parts joining to what it sent", last)
	}
}

// An id the kit does not order, and the product prompt beside a child
// prompt of its own, which would put two parts under one id, are
// refused.
func TestChildConfigRefusesAPartItCannotCompose(t *testing.T) {
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithInstructions("Be the parent."),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	if _, err := kit.ChildConfig(agentturn.Config{Name: "c"}, "nonesuch"); err == nil {
		t.Error("an unknown part id was accepted")
	}
	if _, err := kit.ChildConfig(agentturn.Config{Name: "c", Instructions: "Mine."}, agentkit.PartProduct); err == nil {
		t.Error("the product part beside the child's own prompt was accepted")
	}
	cfg, err := kit.ChildConfig(agentturn.Config{Name: "c"}, agentkit.PartProduct)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Instructions != "Be the parent." {
		t.Errorf("a child with no prompt of its own given the product part has instructions %q", cfg.Instructions)
	}
}

// The compositions a WithChildAgent child made at New are pinned: past
// the bound on runtime compositions, ChildConfig evicts the runtime one
// asked for least recently, and the child New composed is still
// recognised, so its run records its instructions as parts. (agentkit
// l6-5b review M1; agentsession RFC 0001 0.21, Instructions as parts)
func TestChildConfigKeepsNewsChildPastTheRuntimeBound(t *testing.T) {
	store := memStore(t, agentmemory.Entry{Scope: "user", Name: "likes-tea", Content: "tea, not coffee"})
	sessions := agentsession.NewMemoryStore()
	child := &scriptModel{}
	parent := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurn("explore", `{"input":"look"}`)}}

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(parent, "test-model"),
		agentkit.WithInstructions("Be the parent."),
		agentkit.WithMemory(store, "user"),
		agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}),
		agentkit.WithChild(agentturn.Config{
			Name:         "explore",
			Description:  "delegate",
			Instructions: "Explore.",
			Model:        child,
			ModelName:    "test-model",
		}, agentkit.ChildParts(agentkit.PartMemory)),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	var runtime []string
	for i := range 65 {
		cfg, err := kit.ChildConfig(agentturn.Config{Name: "c", Instructions: fmt.Sprintf("Child %d.", i)}, agentkit.PartMemory)
		if err != nil {
			t.Fatal(err)
		}
		runtime = append(runtime, cfg.Instructions)
	}
	if p, _ := kit.PartsFor(t.Context(), openresponses.Request{Instructions: runtime[0]}); p != nil {
		t.Error("the runtime composition asked for least recently is still recognised past the bound")
	}
	for _, i := range []int{1, 64} {
		if p, _ := kit.PartsFor(t.Context(), openresponses.Request{Instructions: runtime[i]}); p == nil {
			t.Errorf("runtime composition %d is forgotten, want only the oldest evicted", i)
		}
	}

	agent := agentturn.New(kit.Config())
	defer kit.Attach(agent)()
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("go")); err != nil {
		t.Fatal(err)
	}
	s := openSession(t, sessions, agentsession.SubsessionID(kit.SessionID(), "call-explore"))
	var own, entry bool
	for _, p := range partsOf(s) {
		switch {
		case p.ID == agentkit.PartProduct && p.Text == "Explore.":
			own = true
		case p.Source == agentkit.SourceMemory && strings.Contains(p.Text, "tea, not coffee"):
			entry = hasLabel(p.Trust, "untrusted")
		}
	}
	if !own || !entry {
		t.Fatalf("after 65 runtime compositions the New child's session records parts %+v, want its own prompt (%v) and the untrusted memory entry (%v)", partsOf(s), own, entry)
	}
}
