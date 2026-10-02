package agentkit_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// A child agent that reads no skill and runs a grandchild that does:
// the conversation's next message ends the grandchild's grant, reached
// through the child's scope though the child granted nothing.
func TestGrandchildGrantEndsWithTheConversationsMessage(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "release", "Bash(git:*)")
	sessions := agentsession.NewMemoryStore()
	var kit *agentkit.Kit
	read := agenttool.New("read_release", "read", func(ctx context.Context, _ agenttool.NoArgs) (string, error) {
		tool, ok := kit.LookupTool(agentskill.ToolName)
		if !ok {
			return "", errors.New("no skill tool")
		}
		_, err := tool.Execute(ctx, agenttool.Call{ID: "call-read", Args: json.RawMessage(`{"name":"release"}`)})
		return "", err
	})
	nest := agenttool.New("nest", "nest", func(ctx context.Context, _ agenttool.NoArgs) (string, error) {
		tool, ok := kit.LookupTool("explore")
		if !ok {
			return "", errors.New("no explore")
		}
		_, err := tool.Execute(ctx, agenttool.Call{ID: "call-nested", Args: json.RawMessage(`{"input":"x"}`)})
		return "", err
	})
	child := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurn("nest", `{}`), callTurn("read_release", `{}`)}}
	parent := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurnID("call-a", "explore", `{"input":"go"}`)}}
	var err error
	kit, err = agentkit.New(t.Context(),
		agentkit.WithModel(parent, "m"),
		agentkit.WithSkills(skills),
		agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{
			Read: []string{agentskill.ToolName, "read_release", "explore", "nest"},
		}), map[string]agentpolicy.ToolMatcher{"Bash": {Match: agentpolicy.PrefixMatcher("command")}}),
		agentkit.WithSkillGrants(trustedSkills),
		agentkit.WithSkillGrantScope(),
		agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}),
		agentkit.WithChildAgent(agentturn.Config{
			Name: "explore", Description: "delegate", Model: child, ModelName: "m",
			Tools: []agenttool.Tool{read, nest},
			BeforeToolCall: func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
				return kit.Engine().BeforeToolCall()(ctx, info)
			},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	agent := agentturn.New(kit.Config())
	defer kit.Attach(agent)()
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("go")); err != nil {
		t.Fatal(err)
	}
	if n := len(kit.Engine().Grants()); n != 1 {
		t.Fatalf("grants after the first message = %d, want the grandchild's", n)
	}
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("again")); err != nil {
		t.Fatal(err)
	}
	if n := len(kit.Engine().Grants()); n != 0 {
		t.Fatalf("grandchild grant survived the parent's next message: %d", n)
	}
}
