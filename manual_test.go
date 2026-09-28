package agentkit_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agentsmd"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
)

// TestTheManualPathIsTheSamePath is the test of the package's one rule:
// every field Config sets a product could have set by hand, with the
// same values, by calling the same exported functions. The manual side
// here is the code docs/manual.md names, written out; if the kit ever
// grows a private seam, the two sides stop matching.
func TestTheManualPathIsTheSamePath(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), "the repository has the last word")
	writeFile(t, filepath.Join(root, "sub", "AGENTS.md"), "and the nearest file wins")
	skills := skillDir(t, filepath.Join(root, "skills"), "digging", "how to dig", "dig")
	store := memStore(t, agentmemory.Entry{
		Scope: "user", Name: "likes-tea", Content: "tea, not coffee",
	})
	policy, err := agentpolicy.Merge(agentpolicy.RuleSet{
		Source: agentpolicy.Source{Name: "test", Trusted: true},
		Deny:   []agentpolicy.Rule{{Tool: "write"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	policy.Default = agentpolicy.Allow()

	product := []agenttool.Tool{namedTool(t, "read"), namedTool(t, "write")}
	cwd := filepath.Join(root, "sub")

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "gpt-5"),
		agentkit.WithInstructions("Be brief."),
		agentkit.WithAgentsMD(cwd, agentsmd.Options{Root: root}),
		agentkit.WithSkills(skills),
		agentkit.WithMemory(store, "user"),
		agentkit.WithPolicy(policy, nil),
		agentkit.WithTools(product...),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	// The manual path, written the way docs/manual.md says.
	cat, err := agentskill.DiscoverDirs(skills)
	if err != nil {
		t.Fatal(err)
	}
	chain, err := agentsmd.Chain(cwd, agentsmd.Options{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	block, _, err := agentmemory.Render(t.Context(), store, []agentmemory.Scope{"user"})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := agentpolicy.Build(policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools := append([]agenttool.Tool(nil), product...)
	tools = append(tools, cat.Tool())
	tools = append(tools, agentmemory.Tools(store, []agentmemory.Scope{"user"})...)

	want := agentturn.Config{
		Model:     stubModel{},
		ModelName: "gpt-5",
		Instructions: strings.Join([]string{
			"Be brief.",
			cat.Prompt() + agentkit.Separator + cat.Usage(),
			block,
			agentsmd.Render(chain.Files),
		}, agentkit.Separator),
		ToolProvider: engine.ToolProvider(func(context.Context) []agenttool.Tool { return tools }),
	}

	got := kit.Config()
	// Both sides must actually hold every layer, or the comparison
	// below passes on two empty strings.
	for _, fragment := range []string{"Be brief.", "digging", "tea, not coffee", "nearest file wins"} {
		if !strings.Contains(want.Instructions, fragment) {
			t.Fatalf("the manual side does not hold %q; the comparison would be vacuous", fragment)
		}
	}
	if got.Instructions != want.Instructions {
		t.Errorf("instructions differ:\n--- kit ---\n%s\n--- by hand ---\n%s",
			got.Instructions, want.Instructions)
	}
	if got.ModelName != want.ModelName || got.Model != want.Model {
		t.Errorf("model = %v/%q, want %v/%q", got.Model, got.ModelName, want.Model, want.ModelName)
	}
	gotNames := toolNames(got.ResolveTools(t.Context()))
	wantNames := toolNames(want.ResolveTools(t.Context()))
	if strings.Join(gotNames, ",") != strings.Join(wantNames, ",") {
		t.Errorf("tools = %v, want %v", gotNames, wantNames)
	}
}

// The kit sets no field a product cannot see the source of. This walks
// what it set for the configuration above and fails on a field that is
// set without a line in docs/manual.md naming it, which keeps that
// document honest by construction rather than by review.
func TestEveryFieldTheKitSetsIsDocumented(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), "AGENTS")
	skills := skillDir(t, filepath.Join(root, "skills"), "digging", "how to dig", "dig")

	kit, err := agentkit.New(t.Context(),
		agentkit.WithName("dex", "a coding agent"),
		agentkit.WithModel(stubModel{}, "gpt-5"),
		agentkit.WithInstructions("Be brief."),
		agentkit.WithAgentsMD(root, agentsmd.Options{Root: root}),
		agentkit.WithSkills(skills),
		agentkit.WithMemory(memStore(t), "user"),
		agentkit.WithPolicy(agentpolicy.FullAuto(agentpolicy.Tools{}), nil),
		agentkit.WithTools(namedTool(t, "read")),
		agentkit.WithCompaction(60_000),
		agentkit.WithMaxTurns(20),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	doc := readDoc(t, "docs/manual.md")
	cfg := kit.Config()
	set := map[string]bool{
		"Name":                cfg.Name != "",
		"Description":         cfg.Description != "",
		"Model":               cfg.Model != nil,
		"ModelName":           cfg.ModelName != "",
		"Instructions":        cfg.Instructions != "",
		"Tools":               cfg.Tools != nil,
		"ToolProvider":        cfg.ToolProvider != nil,
		"MaxTurns":            cfg.MaxTurns != 0,
		"BeforeTurn":          cfg.BeforeTurn != nil,
		"BeforeModelCall":     cfg.BeforeModelCall != nil,
		"BeforeToolCall":      cfg.BeforeToolCall != nil,
		"AfterToolCall":       cfg.AfterToolCall != nil,
		"OutputGuard":         cfg.OutputGuard != nil,
		"ShouldStopAfterTurn": cfg.ShouldStopAfterTurn != nil,
		"Transform":           cfg.Transform != nil,
		"Filter":              cfg.Filter != nil,
		"ToolRecorder":        cfg.ToolRecorder != nil,
	}
	for field, isSet := range set {
		if !isSet {
			continue
		}
		if !strings.Contains(doc, "`"+field+"`") {
			t.Errorf("Config.%s is set and docs/manual.md does not name it", field)
		}
	}
}

// TestEveryFieldTheKitSetsIsDocumented above catches a field the kit
// sets without a line in docs/manual.md. This catches the other
// direction: a field of agentturn.Config that no line names at all,
// which is how a field the loop grew stays out of the kit's option
// surface without anyone noticing. The gap that prompted this was
// Retry: the loop had it, the kit had no option for it, and nothing
// failed.
//
// A field the kit deliberately leaves alone is still named in
// docs/manual.md, saying so and why — `Tools` is the standing example,
// which the kit never sets because it sets `ToolProvider` in its place.
// So the rule here is that every field appears, not that every field
// has an option.
func TestEveryConfigFieldIsNamedByTheManual(t *testing.T) {
	doc := readDoc(t, "docs/manual.md")
	cfg := reflect.TypeOf(agentturn.Config{})
	for i := range cfg.NumField() {
		name := cfg.Field(i).Name
		if !strings.Contains(doc, "`"+name+"`") {
			t.Errorf("agentturn.Config.%s is not named in docs/manual.md; "+
				"give it an option and a line, or a line saying why the kit leaves it alone", name)
		}
	}
}

// The fields the kit had no option for when this was written. They are
// plain pass-throughs, so the test is that they arrive: a kit is built
// with each one set to something distinguishable and the config is
// compared against the value a product would have assigned.
func TestThePassThroughFieldsArrive(t *testing.T) {
	retry := agentturn.Retry{MaxAttempts: 3}
	extra := map[string]any{"vendor_flag": true}

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithRetry(retry),
		agentkit.WithToolExecution(agentturn.ExecSequential),
		agentkit.WithMaxParallelTools(3),
		agentkit.WithRequestExtra(extra),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	cfg := kit.Config()
	if cfg.Retry.MaxAttempts != retry.MaxAttempts {
		t.Errorf("Retry.MaxAttempts = %d, want %d", cfg.Retry.MaxAttempts, retry.MaxAttempts)
	}
	if cfg.ToolExecution != agentturn.ExecSequential {
		t.Errorf("ToolExecution = %v, want ExecSequential", cfg.ToolExecution)
	}
	if cfg.MaxParallelTools != 3 {
		t.Errorf("MaxParallelTools = %d, want 3", cfg.MaxParallelTools)
	}
	if !reflect.DeepEqual(cfg.RequestExtra, extra) {
		t.Errorf("RequestExtra = %v, want %v", cfg.RequestExtra, extra)
	}
}

func readDoc(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}
