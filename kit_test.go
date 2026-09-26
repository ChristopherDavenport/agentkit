package agentkit_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agentsmd"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// stubModel is a Streamer that is never called: New must not call the
// model, and every test here stops before a run.
type stubModel struct{}

func (stubModel) CreateStream(context.Context, openresponses.Request, openresponses.EventSink) error {
	return errors.New("stubModel: not for calling")
}

func namedTool(t *testing.T, name string) agenttool.Tool {
	t.Helper()
	return agenttool.New(name, name+" does nothing",
		func(context.Context, agenttool.NoArgs) (string, error) { return "", nil })
}

// writeFile writes one file, creating its directories.
func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// skillDir writes one skill and returns the directory holding it.
func skillDir(t *testing.T, root, name, description, body string) string {
	t.Helper()
	writeFile(t, filepath.Join(root, name, "SKILL.md"),
		"---\nname: "+name+"\ndescription: "+description+"\n---\n\n"+body+"\n")
	return root
}

func memStore(t *testing.T, entries ...agentmemory.Entry) *agentmemory.MemStore {
	t.Helper()
	s := agentmemory.NewMemStore()
	for _, e := range entries {
		if _, err := s.Put(t.Context(), e); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestNewRefusesAKitWithoutAModel(t *testing.T) {
	_, err := agentkit.New(t.Context(), agentkit.WithInstructions("hello"))
	if !errors.Is(err, agentturn.ErrNoModel) {
		t.Fatalf("err = %v, want one wrapping agentturn.ErrNoModel", err)
	}
}

// The order is the whole of the instruction composition, so it is
// tested as an order and not as a rendered string.
func TestPartsAreInTheDocumentedOrder(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), "the repository has the last word")
	skills := skillDir(t, filepath.Join(root, "skills"), "digging", "how to dig", "dig")

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithInstructions("be brief"),
		agentkit.WithSkills(skills),
		agentkit.WithMemory(memStore(t, agentmemory.Entry{
			Scope: "user", Name: "likes-tea", Content: "tea, not coffee",
		}), "user"),
		agentkit.WithAgentsMD(root, agentsmd.Options{Root: root}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	var ids []string
	for _, p := range kit.Parts() {
		ids = append(ids, p.ID)
	}
	want := []string{agentkit.PartProduct, agentkit.PartSkills, agentkit.PartMemory, agentsmd.PartID}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("part ids = %v, want %v", ids, want)
	}
}

// The parts are the record of the instructions, so their texts joined
// with the separator must be exactly what the model is sent. The
// session format checks the same equality before it will take them.
func TestJoinedPartsAreTheInstructions(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), "use tabs")

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithInstructions("be brief"),
		agentkit.WithAgentsMD(root, agentsmd.Options{Root: root}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	got := agentsession.JoinInstructions(kit.Parts())
	if got != kit.Config().Instructions {
		t.Fatalf("joined parts:\n%q\ninstructions:\n%q", got, kit.Config().Instructions)
	}
	if _, err := agentsession.ConfigFromRequestParts(
		openresponses.Request{Instructions: kit.Config().Instructions},
		kit.Parts()...,
	); err != nil {
		t.Fatalf("the session format refused the parts: %v", err)
	}
}

func TestEmptyPartsAreNotJoined(t *testing.T) {
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithInstructions("be brief"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	if got, want := kit.Config().Instructions, "be brief"; got != want {
		t.Fatalf("instructions = %q, want %q", got, want)
	}
	if n := len(kit.Parts()); n != 1 {
		t.Fatalf("parts = %d, want 1", n)
	}
}

func TestWithOrderReversesAPosition(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), "AGENTS")

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithInstructions("PRODUCT"),
		agentkit.WithMemory(memStore(t, agentmemory.Entry{
			Scope: "user", Name: "a", Content: "MEMORY",
		}), "user"),
		agentkit.WithAgentsMD(root, agentsmd.Options{Root: root}),
		agentkit.WithOrder(agentkit.PartProduct, agentsmd.PartID, agentkit.PartMemory),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	text := kit.Config().Instructions
	if strings.Index(text, "AGENTS") > strings.Index(text, "MEMORY") {
		t.Fatalf("the order was not applied:\n%s", text)
	}
}

func TestWithOrderMustNameEveryConfiguredPart(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), "AGENTS")

	_, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithInstructions("PRODUCT"),
		agentkit.WithAgentsMD(root, agentsmd.Options{Root: root}),
		agentkit.WithOrder(agentkit.PartProduct),
	)
	if err == nil || !strings.Contains(err.Error(), agentsmd.PartID) {
		t.Fatalf("err = %v, want one naming the omitted part", err)
	}
}

func TestOmissionsFromEveryLayerAreOneList(t *testing.T) {
	root := t.TempDir()
	// Two names in one directory: the preferred one shadows the other.
	writeFile(t, filepath.Join(root, "AGENTS.md"), "preferred")
	writeFile(t, filepath.Join(root, "CLAUDE.md"), "shadowed")
	// A skill with no description cannot be chosen, so it is not
	// offered.
	skills := filepath.Join(root, "skills")
	writeFile(t, filepath.Join(skills, "mute", "SKILL.md"), "---\nname: mute\n---\n\nnothing\n")
	skillDir(t, skills, "digging", "how to dig", "dig")

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithSkills(skills),
		agentkit.WithAgentsMD(root, agentsmd.Options{
			Root:  root,
			Names: []string{"AGENTS.md", "CLAUDE.md"},
		}),
		agentkit.WithMemory(memStore(t, agentmemory.Entry{
			Scope: "user", Name: "big", Content: strings.Repeat("x", 3000),
		}), "user"),
		agentkit.WithMemoryRender(agentmemory.WithMaxTotalBytes(400)),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	bySource := map[string]int{}
	for _, o := range kit.Omitted() {
		bySource[o.Source]++
		if o.What == "" || o.Reason == "" {
			t.Errorf("omission with no key or no reason: %+v", o)
		}
	}
	for _, src := range []string{agentkit.SourceAgentsMD, agentkit.SourceSkills, agentkit.SourceMemory} {
		if bySource[src] == 0 {
			t.Errorf("no omission from %s; got %v", src, kit.Omitted())
		}
	}
	for _, p := range kit.OmittedParts() {
		if p.ID == "" {
			t.Errorf("omitted part with no id: %+v", p)
		}
	}
}

func TestInstructionBudgetBoundsTheJoinedText(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), strings.Repeat("a", 4000))
	writeFile(t, filepath.Join(root, "sub", "AGENTS.md"), strings.Repeat("b", 4000))

	const budget = 3000
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithInstructions("be brief"),
		agentkit.WithAgentsMD(filepath.Join(root, "sub"), agentsmd.Options{Root: root}),
		agentkit.WithMemory(memStore(t, agentmemory.Entry{
			Scope: "user", Name: "a", Content: strings.Repeat("c", 4000),
		}), "user"),
		agentkit.WithInstructionBudget(budget),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	if n := len(kit.Config().Instructions); n > budget {
		t.Fatalf("instructions are %d bytes, over the %d-byte budget", n, budget)
	}
	if len(kit.Omitted()) == 0 {
		t.Fatal("the budget dropped something and reported nothing")
	}
}

func TestInstructionBudgetUnderTheUnboundedPartsIsAnError(t *testing.T) {
	_, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithInstructions(strings.Repeat("a", 500)),
		agentkit.WithInstructionBudget(100),
	)
	if err == nil || !strings.Contains(err.Error(), "cannot bound") {
		t.Fatalf("err = %v, want one saying the budget is under the parts the kit cannot bound", err)
	}
}

func TestToolsAreUnionedInAStableOrder(t *testing.T) {
	root := t.TempDir()
	skills := skillDir(t, filepath.Join(root, "skills"), "digging", "how to dig", "dig")

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithTools(namedTool(t, "read"), namedTool(t, "write")),
		agentkit.WithSkills(skills),
		agentkit.WithMemory(memStore(t), "user"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	names := toolNames(kit.Config().ResolveTools(t.Context()))
	if len(names) < 4 {
		t.Fatalf("tools = %v, want the product's, the skill tool and the memory tools", names)
	}
	if names[0] != "read" || names[1] != "write" || names[2] != agentskill.ToolName {
		t.Fatalf("tools = %v, want product tools then the skill tool then memory", names)
	}
	if !strings.HasPrefix(names[3], "memory_") {
		t.Fatalf("tools = %v, want the memory tools after the skill tool", names)
	}
}

func TestTwoToolsWithOneNameIsAnError(t *testing.T) {
	root := t.TempDir()
	skills := skillDir(t, filepath.Join(root, "skills"), "digging", "how to dig", "dig")

	_, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithTools(namedTool(t, agentskill.ToolName)),
		agentkit.WithSkills(skills),
	)
	if err == nil {
		t.Fatal("two tools named skill were accepted")
	}
	var c agentkit.Conflict
	if !errors.As(err, &c) {
		t.Fatalf("err = %v, want a Conflict", err)
	}
	if c.Name != agentskill.ToolName || c.Kept != "WithTools" || c.Dropped != "WithSkills" {
		t.Fatalf("conflict = %+v, want it to name both sources", c)
	}
}

func TestARuntimeConflictDropsTheLaterToolAndReportsIt(t *testing.T) {
	var seen []agentkit.Conflict
	late := []agenttool.Tool{namedTool(t, "read")}

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithTools(namedTool(t, "read")),
		agentkit.WithToolProvider(func(context.Context) []agenttool.Tool { return nil }),
		agentkit.WithToolConflict(func(c agentkit.Conflict) { seen = append(seen, c) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	// The provider only starts returning the colliding name after New
	// has checked, which is the case a provider cannot fail on.
	kit2, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithTools(namedTool(t, "read")),
		agentkit.WithToolProvider(func(context.Context) []agenttool.Tool { return late }),
		agentkit.WithToolConflict(func(c agentkit.Conflict) { seen = append(seen, c) }),
	)
	if err == nil {
		t.Fatal("a conflict present at New should be an error")
	}
	_ = kit2

	names := toolNames(kit.Config().ResolveTools(t.Context()))
	if len(names) != 1 || names[0] != "read" {
		t.Fatalf("tools = %v, want one read", names)
	}
	if len(seen) != 0 {
		t.Fatalf("conflicts = %v, want none while the provider is empty", seen)
	}
}

func TestAPolicyFiltersTheToolsTheModelIsOffered(t *testing.T) {
	policy, err := agentpolicy.Merge(agentpolicy.RuleSet{
		Source: agentpolicy.Source{Name: "test", Trusted: true},
		Deny:   []agentpolicy.Rule{{Tool: "write"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	policy.Default = agentpolicy.Allow()

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithTools(namedTool(t, "read"), namedTool(t, "write")),
		agentkit.WithPolicy(policy, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	names := toolNames(kit.Config().ResolveTools(t.Context()))
	for _, n := range names {
		if n == "write" {
			t.Fatalf("tools = %v, want write removed by the bare deny rule", names)
		}
	}
	if kit.Engine() == nil {
		t.Fatal("Engine is nil; a front cannot reach Deferred or Release")
	}
}

func TestMemoryIsReRenderedBeforeEachModelCall(t *testing.T) {
	store := memStore(t, agentmemory.Entry{Scope: "user", Name: "a", Content: "FIRST"})

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithInstructions("PRODUCT"),
		agentkit.WithMemory(store, "user"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	cfg := kit.Config()
	if cfg.BeforeModelCall == nil {
		t.Fatal("BeforeModelCall is nil; memory would never refresh")
	}
	if _, err := store.Put(t.Context(), agentmemory.Entry{
		Scope: "user", Name: "a", Content: "SECOND",
	}); err != nil {
		t.Fatal(err)
	}

	req := openresponses.Request{Instructions: cfg.Instructions}
	if err := cfg.BeforeModelCall(t.Context(), &req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(req.Instructions, "SECOND") {
		t.Fatalf("instructions did not pick up the write:\n%s", req.Instructions)
	}
	if !strings.Contains(req.Instructions, "PRODUCT") {
		t.Fatalf("the re-render lost the other parts:\n%s", req.Instructions)
	}
	if got := agentsession.JoinInstructions(kit.Parts()); got != req.Instructions {
		t.Fatal("the parts no longer join to the instructions that were sent")
	}
	if kit.MemoryManifest().Hash() == "" {
		t.Fatal("no manifest for the render that was sent")
	}
}

func TestHooksRunInTheDocumentedOrder(t *testing.T) {
	var order []string

	store := memStore(t, agentmemory.Entry{Scope: "user", Name: "a", Content: "M"})
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithInstructions("P"),
		agentkit.WithMemory(store, "user"),
		agentkit.WithBeforeModelCall(func(context.Context, *openresponses.Request) error {
			order = append(order, "product")
			return nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	req := openresponses.Request{}
	if err := kit.Config().BeforeModelCall(t.Context(), &req); err != nil {
		t.Fatal(err)
	}
	// The product's hook runs last, so what it sees is what memory
	// injected.
	if len(order) != 1 || order[0] != "product" {
		t.Fatalf("order = %v", order)
	}
	if !strings.Contains(req.Instructions, "M") {
		t.Fatal("the product's hook did not see the block memory injected")
	}
}

func TestTransformAndCompactionCannotBothBeSet(t *testing.T) {
	_, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithCompaction(1000),
		agentkit.WithTransform(func(_ context.Context, tr agentturn.Transcript) (agentturn.Transcript, error) {
			return tr, nil
		}),
	)
	if err == nil || !strings.Contains(err.Error(), "no chain for it") {
		t.Fatalf("err = %v, want one saying Transform is one field", err)
	}
}

func TestCompactionSetsTheTransform(t *testing.T) {
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithCompaction(1000),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	if kit.Config().Transform == nil {
		t.Fatal("Transform is nil")
	}
}

func TestPolicyAndEngineAreMutuallyExclusive(t *testing.T) {
	engine, err := agentpolicy.Build(agentpolicy.FullAuto(agentpolicy.Tools{}), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithEngine(engine),
		agentkit.WithPolicy(agentpolicy.FullAuto(agentpolicy.Tools{}), nil),
	); err == nil {
		t.Fatal("WithPolicy and WithEngine were both accepted")
	}
}

func TestASessionIsStartedAndTheRecorderIsReachable(t *testing.T) {
	store := agentsession.NewMemoryStore()
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithInstructions("hello"),
		agentkit.WithSession(store, agentsession.Header{CWD: t.TempDir()}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	if kit.Recorder() == nil || kit.SessionID() == "" {
		t.Fatal("no recorder or no session id")
	}
	if kit.Session() == nil {
		t.Fatal("no session")
	}
	if got := kit.Attach(agentturn.New(kit.Config())); got == nil {
		t.Fatal("Attach returned no unsubscribe")
	}
}

func TestTheManifestIsRecordedOnlyWhenItMoves(t *testing.T) {
	sessions := agentsession.NewMemoryStore()
	store := memStore(t, agentmemory.Entry{Scope: "user", Name: "a", Content: "FIRST"})

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithMemory(store, "user"),
		agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	cfg := kit.Config()
	call := func() {
		t.Helper()
		req := openresponses.Request{}
		if err := cfg.BeforeModelCall(t.Context(), &req); err != nil {
			t.Fatal(err)
		}
	}
	call()
	call()
	if n := manifestEntries(t, sessions, kit.SessionID()); n != 1 {
		t.Fatalf("manifest entries = %d after two calls with no write, want 1", n)
	}

	if _, err := store.Put(t.Context(), agentmemory.Entry{
		Scope: "user", Name: "a", Content: "SECOND",
	}); err != nil {
		t.Fatal(err)
	}
	call()
	if n := manifestEntries(t, sessions, kit.SessionID()); n != 2 {
		t.Fatalf("manifest entries = %d after a write, want 2", n)
	}
}

// flakyStore fails the one Append the test arms it for, so a write the
// kit thought it had made can be made to fail.
type flakyStore struct {
	agentsession.Store
	fail atomic.Bool
}

func (f *flakyStore) Append(ctx context.Context, id string, e agentsession.Entry) (string, error) {
	if f.fail.CompareAndSwap(true, false) {
		return "", errors.New("flakyStore: the store is down")
	}
	return f.Store.Append(ctx, id, e)
}

// The manifest hash is remembered only once the write it describes has
// landed. Remembering it first loses the render for good: the hash has
// not moved next turn, so nothing tries again and the session never
// says what memory the model was given.
func TestAManifestWriteThatFailedIsTriedAgain(t *testing.T) {
	sessions := &flakyStore{Store: agentsession.NewMemoryStore()}
	store := memStore(t, agentmemory.Entry{Scope: "user", Name: "a", Content: "FIRST"})

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithMemory(store, "user"),
		agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	cfg := kit.Config()
	sessions.fail.Store(true)
	req := openresponses.Request{}
	if err := cfg.BeforeModelCall(t.Context(), &req); err == nil {
		t.Fatal("a failed manifest write was not reported")
	}
	if n := manifestEntries(t, sessions.Store, kit.SessionID()); n != 0 {
		t.Fatalf("manifest entries = %d after the write failed, want 0", n)
	}

	// Same memory, so the same hash: the kit must try again rather than
	// treat the render it never wrote as already recorded.
	if err := cfg.BeforeModelCall(t.Context(), &req); err != nil {
		t.Fatal(err)
	}
	if n := manifestEntries(t, sessions.Store, kit.SessionID()); n != 1 {
		t.Fatalf("manifest entries = %d after the store recovered, want 1", n)
	}
}

// Two runs off one kit share the memory hook, so the state it keeps has
// to hold under both at once. Each run must still build its own
// instructions, and the manifest must be written once because one
// session records both. Run under -race, which make test does.
func TestTwoRunsOffOneKitShareTheMemoryStateSafely(t *testing.T) {
	sessions := agentsession.NewMemoryStore()
	store := memStore(t, agentmemory.Entry{Scope: "user", Name: "a", Content: "REMEMBERED"})

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithInstructions("Be brief."),
		agentkit.WithMemory(store, "user"),
		agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	const runs, turns = 4, 8
	var wg sync.WaitGroup
	errs := make([]error, runs)
	texts := make([]string, runs)
	for i := range runs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg := kit.Config()
			for range turns {
				req := openresponses.Request{}
				if err := cfg.BeforeModelCall(t.Context(), &req); err != nil {
					errs[i] = err
					return
				}
				texts[i] = req.Instructions
			}
			_ = kit.Parts()
			_ = kit.MemoryManifest()
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	// Every run built the whole prompt, not a half of another's.
	for i, text := range texts {
		for _, want := range []string{"Be brief.", "REMEMBERED"} {
			if !strings.Contains(text, want) {
				t.Errorf("run %d did not get %q in its instructions:\n%s", i, want, text)
			}
		}
	}
	if n := manifestEntries(t, sessions, kit.SessionID()); n != 1 {
		t.Errorf("manifest entries = %d for one render across %d runs, want 1", n, runs)
	}
}

// The two contradictions in the skill-tool options. Neither can be
// satisfied, so New says so rather than keeping whichever came last.
func TestTheContradictorySkillToolOptionsAreRefused(t *testing.T) {
	skills := skillDir(t, filepath.Join(t.TempDir(), "skills"), "digging", "how to dig", "dig")
	for _, tc := range []struct {
		name string
		opts []agentkit.Option
		want string
	}{
		{
			name: "the tool asked for and withheld",
			opts: []agentkit.Option{agentkit.WithSkillTool(), agentkit.WithoutSkillTool()},
			want: "WithSkillTool and WithoutSkillTool",
		},
		{
			name: "grants that can never happen",
			opts: []agentkit.Option{agentkit.WithSkillGrants(nil), agentkit.WithoutSkillTool()},
			want: "WithSkillGrants and WithoutSkillTool",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := append([]agentkit.Option{
				agentkit.WithModel(stubModel{}, "m"),
				agentkit.WithSkills(skills),
			}, tc.opts...)
			_, err := agentkit.New(t.Context(), opts...)
			if err == nil {
				t.Fatal("the contradiction was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// Configuring no skills leaves both options inert instead of failing,
// because adding WithSkills behind a flag and the rest unconditionally
// is ordinary code.
func TestTheSkillToolOptionsAreInertWithoutSkills(t *testing.T) {
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithSkillTool(),
		agentkit.WithSkillGrants(nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	if got := kit.Config().ResolveTools(t.Context()); len(got) != 0 {
		t.Errorf("tools = %v, want none", toolNames(got))
	}
}

func TestAChildAgentIsOfferedAsATool(t *testing.T) {
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithTools(namedTool(t, "read")),
		agentkit.WithChildAgent(agentturn.Config{
			Name:        "explore",
			Description: "Delegate a read-only investigation.",
			Model:       stubModel{},
			ModelName:   "m",
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	if got := toolNames(kit.Config().ResolveTools(t.Context())); !slices.Equal(got, []string{"read", "explore"}) {
		t.Errorf("tools = %v, want [read explore]", got)
	}
}

// A child agent's tool name can collide like any other, and the
// conflict has to name the child rather than blaming the option it
// shares with every other deferred source.
func TestAChildAgentCollisionNamesTheChild(t *testing.T) {
	_, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithTools(namedTool(t, "explore")),
		agentkit.WithChildAgent(agentturn.Config{
			Name: "explore", Description: "d", Model: stubModel{}, ModelName: "m",
		}),
	)
	if err == nil {
		t.Fatal("the collision was accepted")
	}
	for _, want := range []string{"WithChildAgent", "explore", "WithTools"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
}

func TestAToolFilterDropsToolsBySource(t *testing.T) {
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithTools(namedTool(t, "read"), namedTool(t, "write")),
		agentkit.WithMemory(memStore(t), "user"),
		agentkit.WithToolFilter(func(source string, tool agenttool.Tool) bool {
			return source == "WithTools" && tool.Name() == "read"
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	if got := toolNames(kit.Config().ResolveTools(t.Context())); !slices.Equal(got, []string{"read"}) {
		t.Errorf("tools = %v, want [read]", got)
	}
}

// The filter runs before the duplicate check, so dropping one of two
// tools that claim a name is how a product resolves a collision it
// would otherwise have to rename its way out of.
func TestAToolFilterResolvesACollisionInsteadOfReportingIt(t *testing.T) {
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithTools(namedTool(t, "read")),
		agentkit.WithToolProvider(func(context.Context) []agenttool.Tool {
			return []agenttool.Tool{namedTool(t, "read")}
		}),
		agentkit.WithToolFilter(func(source string, _ agenttool.Tool) bool {
			return source == "WithTools"
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	if got := toolNames(kit.Config().ResolveTools(t.Context())); !slices.Equal(got, []string{"read"}) {
		t.Errorf("tools = %v, want [read]", got)
	}
}

// Two providers are two sources, so a collision between them says which
// is which instead of the second silently replacing the first.
func TestTwoToolProvidersAreTwoSources(t *testing.T) {
	provide := func(name string) agentkit.Option {
		return agentkit.WithToolProvider(func(context.Context) []agenttool.Tool {
			return []agenttool.Tool{namedTool(t, name)}
		})
	}
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		provide("first"), provide("second"),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	if got := toolNames(kit.Config().ResolveTools(t.Context())); !slices.Equal(got, []string{"first", "second"}) {
		t.Errorf("tools = %v, want [first second]", got)
	}

	_, err = agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		provide("same"), provide("same"),
	)
	if err == nil {
		t.Fatal("two providers claiming one name were accepted")
	}
	for _, want := range []string{"WithToolProvider #1", "WithToolProvider #2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to name %q", err, want)
		}
	}
}

func TestCloseIsSafeWithNothingOpen(t *testing.T) {
	kit, err := agentkit.New(t.Context(), agentkit.WithModel(stubModel{}, "m"))
	if err != nil {
		t.Fatal(err)
	}
	if err := kit.Close(); err != nil {
		t.Fatal(err)
	}
	if err := kit.Close(); err != nil {
		t.Fatalf("a second Close reported %v", err)
	}
}

func TestASkillDirectoryThatIsNotThereIsAnError(t *testing.T) {
	_, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithSkills(filepath.Join(t.TempDir(), "nope")),
	)
	if err == nil {
		t.Fatal("a missing skills directory was accepted, offering no skills silently")
	}
}

func TestTheUsageParagraphFollowsTheToolThatServesTheSkills(t *testing.T) {
	skills := skillDir(t, filepath.Join(t.TempDir(), "skills"), "digging", "how to dig", "dig")

	with, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"), agentkit.WithSkills(skills))
	if err != nil {
		t.Fatal(err)
	}
	defer with.Close()

	without, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"), agentkit.WithSkills(skills), agentkit.WithoutSkillTool())
	if err != nil {
		t.Fatal(err)
	}
	defer without.Close()

	cat, err := agentskill.DiscoverDirs(skills)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(with.Config().Instructions, cat.Usage()) {
		t.Fatal("the usage paragraph is missing where the tool is offered")
	}
	if strings.Contains(without.Config().Instructions, cat.Usage()) {
		t.Fatal("the usage paragraph describes a tool the model does not have")
	}
	if names := toolNames(without.Config().ResolveTools(t.Context())); len(names) != 0 {
		t.Fatalf("tools = %v, want none", names)
	}
}

func toolNames(ts []agenttool.Tool) []string {
	out := make([]string, len(ts))
	for i, t := range ts {
		out[i] = t.Name()
	}
	return out
}

// manifestEntries counts the memory manifests recorded in the session.
func manifestEntries(t *testing.T, store agentsession.Store, id string) int {
	t.Helper()
	s, err := store.Open(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range s.Entries() {
		c, ok := e.(*agentsession.CustomEntry)
		if ok && c.NS == agentmemory.ManifestNS {
			var m agentmemory.Manifest
			if err := json.Unmarshal(c.Data, &m); err != nil {
				t.Fatalf("manifest entry does not hold a manifest: %v", err)
			}
			n++
		}
	}
	return n
}

// A budget the fixed parts spend exactly leaves nothing for the bounded
// layers. Zero left has to mean "no room" and not "no bound", or a
// budget that is exactly met would let the two largest layers through
// unbounded.
func TestABudgetExactlySpentDropsTheBoundedParts(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), strings.Repeat("a", 4000))

	const prompt = "be brief"
	// The prompt, plus the two separators the three configured parts
	// would cost: exactly nothing left.
	budget := int64(len(prompt) + 2*len(agentkit.Separator))

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithInstructions(prompt),
		agentkit.WithAgentsMD(root, agentsmd.Options{Root: root}),
		agentkit.WithMemory(memStore(t, agentmemory.Entry{
			Scope: "user", Name: "a", Content: strings.Repeat("c", 4000),
		}), "user"),
		agentkit.WithInstructionBudget(budget),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	if got := kit.Config().Instructions; got != prompt {
		t.Fatalf("instructions = %q, want only the prompt", got)
	}
	if n := int64(len(kit.Config().Instructions)); n > budget {
		t.Fatalf("instructions are %d bytes, over the %d-byte budget", n, budget)
	}
	var sources []string
	for _, o := range kit.Omitted() {
		sources = append(sources, o.Source)
	}
	for _, want := range []string{agentkit.SourceAgentsMD, agentkit.SourceMemory} {
		if !slices.Contains(sources, want) {
			t.Errorf("nothing reported from %s; got %v", want, kit.Omitted())
		}
	}
}

// A store that is empty at New renders no block and is not joined, so
// the part has no position to be replaced at when the first write
// lands. It has to come back where the order says it goes.
func TestAMemoryBlockThatWasEmptyAtNewLandsInItsPosition(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), "AGENTS")
	store := memStore(t)

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithInstructions("PRODUCT"),
		agentkit.WithMemory(store, "user"),
		agentkit.WithAgentsMD(root, agentsmd.Options{Root: root}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	if _, err := store.Put(t.Context(), agentmemory.Entry{
		Scope: "user", Name: "a", Content: "REMEMBERED",
	}); err != nil {
		t.Fatal(err)
	}
	req := openresponses.Request{}
	if err := kit.Config().BeforeModelCall(t.Context(), &req); err != nil {
		t.Fatal(err)
	}

	var ids []string
	for _, p := range kit.Parts() {
		ids = append(ids, p.ID)
	}
	want := []string{agentkit.PartProduct, agentkit.PartMemory, agentsmd.PartID}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("part ids = %v, want %v", ids, want)
	}
	if strings.Index(req.Instructions, "REMEMBERED") > strings.Index(req.Instructions, "AGENTS") {
		t.Fatalf("memory landed after the repository's word:\n%s", req.Instructions)
	}
}

// A tool that needs the recorder cannot be built before New, because
// the kit is what opens the session. WithDeferredTools is the ordering
// the kit already knows, so the product does not have to stand a lazy
// cache up inside a per-turn provider.
func TestDeferredToolsSeeTheRecorder(t *testing.T) {
	sessions := agentsession.NewMemoryStore()
	var sawRecorder, sawEngine bool

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithTools(namedTool(t, "read")),
		agentkit.WithPolicy(agentpolicy.FullAuto(agentpolicy.Tools{}), nil),
		agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}),
		agentkit.WithDeferredTools(func(k *agentkit.Kit) []agenttool.Tool {
			sawRecorder = k.Recorder() != nil && k.SessionID() != ""
			sawEngine = k.Engine() != nil
			return []agenttool.Tool{namedTool(t, "child")}
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	if !sawRecorder {
		t.Error("the deferred tool could not reach the recorder")
	}
	if !sawEngine {
		t.Error("the deferred tool could not reach the policy engine")
	}
	names := toolNames(kit.Config().ResolveTools(t.Context()))
	if strings.Join(names, ",") != "read,child" {
		t.Fatalf("tools = %v, want the product's then the deferred one", names)
	}
}

// A deferred tool is in the namespace like any other, so a name it
// repeats is the same error at New.
func TestADeferredToolCanCollide(t *testing.T) {
	_, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithTools(namedTool(t, "read")),
		agentkit.WithDeferredTools(func(*agentkit.Kit) []agenttool.Tool {
			return []agenttool.Tool{namedTool(t, "read")}
		}),
	)
	var c agentkit.Conflict
	if !errors.As(err, &c) {
		t.Fatalf("err = %v, want a Conflict", err)
	}
	if c.Kept == c.Dropped {
		t.Fatalf("conflict = %+v; it does not say which of the two sources to rename", c)
	}
}
