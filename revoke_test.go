package agentkit_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// A child agent decides under a scope of its own whenever the kit has an
// engine (#109), and a front may grant under it, but the kit linked the
// child's scope to its conversation only under WithSkillGrants, so
// without skills RevokeSkillGrants ended nothing and a set the front
// activated under a child's scope outlived the conversation. (#121)
// The front grants while the child runs: a child that ended holding
// nothing under its scope is forgotten with its call.
func TestRevokeSkillGrantsEndsAChildScopeWithoutSkillGrants(t *testing.T) {
	var childScope string
	var kit *agentkit.Kit
	var granted []agentpolicy.Rule
	where := agenttool.New("where", "says where it runs", func(ctx context.Context, _ struct{}) (string, error) {
		childScope = agentpolicy.GrantScopeFromContext(ctx)
		granted, _ = kit.Engine().GrantSet(ctx, agentpolicy.RuleSet{
			Source: agentpolicy.Source{Name: "front", Trusted: true},
			Allow:  []agentpolicy.Rule{{Tool: "Bash", Spec: "git:*"}},
		})
		return "here", nil
	})
	bash := agenttool.New("Bash", "run a command", func(context.Context, struct {
		Command string `json:"command"`
	}) (string, error) {
		return "", nil
	})
	child := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurn("where", `{}`)}}
	parent := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurn("explore", `{"input":"look"}`)}}
	sessions := agentsession.NewMemoryStore()
	var err error
	kit, err = agentkit.New(t.Context(),
		agentkit.WithModel(parent, "m"),
		agentkit.WithTools(bash),
		agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{
			Read:    []string{"explore", "where"},
			Execute: []string{"Bash"},
		}), map[string]agentpolicy.ToolMatcher{"Bash": {Match: agentpolicy.PrefixMatcher("command")}}),
		agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}),
		agentkit.WithChildAgent(agentturn.Config{
			Name: "explore", Description: "delegate", Model: child, ModelName: "m",
			Tools: []agenttool.Tool{where, bash},
			BeforeToolCall: func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
				return kit.Engine().BeforeToolCall()(ctx, info)
			},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	ctx := agentpolicy.ContextWithGrantScope(t.Context(), "host")
	agent := agentturn.New(kit.Config())
	defer kit.Attach(agent)()
	if _, err := agent.Prompt(ctx, openresponses.UserText("go")); err != nil {
		t.Fatal(err)
	}
	if childScope == "" || childScope == "host" {
		t.Fatalf("the child ran under scope %q, want one of its own", childScope)
	}
	cctx := agentpolicy.ContextWithGrantScope(t.Context(), childScope)
	if len(granted) != 1 {
		t.Fatalf("the front's grant under the child's scope: %v", granted)
	}
	call := agentturn.ToolCallInfo{Call: &openresponses.FunctionCall{CallID: "c", Name: "Bash", Arguments: `{"command":"git status"}`}, Args: json.RawMessage(`{"command":"git status"}`)}
	if v, err := kit.Engine().Would(cctx, call); err != nil || v.Action != agentturn.Allow {
		t.Fatalf("before the revocation the child's call is %v (%v), want allowed by the front's grant", v.Action, err)
	}

	n, err := kit.RevokeSkillGrants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("RevokeSkillGrants removed %d rules, want the child scope's one", n)
	}
	if v, err := kit.Engine().Would(cctx, call); err != nil || v.Action != agentturn.Defer {
		t.Fatalf("after the revocation the child's call is %v (%v), want asked", v.Action, err)
	}
	sess, err := sessions.Open(t.Context(), kit.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range customEntries(sess, agentpolicy.VerdictNS) {
		var v struct {
			Grant string `json:"grant"`
			Scope string `json:"scope"`
		}
		if json.Unmarshal(c.Data, &v) == nil && v.Grant == string(agentpolicy.KindRevokeScope) && v.Scope == childScope {
			found = true
		}
	}
	if !found {
		t.Fatal("no revoke_scope verdict for the child's scope is recorded")
	}
}

// switchStore refuses the policy verdicts the recorder appends while
// refuse is set.
type switchStore struct {
	*agentsession.MemoryStore
	refuse *atomic.Bool
}

func (s switchStore) Append(ctx context.Context, id string, e agentsession.Entry) (string, error) {
	if c, ok := e.(*agentsession.CustomEntry); ok && c.NS == agentpolicy.VerdictNS && s.refuse.Load() {
		return "", errNoVerdicts
	}
	return s.MemoryStore.Append(ctx, id, e)
}

// A revocation the session would not record was dropped: the kit keeps a
// lost verdict only under a run's ID, RevokeSkillGrants is called
// between runs, and it returned a count alone, so a front ending a
// conversation never heard that the journal a restart replays says the
// grant is still in force. It returns the error now, the engine's
// revocation's and the kit's own for a set that held no rule alike, and
// revokes all the same. (#117)
func TestRevokeSkillGrantsReturnsALostRevocationWrite(t *testing.T) {
	for _, tc := range []struct {
		name    string
		source  func(*agentskill.Skill) agentpolicy.Source
		removed int
	}{
		{"the engine's revocation", trustedSkills, 1},
		{"the kit's own, of a set that held no rule", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "sysinfo", "Bash")
			refuse := &atomic.Bool{}
			kit, err := agentkit.New(t.Context(),
				agentkit.WithModel(stubModel{}, "m"),
				agentkit.WithSkills(skills),
				agentkit.WithSkillGrants(tc.source),
				agentkit.WithPolicy(agentpolicy.Policy{Default: agentpolicy.Ask()}, nil),
				agentkit.WithSession(switchStore{agentsession.NewMemoryStore(), refuse}, agentsession.Header{}),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer kit.Close()
			readSkill(t, skillTool(t, kit), "sysinfo")
			refuse.Store(true)
			n, err := kit.RevokeSkillGrants(t.Context())
			if !errors.Is(err, errNoVerdicts) {
				t.Fatalf("RevokeSkillGrants returned %v, want the write the store refused", err)
			}
			if n != tc.removed {
				t.Fatalf("RevokeSkillGrants removed %d rules, want %d", n, tc.removed)
			}
			if sets := kit.Engine().GrantsFor(agentpolicy.ContextWithGrantScope(t.Context(), kit.GrantScope(t.Context()))); len(sets) != 0 {
				t.Fatalf("the engine still holds %v after a revocation whose write failed", sets)
			}
			refuse.Store(false)
			if _, err := kit.RevokeSkillGrants(t.Context()); err != nil {
				t.Fatalf("a revocation after one whose write failed returned %v again", err)
			}
		})
	}
}

// A verdict the session refused outside any run, a product's own rule
// change between runs, was dropped. It is kept for the session, and the
// next run recorded there fails on it, once. (#117)
func TestAVerdictLostBetweenRunsFailsTheNextRun(t *testing.T) {
	refuse := &atomic.Bool{}
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(&scriptModel{}, "m"),
		agentkit.WithPolicy(agentpolicy.Policy{Default: agentpolicy.Ask()}, nil),
		agentkit.WithSession(switchStore{agentsession.NewMemoryStore(), refuse}, agentsession.Header{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	refuse.Store(true)
	if err := kit.Engine().Grant(t.Context(), agentpolicy.Rule{Tool: "Read"}); err != nil {
		t.Fatal(err)
	}
	refuse.Store(false)
	agent := agentturn.New(kit.Config())
	defer kit.Attach(agent)()
	end, err := agent.Prompt(t.Context(), openresponses.UserText("go"))
	if err == nil && end != nil {
		err = end.Err
	}
	if !errors.Is(err, errNoVerdicts) {
		t.Fatalf("the next run ended with %v, want the verdict lost between runs", err)
	}
	end, err = agent.Prompt(t.Context(), openresponses.UserText("again"))
	if err == nil && end != nil {
		err = end.Err
	}
	if err != nil {
		t.Fatalf("the run after it failed too: %v", err)
	}
}

// A head move whose revocation the session would not record fails, and
// leaves the head, the agent and the grant where they were: the branch
// left would say the grant is still in force, and a move that went on
// ignored it. (#116, #117)
func TestControlContinueFromFailsOnALostRevocation(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "digging", "danger")
	refuse := &atomic.Bool{}
	store := switchStore{agentsession.NewMemoryStore(), refuse}
	model := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurn(agentskill.ToolName, `{"name":"digging"}`)}}
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "test-model"),
		agentkit.WithTools(counting("danger", &atomic.Int32{})),
		agentkit.WithSkills(skills),
		agentkit.WithPolicy(agentpolicy.Policy{Allow: policyRules(t, "skill"), Default: agentpolicy.Ask()}, nil),
		agentkit.WithSkillGrants(trustedSkills),
		agentkit.WithSession(store, agentsession.Header{Records: agentsession.AllRecords}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	agent := agentturn.New(kit.Config(), kit.AgentOptions()...)
	defer kit.Attach(agent)()
	ctl, err := kit.Control(agent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ctl.Prompt(t.Context(), openresponses.UserText("learn")); err != nil {
		t.Fatal(err)
	}
	grants := func() int {
		ctx := agentpolicy.ContextWithGrantScope(t.Context(), kit.GrantScope(ctl.RunContext(t.Context())))
		return len(kit.Engine().GrantsFor(ctx))
	}
	if grants() == 0 {
		t.Fatal("no grant after the read")
	}
	var user string
	for _, e := range kit.Session().Entries() {
		if it, ok := e.(*agentsession.ItemEntry); ok {
			if m, ok := it.Item.(*openresponses.Message); ok && m.Role == openresponses.RoleUser {
				user = e.Base().ID
				break
			}
		}
	}
	leaf, items := kit.Session().Leaf(), len(ctl.State().Transcript)
	refuse.Store(true)
	if err := ctl.ContinueFrom(t.Context(), user); !errors.Is(err, errNoVerdicts) {
		t.Fatalf("the move returned %v, want the revocation the store refused", err)
	}
	refuse.Store(false)
	if got := kit.Session().Leaf(); got != leaf {
		t.Fatalf("the leaf is %s after the failed move, want %s", got, leaf)
	}
	if n := len(ctl.State().Transcript); n != items {
		t.Fatalf("the transcript has %d items after the failed move, want %d", n, items)
	}
	if grants() == 0 {
		t.Fatal("the grant was not put back after the failed move")
	}
}

// A revocation's verdict may be reported on another goroutine, the one
// already reporting an earlier change, while RevokeSkillGrants waits for
// it. The write that fails there still reaches the call's result: the
// sink rides on the context the engine keeps with the verdict. (#117)
func TestARevocationReportedOnAnotherGoroutineReachesItsCall(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "sysinfo", "Bash")
	refuse := &atomic.Bool{}
	entered, release := make(chan struct{}), make(chan struct{})
	var blocked atomic.Bool
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(&scriptModel{}, "m"),
		agentkit.WithSkills(skills),
		agentkit.WithSkillGrants(trustedSkills),
		agentkit.WithPolicy(agentpolicy.Policy{Default: agentpolicy.Ask()}, nil),
		agentkit.WithSession(switchStore{agentsession.NewMemoryStore(), refuse}, agentsession.Header{}),
		agentkit.WithVerdictObserver(func(_ context.Context, v agentpolicy.Verdict) {
			// The product's own grant: its report holds the engine's
			// reporting goroutine until the revocation is queued behind it.
			if v.Grant == agentpolicy.KindGrant && blocked.CompareAndSwap(false, true) {
				close(entered)
				<-release
			}
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	readSkill(t, skillTool(t, kit), "sysinfo")
	scope := agentpolicy.ContextWithGrantScope(t.Context(), kit.GrantScope(t.Context()))
	if len(kit.Engine().GrantsFor(scope)) == 0 {
		t.Fatal("no grant after the read")
	}
	granted := make(chan error, 1)
	go func() { granted <- kit.Engine().Grant(t.Context(), agentpolicy.Rule{Tool: "Read"}) }()
	<-entered
	refuse.Store(true)
	type result struct {
		n   int
		err error
	}
	revoked := make(chan result, 1)
	go func() {
		n, err := kit.RevokeSkillGrants(t.Context())
		revoked <- result{n, err}
	}()
	// The revocation took effect, and its verdict is queued, under the
	// engine's lock; the reporting goroutine is still in the observer.
	deadline := time.Now().Add(10 * time.Second)
	for len(kit.Engine().GrantsFor(scope)) > len(kit.Engine().GrantsFor(t.Context())) {
		if time.Now().After(deadline) {
			t.Fatal("the revocation never took effect")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case r := <-revoked:
		t.Fatalf("RevokeSkillGrants returned (%d, %v) before its verdict was reported", r.n, r.err)
	default:
	}
	close(release)
	if err := <-granted; err != nil {
		t.Fatal(err)
	}
	r := <-revoked
	if !errors.Is(r.err, errNoVerdicts) {
		t.Fatalf("RevokeSkillGrants returned %v, want the write the store refused on the reporting goroutine", r.err)
	}
	if r.n != 1 {
		t.Fatalf("RevokeSkillGrants removed %d rules, want 1", r.n)
	}
	// Returned once, to the call: the next run does not fail on it too.
	agent := agentturn.New(kit.Config())
	defer kit.Attach(agent)()
	end, err := agent.Prompt(t.Context(), openresponses.UserText("go"))
	if err == nil && end != nil {
		err = end.Err
	}
	if err != nil {
		t.Fatalf("the run after the call that returned the loss failed with %v", err)
	}
}

// RevokeSkillGrants called with the context an observer was handed does
// not wait for its verdict, which is reported after it returns, by the
// report that called it. The write that fails then has no call to go
// to: it is kept for the session, and the next run fails on it. (#117)
func TestARevocationReportedAfterItsCallFailsTheNextRun(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "sysinfo", "Bash")
	refuse := &atomic.Bool{}
	var kit *agentkit.Kit
	var fired atomic.Bool
	type result struct {
		n   int
		err error
	}
	var inside result
	var err error
	kit, err = agentkit.New(t.Context(),
		agentkit.WithModel(&scriptModel{}, "m"),
		agentkit.WithSkills(skills),
		agentkit.WithSkillGrants(trustedSkills),
		agentkit.WithPolicy(agentpolicy.Policy{Default: agentpolicy.Ask()}, nil),
		agentkit.WithSession(switchStore{agentsession.NewMemoryStore(), refuse}, agentsession.Header{}),
		agentkit.WithVerdictObserver(func(ctx context.Context, v agentpolicy.Verdict) {
			// The product's own grant, made with no skill read or
			// revocation in flight, so the kit's grant lock is free: the
			// case RevokeSkillGrants' doc names.
			if v.Grant == agentpolicy.KindGrant && fired.CompareAndSwap(false, true) {
				refuse.Store(true)
				inside.n, inside.err = kit.RevokeSkillGrants(ctx)
			}
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	readSkill(t, skillTool(t, kit), "sysinfo")
	if err := kit.Engine().Grant(t.Context(), agentpolicy.Rule{Tool: "Read"}); err != nil {
		t.Fatal(err)
	}
	refuse.Store(false)
	if !fired.Load() {
		t.Fatal("the observer never saw the product's grant")
	}
	if inside.err != nil || inside.n != 1 {
		t.Fatalf("RevokeSkillGrants inside the observer returned (%d, %v), want (1, nil): its verdict is reported after it returns", inside.n, inside.err)
	}
	agent := agentturn.New(kit.Config())
	defer kit.Attach(agent)()
	end, err := agent.Prompt(t.Context(), openresponses.UserText("go"))
	if err == nil && end != nil {
		err = end.Err
	}
	if !errors.Is(err, errNoVerdicts) {
		t.Fatalf("the next run ended with %v, want the revocation lost after its call returned", err)
	}
}
