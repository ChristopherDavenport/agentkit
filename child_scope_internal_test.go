package agentkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	childagent "github.com/ChristopherDavenport/agentturn/tools/agent"
	"github.com/ChristopherDavenport/openresponses"
)

// callsThenAnswers calls the tool named by call on its first request
// when call is set, and answers "done" on every other.
type callsThenAnswers struct {
	call string
	n    atomic.Int32
}

func (m *callsThenAnswers) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	e := openresponses.NewEmitter(sink, openresponses.NewResponse(req))
	if err := e.Start(); err != nil {
		return err
	}
	if m.call != "" && m.n.Add(1) == 1 {
		w, err := e.FunctionCall("call-"+m.call, m.call)
		if err != nil {
			return err
		}
		if err := w.Arguments(`{}`); err != nil {
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
		return e.Complete()
	}
	w, err := e.Message(openresponses.PhaseFinalAnswer)
	if err != nil {
		return err
	}
	if err := w.Text("done"); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return e.Complete()
}

// linkedUnder returns the child scopes the kit's registry holds, the
// conversation's and every other.
func linkedUnder(k *Kit) int {
	k.scopes.mu.Lock()
	defer k.scopes.mu.Unlock()
	n := 0
	for _, children := range k.scopes.children {
		n += len(children)
	}
	return n
}

// A child agent's scope was linked to its conversation for every run,
// with or without skill grants, and nothing but RevokeSkillGrants, or a
// message under WithSkillGrantScope, forgot it: a front that offers
// child agents without skills and never ends its conversation, dax's
// agents extension, kept an entry for every explore call for the life
// of the process. A child that ended holding nothing under its scope is
// forgotten when its call returns, and its next run links it again.
func TestChildRunsLeaveNoScopeLinked(t *testing.T) {
	k, err := New(t.Context(),
		WithModel(&callsThenAnswers{}, "m"),
		WithPolicy(agentpolicy.Policy{Default: agentpolicy.Ask()}, nil),
		WithChildAgent(agentturn.Config{Name: "explore", Description: "delegate", Model: &callsThenAnswers{}, ModelName: "m"}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	tool, ok := k.LookupTool("explore")
	if !ok {
		t.Fatal("no explore tool")
	}
	ctx := agentpolicy.ContextWithGrantScope(t.Context(), "host")
	for i := range 1000 {
		if _, err := tool.Execute(ctx, agenttool.Call{ID: fmt.Sprint("call-", i), Args: json.RawMessage(`{"input":"look"}`)}); err != nil {
			t.Fatal(err)
		}
	}
	if n := linkedUnder(k); n != 0 {
		t.Fatalf("after 1000 child runs the registry holds %d child scopes, want none", n)
	}
}

// A child whose run ended waiting on a held call, or holding a set under
// its scope, is still the conversation's: the answer, or a person's
// "allow for this session", is granted under its scope, and
// RevokeSkillGrants must reach it.
func TestAChildThatIsHeldOrHoldsASetStaysLinked(t *testing.T) {
	for _, tc := range []struct {
		name  string
		grant bool
	}{
		{"held on a call", false},
		{"holding a set", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var k *Kit
			where := agenttool.New("where", "says where it runs", func(ctx context.Context, _ struct{}) (string, error) {
				if tc.grant {
					k.Engine().GrantSet(ctx, agentpolicy.RuleSet{
						Source: agentpolicy.Source{Name: "front", Trusted: true},
						Allow:  []agentpolicy.Rule{{Tool: "where"}},
					})
				}
				return "here", nil
			})
			policy := agentpolicy.Policy{Default: agentpolicy.Ask()}
			if tc.grant {
				policy.Allow = []agentpolicy.Rule{{Tool: "where"}}
			}
			var err error
			k, err = New(t.Context(),
				WithModel(&callsThenAnswers{}, "m"),
				WithPolicy(policy, nil),
				WithChildAgent(agentturn.Config{
					Name: "explore", Description: "delegate", Model: &callsThenAnswers{call: "where"}, ModelName: "m",
					Tools: []agenttool.Tool{where},
					BeforeToolCall: func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
						return k.Engine().BeforeToolCall()(ctx, info)
					},
				}),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer k.Close()
			tool, _ := k.LookupTool("explore")
			_, err = tool.Execute(agentpolicy.ContextWithGrantScope(t.Context(), "host"), agenttool.Call{ID: "c", Args: json.RawMessage(`{"input":"look"}`)})
			var held *childagent.InputRequiredError
			if tc.grant && err != nil || !tc.grant && !errors.As(err, &held) {
				t.Fatalf("the child's call returned %v", err)
			}
			if n := linkedUnder(k); n != 1 {
				t.Fatalf("the registry holds %d child scopes, want the child's", n)
			}
		})
	}
}
