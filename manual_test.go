package agentkit_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentpolicy/guard"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agentsmd"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
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
			block + agentkit.Separator + agentmemory.Usage(),
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
		agentkit.WithName("dax", "a coding agent"),
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

// The manual path under guard.Redact, written as docs/manual.md now
// writes it: the guard pass over New's render, the per-turn hook, and a
// parts function that falls back to the guarded render and never to
// the layers'. A secret saved to memory reaches neither path's requests
// nor any config entry of either path's session, and the two send the
// same instructions. Without the pass at New the session's first config
// entry held the key the model never saw. (#32)
func TestTheManualPathKeepsARedactedSecretOutOfTheRecord(t *testing.T) {
	const secret = "AKIAABCDEFGHIJKLMNOP"
	store := memStore(t, agentmemory.Entry{Scope: "user", Name: "aws", Content: "the key is " + secret})
	scopes := []agentmemory.Scope{"user"}
	guards := []guard.Guard{guard.Redact()}
	sessions := agentsession.NewMemoryStore()

	// The kit's side.
	kitModel := &scriptModel{}
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(kitModel, "m"),
		agentkit.WithInstructions("Be brief."),
		agentkit.WithMemory(store, scopes...),
		agentkit.WithGuards(guards...),
		agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	kitAgent := agentturn.New(kit.Config(), kit.AgentOptions()...)
	unsubscribe := kit.Attach(kitAgent)
	if _, err := kitAgent.Prompt(t.Context(), openresponses.UserText("hi")); err != nil {
		t.Fatal(err)
	}
	unsubscribe()

	// The manual side, as docs/manual.md writes it.
	var rec *session.Recorder
	observe := func(ctx context.Context, v agentpolicy.Verdict) {
		if v.Guard == "" || v.Action != agentturn.Allow || v.Reason != "" {
			ns, data := v.Record()
			_, _ = rec.Annotate(ctx, ns, json.RawMessage(data))
		}
	}
	chain := guard.Chain{Guards: guards, Observer: observe}
	guardParts := func(ctx context.Context, c guard.Chain, parts []agentsession.InstructionPart) ([]agentsession.InstructionPart, error) {
		out := make([]agentsession.InstructionPart, 0, len(parts))
		for _, p := range parts {
			pc := c
			if obs := c.Observer; obs != nil {
				subject := "instructions/" + p.ID
				pc.Observer = func(ctx context.Context, v agentpolicy.Verdict) {
					v.Subject = subject
					obs(ctx, v)
				}
			}
			one := openresponses.Request{Instructions: p.Text}
			if err := pc.BeforeModelCall()(ctx, &one); err != nil {
				return nil, fmt.Errorf("instructions/%s: %w", p.ID, err)
			}
			if one.Instructions != "" {
				p.Text = one.Instructions
				out = append(out, p)
			}
		}
		return out, nil
	}
	layers := func(ctx context.Context) ([]agentsession.InstructionPart, error) {
		pieces, _, err := agentmemory.RenderParts(ctx, store, scopes)
		if err != nil {
			return nil, err
		}
		parts := []agentsession.InstructionPart{{ID: agentkit.PartProduct, Text: "Be brief.", Source: agentkit.SourceProduct}}
		for _, p := range pieces {
			parts = append(parts, agentsession.InstructionPart{ID: p.ID, Text: p.Text, Source: agentkit.SourceMemory})
		}
		return append(parts, agentsession.InstructionPart{ID: agentkit.PartMemoryUsage, Text: agentmemory.Usage(), Source: agentkit.SourceMemory}), nil
	}
	rendered, err := layers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	first, err := guardParts(t.Context(), guard.Chain{Guards: guards}, rendered)
	if err != nil {
		t.Fatal(err)
	}
	var (
		mu   sync.Mutex
		sent = first
	)
	partsFor := func(_ context.Context, req openresponses.Request) ([]agentsession.InstructionPart, []agentsession.OmittedPart) {
		mu.Lock()
		defer mu.Unlock()
		for _, parts := range [][]agentsession.InstructionPart{sent, first} {
			if agentsession.JoinInstructions(parts) == req.Instructions {
				return parts, nil
			}
		}
		return nil, nil
	}
	manualModel := &scriptModel{}
	cfg := agentturn.Config{
		Model:        manualModel,
		ModelName:    "m",
		Instructions: agentsession.JoinInstructions(first),
		BeforeModelCall: agentturn.ChainBeforeModelCall(
			func(ctx context.Context, req *openresponses.Request) error {
				parts, err := layers(ctx)
				if err != nil {
					return err
				}
				guarded, err := guardParts(ctx, chain, parts)
				if err != nil {
					return err
				}
				mu.Lock()
				sent = guarded
				mu.Unlock()
				req.Instructions = agentsession.JoinInstructions(guarded)
				return nil
			},
			chain.BeforeModelCall(),
		),
		OutputGuard:         chain.OutputGuard(),
		ShouldStopAfterTurn: chain.ShouldStopAfterTurn(),
	}
	rec, _, err = session.Start(t.Context(), sessions, agentsession.Header{CWD: t.TempDir()},
		session.WithInstructionsParts(partsFor))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ToolRecorder = rec.RecordFunc()
	manualAgent := agentturn.New(cfg)
	unsubscribe = rec.Attach(manualAgent)
	if _, err := manualAgent.Prompt(t.Context(), openresponses.UserText("hi")); err != nil {
		t.Fatal(err)
	}
	unsubscribe()

	if kit.Config().Instructions != cfg.Instructions {
		t.Errorf("Instructions differ:\n--- kit ---\n%s\n--- by hand ---\n%s", kit.Config().Instructions, cfg.Instructions)
	}
	kitSent, manualSent := kitModel.requests(), manualModel.requests()
	if len(kitSent) == 0 || len(kitSent) != len(manualSent) || kitSent[0].Instructions != manualSent[0].Instructions {
		t.Errorf("the two paths sent different requests")
	}
	for _, req := range append(kitSent, manualSent...) {
		if strings.Contains(req.Instructions, secret) {
			t.Fatal("a request carried the secret")
		}
	}
	// The guards' verdicts name the part they were about the same way on
	// both paths. (#39)
	redacted := func(id string) []string {
		var out []string
		for _, c := range customEntries(openSession(t, sessions, id), agentpolicy.VerdictNS) {
			var v struct{ Guard, Subject string }
			if err := json.Unmarshal(c.Data, &v); err != nil {
				t.Fatal(err)
			}
			if v.Guard != "" {
				out = append(out, v.Guard+" "+v.Subject)
			}
		}
		return out
	}
	kitVerdicts, manualVerdicts := redacted(kit.SessionID()), redacted(rec.SessionID())
	if len(kitVerdicts) == 0 || !slices.Equal(kitVerdicts, manualVerdicts) {
		t.Errorf("the guards' verdicts differ:\n kit    %q\n manual %q", kitVerdicts, manualVerdicts)
	}
	if !slices.Contains(kitVerdicts, "redact instructions/memory/user/aws") {
		t.Errorf("the kit's guard verdicts %q do not name the memory entry the secret was in", kitVerdicts)
	}

	for name, id := range map[string]string{"kit": kit.SessionID(), "manual": rec.SessionID()} {
		entries := configEntries(openSession(t, sessions, id))
		if len(entries) == 0 {
			t.Fatalf("the %s session holds no config entry", name)
		}
		for i, c := range entries {
			if len(c.InstructionsParts) == 0 && c.Instructions != nil && *c.Instructions != "" {
				t.Errorf("%s config entry %d records the instructions as one string", name, i)
			}
			b, err := json.Marshal(c)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(b), secret) {
				t.Errorf("%s config entry %d holds the secret the guard kept from the model", name, i)
			}
		}
	}
}

// Every Go block in docs/manual.md parses as Go: as declarations, or as
// the body of a function, since most blocks are statements over names
// the prose defines. A block that elides a value with `...` reads well
// and compiles nowhere, and a product copying the manual meets the
// parse error before anything else. The AddMCP block at v0.0.6 used the
// server before checking Connect's error and returned `Conflict{...}`,
// and the grant guard reported `SkillGrant{Err: ...}`. (#67)
func TestManualGoBlocksParse(t *testing.T) {
	for _, path := range []string{"docs/manual.md", "README.md"} {
		t.Run(path, func(t *testing.T) {
			blocks := goBlocks(readDoc(t, path))
			if len(blocks) == 0 {
				t.Fatalf("%s has no Go blocks; the test would be vacuous", path)
			}
			for _, b := range blocks {
				if err := parseGoBlock(b.body); err != nil {
					t.Errorf("%s:%d: the Go block does not parse: %v", path, b.line, err)
				}
			}
			t.Logf("%d Go blocks in %s", len(blocks), path)
		})
	}
}

// goBlock is one ```go fenced block of a Markdown document and the line
// its first line of code is on.
type goBlock struct {
	line int
	body string
}

// goBlocks extracts the ```go fenced blocks, indented ones among them.
func goBlocks(doc string) []goBlock {
	var out []goBlock
	var cur *goBlock
	var lines []string
	for i, l := range strings.Split(doc, "\n") {
		switch trimmed := strings.TrimSpace(l); {
		case cur == nil && trimmed == "```go":
			cur, lines = &goBlock{line: i + 2}, nil
		case cur != nil && trimmed == "```":
			cur.body = strings.Join(lines, "\n")
			out = append(out, *cur)
			cur = nil
		case cur != nil:
			lines = append(lines, l)
		}
	}
	return out
}

// parseGoBlock parses body as a Go file without its package clause,
// and failing that as the body of a function. The error it returns is
// the second's, with its line numbered within the block.
func parseGoBlock(body string) error {
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, "block.go", "package p\n"+body+"\n", parser.AllErrors); err == nil {
		return nil
	}
	const header = "package p\nfunc _() {\n"
	fset = token.NewFileSet()
	_, err := parser.ParseFile(fset, "block.go", header+body+"\n}\n", parser.AllErrors)
	if err == nil {
		return nil
	}
	var list scanner.ErrorList
	if !errors.As(err, &list) || len(list) == 0 {
		return err
	}
	first := list[0]
	return fmt.Errorf("line %d of the block: %s", first.Pos.Line-strings.Count(header, "\n"), first.Msg)
}
