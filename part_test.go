package agentkit_test

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsmd"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// A product's own part, WithPart, is rendered for each request, placed
// by the order, and recorded as a part of its own: when its text changes
// mid-session the session's delta carries the new text under its id,
// source and trust, and the rest stay parts, where a product hook that
// rewrote the instructions left the session one flattened string. Its
// omissions join Kit.Omitted. (agentsession RFC 0001 instructions_parts,
// #111)
func TestAProductPartThatChangesIsRecordedAsADeltaPart(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), "Run the tests.")
	var version atomic.Int32
	version.Store(1)
	rules := func(context.Context) (agentkit.PartRender, error) {
		r := agentkit.PartRender{Text: "Go files use gofmt.", Trust: &agentsession.Trust{Reads: []string{"untrusted"}}}
		if version.Load() == 2 {
			r.Text = "Go files use gofmt and go vet."
			r.Omitted = []agentkit.Omission{{What: "/repo/.cursor/rules/big.mdc", Reason: "too large", Size: 9000}}
		}
		return r, nil
	}
	sessions := agentsession.NewMemoryStore()
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(&scriptModel{}, "m"),
		agentkit.WithInstructions("Be brief."),
		agentkit.WithAgentsMD(root, agentsmd.Options{Root: root}),
		agentkit.WithPart("rules", "product", rules),
		agentkit.WithSession(sessions, agentsession.Header{CWD: root}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	var ids []string
	for _, p := range kit.Parts() {
		ids = append(ids, p.ID)
	}
	if want := []string{agentkit.PartProduct, agentsmd.PartID, "rules"}; !slices.Equal(ids, want) {
		t.Fatalf("parts at New = %v, want %v", ids, want)
	}
	agent := agentturn.New(kit.Config(), kit.AgentOptions()...)
	defer kit.Attach(agent)()
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("one")); err != nil {
		t.Fatal(err)
	}
	version.Store(2)
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("two")); err != nil {
		t.Fatal(err)
	}

	entries := configEntries(openSession(t, sessions, kit.SessionID()))
	if len(entries) < 2 {
		t.Fatalf("config entries = %d, want the first and the part's delta", len(entries))
	}
	for i, e := range entries {
		if len(e.InstructionsParts) == 0 {
			t.Fatalf("config entry %d records the instructions as one string", i)
		}
	}
	last := entries[len(entries)-1]
	var changed *agentsession.InstructionPart
	for i, p := range last.InstructionsParts {
		if p.Text != "" {
			if changed != nil {
				t.Fatalf("the delta carries the text of %s and %s, want the product part's alone", changed.ID, p.ID)
			}
			changed = &last.InstructionsParts[i]
		}
	}
	if changed == nil || changed.ID != "rules" || changed.Text != "Go files use gofmt and go vet." || changed.Source != "product" ||
		changed.Trust == nil || !slices.Equal(changed.Trust.Reads, []string{"untrusted"}) {
		t.Fatalf("the delta's part = %+v, want rules' new text with its source and trust", changed)
	}
	om := false
	for _, o := range kit.Omitted() {
		om = om || (o.Part == "rules" && strings.HasSuffix(o.What, "big.mdc"))
	}
	if !om {
		t.Errorf("Kit.Omitted = %+v, want the part's omission", kit.Omitted())
	}
}

// A part's id is its own: one that names a part the kit composes, or
// falls in agentmemory's reserved space, is refused at New.
func TestWithPartRefusesAnIDTheKitOwns(t *testing.T) {
	fn := func(context.Context) (agentkit.PartRender, error) { return agentkit.PartRender{Text: "x"}, nil }
	for _, id := range []string{"", agentkit.PartProduct, agentkit.PartSkills, agentsmd.PartID, agentkit.PartMemory, agentkit.PartMemoryUsage, "memory/x", "memory:x"} {
		if _, err := agentkit.New(t.Context(), agentkit.WithModel(stubModel{}, "m"), agentkit.WithPart(id, "product", fn)); err == nil {
			t.Errorf("WithPart(%q) was accepted", id)
		}
	}
}
