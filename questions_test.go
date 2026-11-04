package agentkit_test

import (
	"context"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// A question put as an event is answered by a person, so the answer
// the asking tool gets names human (agenttool.Answer.By) when the reply
// names nobody, with a session and without one, and keeps the decider a
// reply names. (agenttool RFC 0001 Questions)
func TestQuestionsForNamesAPersonsAnswer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session bool
		replyBy string
		want    string
	}{
		{name: "no session", want: agenttool.ByHuman},
		{name: "a session", session: true, want: agenttool.ByHuman},
		{name: "a reply naming the policy", session: true, replyBy: agenttool.ByPolicy, want: agenttool.ByPolicy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got agenttool.Answer
			confirm := agenttool.New("confirm", "asks the user", func(ctx context.Context, _ noArgs) (string, error) {
				ask, ok := agenttool.ElicitorFrom(ctx)
				if !ok {
					return "nobody to ask", nil
				}
				ans, err := ask(ctx, agenttool.Elicitation{Message: "go ahead?"})
				if err != nil {
					return "", err
				}
				got = ans
				return "asked", nil
			})
			opts := []agentkit.Option{
				agentkit.WithModel(&scriptModel{turns: []func(*openresponses.Emitter) error{callTurn("confirm", `{}`), textTurn("done")}}, "m"),
				agentkit.WithTools(confirm),
				agentkit.WithQuestionEvents(),
			}
			if tc.session {
				opts = append(opts, agentkit.WithSession(agentsession.NewMemoryStore(), agentsession.Header{}))
			}
			kit, err := agentkit.New(t.Context(), opts...)
			if err != nil {
				t.Fatal(err)
			}
			defer kit.Close()
			agent := agentturn.New(kit.Config(), kit.AgentOptions()...)
			defer kit.Attach(agent)()
			defer agent.Subscribe(func(ctx context.Context, ev agentturn.Event) error {
				if q, ok := ev.(*agentturn.Question); ok {
					return agent.Reply(ctx, q.ID, agenttool.Answer{Action: agenttool.ActionAccept, By: tc.replyBy})
				}
				return nil
			})()
			ctx := agentkit.ContextWithQuestions(t.Context(), kit.QuestionsFor(agent))
			if _, err := agent.Prompt(ctx, openresponses.UserText("confirm it")); err != nil {
				t.Fatal(err)
			}
			if got.Action != agenttool.ActionAccept || got.By != tc.want {
				t.Fatalf("the tool's answer = %+v, want accepted by %q", got, tc.want)
			}
		})
	}
}
