package agentkit_test

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// The replay of a session's skill grants reads a verdict's grant, source
// and scope members, as agentpolicy RFC 0001 (draft 0.4, The record)
// has a reader do, and parses the reason only for a record written
// before agentpolicy v0.0.19, which has none: a revocation is the one
// its members say, under its own scope, whatever its reason reads. (#80)
func TestTheReplayReadsAVerdictsMembers(t *testing.T) {
	root := t.TempDir()
	skills := skillWithTools(t, filepath.Join(root, "skills"), "release", "Bash(git:*)")
	sessions := agentsession.NewMemoryStore()
	opts := func(model agentturn.Model, sess agentkit.Option) []agentkit.Option {
		return []agentkit.Option{
			agentkit.WithModel(model, "m"),
			agentkit.WithSkills(skills),
			agentkit.WithPolicy(agentpolicy.FullAuto(agentpolicy.Tools{Read: []string{agentskill.ToolName}}),
				map[string]agentpolicy.ToolMatcher{"Bash": {Match: agentpolicy.PrefixMatcher("command")}}),
			agentkit.WithSkillGrants(trustedSkills),
			sess,
		}
	}
	// read reads the skill in a new session, then appends rec, a verdict
	// record written as a product or an older engine wrote it, and
	// returns the session.
	read := func(record func(scope string) map[string]any) string {
		t.Helper()
		kit, err := agentkit.New(t.Context(), opts(&scriptModel{turns: []func(*openresponses.Emitter) error{
			callTurn(agentskill.ToolName, `{"name":"release"}`),
		}}, agentkit.WithSession(sessions, agentsession.Header{CWD: root}))...)
		if err != nil {
			t.Fatal(err)
		}
		defer kit.Close()
		agent := agentturn.New(kit.Config())
		defer kit.Attach(agent)()
		if _, err := agent.Prompt(t.Context(), openresponses.UserText("cut the release")); err != nil {
			t.Fatal(err)
		}
		if record != nil {
			data, _ := json.Marshal(record(kit.GrantScope(t.Context())))
			if _, err := kit.Recorder().Annotate(t.Context(), agentpolicy.VerdictNS, json.RawMessage(data)); err != nil {
				t.Fatal(err)
			}
		}
		return kit.SessionID()
	}
	grantsAfterRestart := func(id string) int {
		t.Helper()
		kit, err := agentkit.New(t.Context(), opts(&scriptModel{}, agentkit.WithResumedSession(sessions, id))...)
		if err != nil {
			t.Fatal(err)
		}
		defer kit.Close()
		return len(kit.Engine().Grants())
	}

	cases := []struct {
		name   string
		record func(scope string) map[string]any
		want   int
	}{
		{"no revocation", nil, 1},
		{"a revocation its members name, whatever its reason reads", func(scope string) map[string]any {
			return map[string]any{"action": "block", "reason": "the operator ended it", "by": "policy", "grant": "revoke", "source": "skill:release", "scope": scope}
		}, 0},
		{"a revocation under another conversation's scope", func(string) map[string]any {
			return map[string]any{"action": "block", "reason": "revoked the rules granted by skill:release", "by": "policy", "grant": "revoke", "source": "skill:release", "scope": "another-conversation"}
		}, 1},
		{"a scope's revocation its members name", func(scope string) map[string]any {
			return map[string]any{"action": "block", "reason": "the conversation ended", "by": "policy", "grant": "revoke_scope", "scope": scope}
		}, 0},
		{"a revocation written before the members, by its reason", func(string) map[string]any {
			return map[string]any{"action": "block", "reason": "revoked the rules granted by skill:release", "by": "policy"}
		}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := grantsAfterRestart(read(tc.record)); got != tc.want {
				t.Fatalf("grants after the restart = %d, want %d", got, tc.want)
			}
		})
	}
}

// A front that scoped the conversation's grants itself, with
// agentpolicy.ContextWithGrantScope, finds them under that scope after a
// restart that names none: the session's records name the scope the
// grants were made under, and the replay makes them again there. It made
// them under the kit's own conversation, where the front's runs never
// look. (#80)
func TestAResumeRegrantsUnderTheScopeTheRecordsName(t *testing.T) {
	root := t.TempDir()
	skills := skillWithTools(t, filepath.Join(root, "skills"), "release", "Bash(git:*)")
	sessions := agentsession.NewMemoryStore()
	opts := func(model agentturn.Model, sess agentkit.Option) []agentkit.Option {
		return []agentkit.Option{
			agentkit.WithModel(model, "m"),
			agentkit.WithSkills(skills),
			agentkit.WithPolicy(agentpolicy.FullAuto(agentpolicy.Tools{Read: []string{agentskill.ToolName}}),
				map[string]agentpolicy.ToolMatcher{"Bash": {Match: agentpolicy.PrefixMatcher("command")}}),
			agentkit.WithSkillGrants(trustedSkills),
			sess,
		}
	}
	const front = "front:conversation-7"
	first, err := agentkit.New(t.Context(), opts(&scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn(agentskill.ToolName, `{"name":"release"}`),
	}}, agentkit.WithSession(sessions, agentsession.Header{CWD: root}))...)
	if err != nil {
		t.Fatal(err)
	}
	agent := agentturn.New(first.Config())
	unsubscribe := first.Attach(agent)
	if _, err := agent.Prompt(agentpolicy.ContextWithGrantScope(t.Context(), front), openresponses.UserText("cut the release")); err != nil {
		t.Fatal(err)
	}
	unsubscribe()
	_ = first.Close()

	second, err := agentkit.New(t.Context(), opts(&scriptModel{}, agentkit.WithResumedSession(sessions, first.SessionID()))...)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if got := len(second.Engine().GrantsFor(agentpolicy.ContextWithGrantScope(t.Context(), front))); got == 0 {
		t.Fatal("no grant under the front's scope after the restart")
	}
}
