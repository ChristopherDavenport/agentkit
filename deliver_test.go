package agentkit_test

import (
	"context"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// Control offers agentturn's delivery: a message that lands after the
// run's last drain, between its final turn and its end, starts a run
// once that run has ended, rather than waiting in a queue nothing
// drains until the next input, and the recorder writes it as a
// delivery before the agent takes it. (agentturn RFC 0001 Queues, #106)
func TestControlDeliversAMessageThatMissedTheRun(t *testing.T) {
	model := &scriptModel{turns: []func(*openresponses.Emitter) error{textTurn("one"), textTurn("two")}}
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "m"),
		agentkit.WithSession(agentsession.NewMemoryStore(), agentsession.Header{}),
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
	d, ok := any(ctl).(agentturn.Deliverer)
	if !ok {
		t.Fatal("Control is not an agentturn.Deliverer")
	}
	type result struct {
		joined bool
		end    *agentturn.RunEnd
		err    error
	}
	delivered := make(chan result, 1)
	var once bool
	defer agent.Subscribe(func(_ context.Context, ev agentturn.Event) error {
		// The first run's last turn has ended: it will not drain again.
		if _, ok := ev.(*agentturn.TurnEnd); ok && !once {
			once = true
			go func() {
				joined, end, err := d.Deliver(context.Background(), openresponses.UserText("and another thing"))
				delivered <- result{joined, end, err}
			}()
		}
		return nil
	})()
	if _, err := ctl.Prompt(t.Context(), openresponses.UserText("hello")); err != nil {
		t.Fatal(err)
	}
	r := <-delivered
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.joined {
		t.Fatal("the delivery joined a run past its last drain")
	}
	if r.end == nil || r.end.Reason != agentturn.ReasonDone || len(model.requests()) != 2 {
		t.Fatalf("delivery end = %+v, requests = %d; want a second run that answered it", r.end, len(model.requests()))
	}
	recorded := false
	for _, e := range kit.Session().Entries() {
		if q, ok := e.(*agentsession.QueuedEntry); ok {
			line, _ := agentsession.MarshalEntry(q)
			recorded = recorded || strings.Contains(string(line), `"deliver":true`)
		}
	}
	if !recorded {
		t.Error("the session records no delivery")
	}
}
