package agentkit_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// stoppedShort answers every request with a message its provider ended
// incomplete for reason: a refusal for content_filter, cut text for
// max_output_tokens.
type stoppedShort struct {
	reason openresponses.IncompleteReason
}

func (m stoppedShort) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	em := openresponses.NewEmitter(sink, openresponses.NewResponse(req))
	if err := em.Start(); err != nil {
		return err
	}
	w, err := em.Message(openresponses.PhaseFinalAnswer)
	if err != nil {
		return err
	}
	if m.reason == openresponses.IncompleteReasonContentFilter {
		err = w.Refusal("I can't help with that.")
	} else {
		err = w.Text("Once upon")
	}
	if err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return em.Incomplete(m.reason)
}

// A run the provider stopped reaches the Control's caller with its
// cause, the provider's details and the refusal, as agentturn ended it:
// the kit passes the end through and drops none of it.
func TestAProviderStopReachesTheControlsCaller(t *testing.T) {
	for _, c := range []struct {
		reason openresponses.IncompleteReason
		cause  agentturn.StopCause
		refuse string
	}{
		{openresponses.IncompleteReasonContentFilter, agentturn.StopContentFilter, "I can't help with that."},
		{openresponses.IncompleteReasonMaxOutputTokens, agentturn.StopMaxOutputTokens, ""},
	} {
		t.Run(string(c.reason), func(t *testing.T) {
			_, ctl, _ := controlled(t, agentkit.WithModel(stoppedShort{reason: c.reason}, "m"))
			end, err := ctl.Prompt(t.Context(), openresponses.UserText("hi"))
			if err != nil {
				t.Fatal(err)
			}
			if end.Reason != agentturn.ReasonStopped || end.Cause != c.cause {
				t.Errorf("end = %s/%s, want stopped/%s", end.Reason, end.Cause, c.cause)
			}
			if end.Incomplete == nil || end.Incomplete.Reason != c.reason {
				t.Errorf("incomplete = %+v, want reason %s", end.Incomplete, c.reason)
			}
			if end.Refusal != c.refuse {
				t.Errorf("refusal = %q, want %q", end.Refusal, c.refuse)
			}
		})
	}
}

// A child agent the kit offers, recorded in the kit's session, fails
// its call with the refusal when its provider's content filter stopped
// it, and marks an answer cut at the output limit: the parent's model
// reads why, through the kit's wrap of the child's tool.
func TestAChildsProviderStopReachesTheParentsModel(t *testing.T) {
	for _, c := range []struct {
		reason openresponses.IncompleteReason
		want   string
	}{
		{openresponses.IncompleteReasonContentFilter, `was refused by its model's provider (content_filter): I can't help with that.`},
		{openresponses.IncompleteReasonMaxOutputTokens, `answer was cut short at its model's output limit (max_output_tokens)`},
	} {
		t.Run(string(c.reason), func(t *testing.T) {
			parent := &scriptModel{turns: []func(*openresponses.Emitter) error{
				callTurn("explore", `{"input":"look"}`),
			}}
			kit, err := agentkit.New(t.Context(),
				agentkit.WithModel(parent, "m"),
				agentkit.WithChildAgent(agentturn.Config{
					Name:        "explore",
					Description: "Delegate a read-only investigation.",
					Model:       stoppedShort{reason: c.reason},
					ModelName:   "m",
				}),
				agentkit.WithSession(agentsession.NewMemoryStore(), agentsession.Header{}),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer kit.Close()
			agent := agentturn.New(kit.Config(), kit.AgentOptions()...)
			defer kit.Attach(agent)()
			if _, err := agent.Prompt(t.Context(), openresponses.UserText("go")); err != nil {
				t.Fatal(err)
			}
			reqs := parent.requests()
			if len(reqs) != 2 {
				t.Fatalf("parent requests = %d, want 2", len(reqs))
			}
			input, err := json.Marshal(reqs[1].Input)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(input), c.want) {
				t.Errorf("the parent's model was not told %q:\n%s", c.want, input)
			}
		})
	}
}
