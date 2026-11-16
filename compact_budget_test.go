package agentkit_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/openresponses"
)

// scopedFold is a fold as a conversation's fold observer heard it: the
// conversation's grant scope and the budget the fold was made under.
type scopedFold struct {
	scope     string
	budget    int
	requested bool
}

// foldLog is the folds a kit's observer heard, safe for a fold made
// while the test changes a budget.
type foldLog struct {
	mu    sync.Mutex
	folds []scopedFold
}

func (l *foldLog) of(scope string) []scopedFold {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []scopedFold
	for _, f := range l.folds {
		if f.scope == scope {
			out = append(out, f)
		}
	}
	return out
}

// budgetKit is a kit that records the person's conversation, compacting
// under a kit-wide budget nothing reaches, whose fold observer notes
// each fold's conversation, and the person's Control.
func budgetKit(t *testing.T, model agentturn.Model) (*agentkit.Kit, *agentkit.Control, *foldLog) {
	t.Helper()
	var (
		kit *agentkit.Kit
		log foldLog
	)
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "m"),
		agentkit.WithInstructions("Be brief."),
		agentkit.WithCompaction(1_000_000, compact.WithKeepLast(1), compact.WithMinFold(0)),
		agentkit.WithFoldObserver(func(ctx context.Context, f compact.Fold) {
			log.mu.Lock()
			defer log.mu.Unlock()
			log.folds = append(log.folds, scopedFold{scope: kit.GrantScope(ctx), budget: f.Budget, requested: f.Requested})
		}),
		agentkit.WithSession(agentsession.NewMemoryStore(), agentsession.Header{}),
	)
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
	return kit, ctl, &log
}

// caller is a conversation of its own on kit, as an A2A caller's is:
// an agent over the kit's config, prompted under a grant scope.
type caller struct {
	agent *agentturn.Agent
	ctx   context.Context
}

func newCaller(t *testing.T, kit *agentkit.Kit, scope string) caller {
	return caller{agentturn.New(kit.Config()), agentpolicy.ContextWithGrantScope(t.Context(), scope)}
}

func (c caller) prompt(t *testing.T, text string) {
	t.Helper()
	if _, err := c.agent.Prompt(c.ctx, openresponses.UserText(text)); err != nil {
		t.Fatal(err)
	}
}

func promptCtl(t *testing.T, ctl *agentkit.Control, text string) {
	t.Helper()
	if _, err := ctl.Prompt(t.Context(), openresponses.UserText(text)); err != nil {
		t.Fatal(err)
	}
}

// Two conversations on one kit fold at their own budgets: a caller given
// a budget of its own folds under it while the person's conversation,
// on the kit-wide one, does not, and the person's own, set later, is
// the one the person's fold is made and recorded under. (#146)
func TestEachConversationFoldsUnderItsOwnBudget(t *testing.T) {
	kit, ctl, log := budgetKit(t, &scriptModel{})
	person := kit.GrantScope(t.Context())
	a := newCaller(t, kit, "a2a:caller-1")
	if err := kit.SetCompactionBudgetFor("a2a:caller-1", 1); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"one", "two"} {
		promptCtl(t, ctl, text)
		a.prompt(t, text)
	}
	if got := log.of(person); len(got) != 0 {
		t.Fatalf("the person's conversation folded %+v under the kit-wide budget nothing reached", got)
	}
	got := log.of("a2a:caller-1")
	if len(got) == 0 {
		t.Fatal("the caller, whose budget is 1, never folded")
	}
	for _, f := range got {
		if f.budget != 1 {
			t.Errorf("the caller's fold was made under %d, want its own 1", f.budget)
		}
	}

	// The person's own budget, and the caller's taken away (0, no budget).
	if err := kit.SetCompactionBudgetFor(person, 2); err != nil {
		t.Fatal(err)
	}
	if err := kit.SetCompactionBudgetFor("a2a:caller-1", 0); err != nil {
		t.Fatal(err)
	}
	before := len(log.of("a2a:caller-1"))
	promptCtl(t, ctl, "three")
	a.prompt(t, "three")
	if got := log.of(person); len(got) == 0 || got[0].budget != 2 {
		t.Fatalf("the person's folds = %+v, want one under its own budget 2", got)
	}
	if got := foldCalls(t, kit.Session()); len(got) == 0 || got[0].Budget == nil || *got[0].Budget != 2 {
		t.Errorf("the person's recorded fold = %+v, want budget 2", got)
	}
	if after := len(log.of("a2a:caller-1")); after != before {
		t.Errorf("the caller folded %d more times under no budget", after-before)
	}

	plain, err := agentkit.New(t.Context(), agentkit.WithModel(&scriptModel{}, "m"))
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	if err := plain.SetCompactionBudgetFor("a2a:caller-1", 10); !errors.Is(err, agentkit.ErrNoCompaction) {
		t.Fatalf("SetCompactionBudgetFor without compaction = %v, want ErrNoCompaction", err)
	}
}

// A conversation with no budget of its own folds under the kit-wide
// one, and follows its later changes; one with a budget of its own does
// not. (#146)
func TestAConversationWithNoBudgetFollowsTheKitWideOne(t *testing.T) {
	kit, _, log := budgetKit(t, &scriptModel{})
	a, b := newCaller(t, kit, "a2a:caller-1"), newCaller(t, kit, "a2a:caller-2")
	if err := kit.SetCompactionBudgetFor("a2a:caller-2", 1_000_000); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"one", "two"} {
		a.prompt(t, text)
		b.prompt(t, text)
	}
	if got := log.of("a2a:caller-1"); len(got) != 0 {
		t.Fatalf("caller 1 folded %+v under the kit-wide budget nothing reached", got)
	}
	if err := kit.SetCompactionBudget(1); err != nil {
		t.Fatal(err)
	}
	a.prompt(t, "three")
	b.prompt(t, "three")
	if got := log.of("a2a:caller-1"); len(got) == 0 || got[0].budget != 1 {
		t.Errorf("caller 1's folds = %+v, want one under the kit-wide budget, cut to 1", got)
	}
	if got := log.of("a2a:caller-2"); len(got) != 0 {
		t.Errorf("caller 2, whose own budget nothing reached, folded %+v after the kit-wide cut", got)
	}
}

// The kit keeps the budgets of the last 1024 conversations given one;
// a conversation past them folds under the kit-wide budget again. (#146)
func TestAnEvictedConversationsBudgetFallsBackToTheKitWideOne(t *testing.T) {
	kit, _, log := budgetKit(t, &scriptModel{})
	if err := kit.SetCompactionBudgetFor("a2a:first", 1); err != nil {
		t.Fatal(err)
	}
	last := ""
	for i := range 1024 {
		last = fmt.Sprint("a2a:caller-", i)
		if err := kit.SetCompactionBudgetFor(last, 1); err != nil {
			t.Fatal(err)
		}
	}
	first, kept := newCaller(t, kit, "a2a:first"), newCaller(t, kit, last)
	for _, text := range []string{"one", "two"} {
		first.prompt(t, text)
		kept.prompt(t, text)
	}
	if got := log.of("a2a:first"); len(got) != 0 {
		t.Errorf("the evicted conversation folded %+v; want the kit-wide budget, which nothing reached", got)
	}
	if got := log.of(last); len(got) == 0 || got[0].budget != 1 {
		t.Errorf("the last conversation given a budget folded %+v, want under its own 1", got)
	}
}

// Control.Compact folds the Control's conversation, through the
// transform of its own a budget of its own gave it, and not another
// conversation's next request. (#146)
func TestControlCompactFoldsItsOwnConversation(t *testing.T) {
	kit, ctl, log := budgetKit(t, &scriptModel{})
	person := kit.GrantScope(t.Context())
	a := newCaller(t, kit, "a2a:caller-1")
	if err := kit.SetCompactionBudgetFor(person, 0); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"one", "two"} {
		promptCtl(t, ctl, text)
		a.prompt(t, text)
	}
	if err := ctl.Compact(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	a.prompt(t, "three")
	if got := log.of("a2a:caller-1"); len(got) != 0 {
		t.Errorf("the caller took the person's /compact: %+v", got)
	}
	promptCtl(t, ctl, "three")
	if got := log.of(person); len(got) != 1 || !got[0].requested || got[0].budget != 0 {
		t.Errorf("the person's folds = %+v, want the one Compact asked for, under no budget", got)
	}
}

// gateModel is a scriptModel whose first summary request waits for the
// test, so a budget can be changed while a fold is in flight.
type gateModel struct {
	scriptModel
	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (m *gateModel) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	if compact.IsFold(ctx) {
		m.once.Do(func() {
			close(m.started)
			<-m.release
		})
	}
	return m.scriptModel.CreateStream(ctx, req, sink)
}

// A budget changed while the conversation's fold is in flight, its own,
// another conversation's or the kit-wide one, leaves that fold under the
// budget it began with, and applies from the next call. (#146)
func TestABudgetChangedDuringAFoldAppliesNext(t *testing.T) {
	model := &gateModel{started: make(chan struct{}), release: make(chan struct{})}
	kit, _, log := budgetKit(t, model)
	a := newCaller(t, kit, "a2a:caller-1")
	if err := kit.SetCompactionBudgetFor("a2a:caller-1", 1_000_000); err != nil {
		t.Fatal(err)
	}
	a.prompt(t, "one")
	if err := kit.SetCompactionBudgetFor("a2a:caller-1", 1); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := a.agent.Prompt(a.ctx, openresponses.UserText("two"))
		done <- err
	}()
	<-model.started
	for _, set := range []func() error{
		func() error { return kit.SetCompactionBudgetFor("a2a:caller-1", 1_000_000) },
		func() error { return kit.SetCompactionBudgetFor("a2a:caller-2", 5) },
		func() error { return kit.SetCompactionBudget(7) },
	} {
		if err := set(); err != nil {
			t.Fatal(err)
		}
	}
	close(model.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := log.of("a2a:caller-1"); len(got) != 1 || got[0].budget != 1 {
		t.Fatalf("folds = %+v, want the one in flight under the budget it began with, 1", got)
	}
	a.prompt(t, "three")
	if got := log.of("a2a:caller-1"); len(got) != 1 {
		t.Errorf("folded again under its own budget nothing reached: %+v", got)
	}
}
