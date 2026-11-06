package agentkit_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

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
func TestRevokeSkillGrantsEndsAChildScopeWithoutSkillGrants(t *testing.T) {
	var childScope string
	where := agenttool.New("where", "says where it runs", func(ctx context.Context, _ struct{}) (string, error) {
		childScope = agentpolicy.GrantScopeFromContext(ctx)
		return "here", nil
	})
	bash := agenttool.New("Bash", "run a command", func(context.Context, struct {
		Command string `json:"command"`
	}) (string, error) {
		return "", nil
	})
	var kit *agentkit.Kit
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
	if granted, _ := kit.Engine().GrantSet(cctx, agentpolicy.RuleSet{
		Source: agentpolicy.Source{Name: "front", Trusted: true},
		Allow:  []agentpolicy.Rule{{Tool: "Bash", Spec: "git:*"}},
	}); len(granted) != 1 {
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
