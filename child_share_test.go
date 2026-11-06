package agentkit_test

import (
	"context"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// A child offered with ShareGrantScope decides under its conversation's
// scope: a set granted there, a person's "allow for this session", lets
// it run the tool unasked. Without the option it is asked under a scope of its own, which
// TestAChildDoesNotTakeTheConversationsGrantWithoutSkillGrants holds.
// (agentkit#120)
func TestChildAgentSharesTheConversationScopeWhenAsked(t *testing.T) {
	var ran []string
	var childScope string
	bash := agenttool.New("Bash", "run a command",
		func(ctx context.Context, in struct {
			Command string `json:"command"`
		}) (string, error) {
			ran = append(ran, in.Command)
			childScope = agentpolicy.GrantScopeFromContext(ctx)
			return "", nil
		})
	var kit *agentkit.Kit
	child := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurn("Bash", `{"command":"git status"}`)}}
	parent := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurn("explore", `{"input":"look"}`)}}
	var err error
	kit, err = agentkit.New(t.Context(),
		agentkit.WithModel(parent, "m"),
		agentkit.WithTools(bash),
		agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{
			Read:    []string{"explore"},
			Execute: []string{"Bash"},
		}), map[string]agentpolicy.ToolMatcher{"Bash": {Match: agentpolicy.PrefixMatcher("command")}}),
		agentkit.WithSession(agentsession.NewMemoryStore(), agentsession.Header{CWD: t.TempDir()}),
		agentkit.WithChildAgent(agentturn.Config{
			Name: "explore", Description: "delegate", Model: child, ModelName: "m",
			Tools: []agenttool.Tool{bash},
			BeforeToolCall: func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
				return kit.Engine().BeforeToolCall()(ctx, info)
			},
		}, agentkit.ShareGrantScope()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	ctx := agentpolicy.ContextWithGrantScope(t.Context(), "host")
	if granted, refused := kit.Engine().GrantSet(ctx, agentpolicy.RuleSet{
		Source: agentpolicy.Source{Name: "product", Trusted: true},
		Allow:  []agentpolicy.Rule{{Tool: "Bash", Spec: "git:*"}},
	}); len(granted) != 1 || len(refused) != 0 {
		t.Fatalf("the product's grant: granted %v, refused %v", granted, refused)
	}
	agent := agentturn.New(kit.Config())
	defer kit.Attach(agent)()
	if _, err := agent.Prompt(ctx, openresponses.UserText("go")); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 1 || childScope != "host" {
		t.Fatalf("the child ran %v under scope %q, want git status under the conversation's, host", ran, childScope)
	}
}
