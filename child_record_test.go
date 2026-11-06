package agentkit_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	childagent "github.com/ChristopherDavenport/agentturn/tools/agent"
	"github.com/ChristopherDavenport/openresponses"
)

// cutStore fails every append once cut: the process stopped, as far as
// the record can tell.
type cutStore struct {
	agentsession.Store
	cut atomic.Bool
}

func (s *cutStore) Append(ctx context.Context, id string, e agentsession.Entry) (string, error) {
	if s.cut.Load() {
		return "", errors.New("the process died")
	}
	return s.Store.Append(ctx, id, e)
}

// A WithChildAgent child whose call the policy put to the person
// (childagent.WithAskParent) is cut by a crash while the question is
// open. After the restart the parent's call, in flight, is run again
// under its key, since the kit gave the child the recorder's loader
// (childagent.WithRecord, Recorder.ChildRecord): the child continues
// its session rather than starting afresh, the same question is asked
// again under the same parent call, the child's call runs once after
// the accept, and the child's grant scope, which names its session, is
// linked to the conversation again, so RevokeSkillGrants ends a set
// made under it. (#124)
func TestAChildsOpenQuestionSurvivesARestart(t *testing.T) {
	ctx := t.Context()
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "release", "Bash(git:*)")
	store := &cutStore{Store: agentsession.NewMemoryStore()}

	var mu sync.Mutex
	type question struct{ under, about string }
	var asked []question
	var scopes []string
	lookups := 0
	var kit *agentkit.Kit
	lookup := agenttool.New("lookup", "looks it up", func(ctx context.Context, _ agenttool.NoArgs) (string, error) {
		mu.Lock()
		lookups++
		mu.Unlock()
		// A set a front makes under the child's scope: the conversation's
		// RevokeSkillGrants reaches it only if the scope is linked.
		kit.Engine().GrantSet(ctx, agentpolicy.RuleSet{
			Source: agentpolicy.Source{Name: "front", Trusted: true},
			Allow:  []agentpolicy.Rule{{Tool: "lookup"}},
		})
		return "FOUND", nil
	})
	ask := func(answer func() (agenttool.Answer, error)) agenttool.Elicitor {
		return func(ctx context.Context, _ agenttool.Elicitation) (agenttool.Answer, error) {
			about, _ := agentturn.AskedCallFrom(ctx)
			under, _ := agenttool.CallFrom(ctx)
			mu.Lock()
			asked = append(asked, question{under: under.ID, about: about.CallID})
			mu.Unlock()
			return answer()
		}
	}
	// The models of the process after the restart answer: the calls
	// they made before it are on the record.
	build := func(opened agentkit.Option, elicit agenttool.Elicitor, restarted bool) *agentkit.Kit {
		t.Helper()
		parent := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurnID("call-a", "explore", `{"input":"find"}`)}}
		child := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurnID("call-l", "lookup", `{}`)}}
		if restarted {
			parent.turns, child.turns = nil, nil
		}
		k, err := agentkit.New(ctx,
			agentkit.WithModel(parent, "m"),
			agentkit.WithSkills(skills),
			agentkit.WithPolicy(agentpolicy.Policy{
				Default: agentpolicy.Ask(),
				Allow:   []agentpolicy.Rule{{Tool: "explore"}, {Tool: agentskill.ToolName}},
			}, nil),
			agentkit.WithSkillGrants(trustedSkills),
			opened,
			agentkit.WithToolElicitor(agentsession.ByHuman, elicit),
			agentkit.WithChildAgent(agentturn.Config{
				Name: "explore", Description: "delegate", Model: child, ModelName: "m",
				Tools: []agenttool.Tool{lookup},
				BeforeToolCall: func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
					mu.Lock()
					scopes = append(scopes, agentpolicy.GrantScopeFromContext(ctx))
					mu.Unlock()
					return kit.Engine().BeforeToolCall()(ctx, info)
				},
			}, agentkit.ChildAgentOptions(childagent.WithAskParent())),
		)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}

	// The process dies while the person decides.
	kit = build(agentkit.WithSession(store, agentsession.Header{CWD: t.TempDir()}),
		ask(func() (agenttool.Answer, error) {
			store.cut.Store(true)
			return agenttool.Answer{}, errors.New("the process died")
		}), false)
	sid := kit.SessionID()
	a := agentturn.New(kit.Config())
	unsub := kit.Attach(a)
	_, _ = a.Prompt(ctx, openresponses.UserText("find"))
	unsub()
	_ = kit.Close()
	store.cut.Store(false)
	if len(asked) != 1 || asked[0].under != "call-a" || asked[0].about == "" {
		t.Fatalf("asked %+v before the crash, want one question under call-a", asked)
	}
	first := asked[0]
	asked = nil
	if len(scopes) != 1 {
		t.Fatalf("the child decided %d calls before the crash, want one", len(scopes))
	}
	firstScope := scopes[0]
	scopes = nil

	kit = build(agentkit.WithResumedSession(store, sid),
		ask(func() (agenttool.Answer, error) { return agenttool.Answer{Action: agenttool.ActionAccept}, nil }), true)
	defer kit.Close()
	b := agentturn.New(kit.Config(), kit.AgentOptions()...)
	defer kit.Attach(b)()
	ctl, err := kit.Control(b)
	if err != nil {
		t.Fatal(err)
	}
	answers, err := session.ReplayAnswers(ctx, kit.Session(), kit.Config().ResolveTools(ctx), kit.Recorder().ReadOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	if len(answers) != 1 || answers[0].CallID != "call-a" || answers[0].Output != nil || answers[0].Reason != agentturn.RunAgainKeyedReason {
		t.Fatalf("answers = %+v, want the parent's call run again under its key", answers)
	}
	end, err := ctl.Resume(ctx, answers...)
	if err != nil {
		t.Fatal(err)
	}
	if end.Reason != agentturn.ReasonDone {
		t.Fatalf("resume ended %s: %v", end.Reason, end.Err)
	}
	if len(asked) != 1 || asked[0] != first {
		t.Errorf("asked %+v after the restart, want the question %+v again", asked, first)
	}
	if lookups != 1 {
		t.Errorf("lookup ran %d times, want once", lookups)
	}
	if len(scopes) != 1 || scopes[0] != firstScope {
		t.Errorf("the child decided under %q after the restart, want its scope %q again", scopes, firstScope)
	}
	n, err := kit.RevokeSkillGrants(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n < 1 || len(kit.Engine().Grants()) != 0 {
		t.Errorf("RevokeSkillGrants ended %d set(s), %d left: the child's scope is not linked to the conversation", n, len(kit.Engine().Grants()))
	}
}
