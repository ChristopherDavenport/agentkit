package agentkit_test

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// A front that scopes its conversations itself passes the scope to New
// on a restart, since the session does not record it: the resumed
// grants are made under that scope, the one the approval runs under, so
// the approved task keeps the skill's tools.
func TestResumedGrantsTakeTheFrontsScope(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "release", "Bash(git:*)")
	sessions := agentsession.NewMemoryStore()
	var ran []string
	bash := agenttool.New("Bash", "run a command",
		func(_ context.Context, in struct {
			Command string `json:"command"`
		}) (string, error) {
			ran = append(ran, in.Command)
			return "", nil
		})
	scoped := agentpolicy.ContextWithGrantScope(t.Context(), "tenant-1")
	build := func(model agentturn.Model, sess agentkit.Option) *agentkit.Kit {
		kit, err := agentkit.New(scoped,
			agentkit.WithModel(model, "m"),
			agentkit.WithSkills(skills),
			agentkit.WithTools(bash),
			agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{
				Read:    []string{agentskill.ToolName},
				Execute: []string{"Bash"},
			}), map[string]agentpolicy.ToolMatcher{
				"Bash": {Match: agentpolicy.PrefixMatcher("command")},
			}),
			agentkit.WithSkillGrants(func(sk *agentskill.Skill) agentpolicy.Source {
				return agentpolicy.Source{Name: "skill:" + sk.Name, Path: sk.Location, Trusted: true}
			}),
			agentkit.WithSkillGrantScope(),
			sess,
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = kit.Close() })
		return kit
	}

	first := build(&scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn(agentskill.ToolName, `{"name":"release"}`),
		callTurn("Bash", `{"command":"rm -rf build"}`),
	}}, agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}))
	agent := agentturn.New(first.Config(), first.AgentOptions()...)
	unsubscribe := first.Attach(agent)
	end, err := agent.Prompt(scoped, openresponses.UserText("cut the release"))
	unsubscribe()
	if err != nil {
		t.Fatal(err)
	}
	if end.Reason != agentturn.ReasonInputRequired {
		t.Fatalf("reason = %q, want rm -rf held", end.Reason)
	}

	// The restart: a new kit over the session, and the approval.
	second := build(&scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn("Bash", `{"command":"git status"}`),
	}}, agentkit.WithResumedSession(sessions, first.SessionID()))
	if got := len(second.Engine().Grants()); got != 1 {
		t.Fatalf("grants in force after the restart = %d, want the skill's", got)
	}
	resumed := agentturn.New(second.Config(), second.AgentOptions()...)
	defer second.Attach(resumed)()
	pending := resumed.State().Pending
	if len(pending) != 1 {
		t.Fatalf("pending = %+v, want the held rm", pending)
	}
	end, err = resumed.Resume(scoped, agentturn.Approve(pending[0].Call.CallID).WithBy(agentpolicy.ByHuman))
	if err != nil {
		t.Fatal(err)
	}
	if end.Reason == agentturn.ReasonInputRequired {
		t.Fatalf("git status was held after the restart; ran %v", ran)
	}
	if !slices.Equal(ran, []string{"rm -rf build", "git status"}) {
		t.Fatalf("ran %v, want the approved rm and then git status under the grant", ran)
	}
}
