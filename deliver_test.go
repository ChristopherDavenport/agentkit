package agentkit_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
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

// A kit that serves several conversations has no session of its own,
// and each conversation's recorder is on the context its front runs it
// with. Control.Deliver and Control.Queue write through that recorder,
// so the queued entry, a delivery's with deliver, is on the record
// before the agent takes the item: here the agent waits on an approval,
// so nothing else would write it until a run took the item. They fell
// back to the agent's own Deliver and Queue. (agentturn#244, #106)
func TestControlDeliversThroughTheConversationsRecorder(t *testing.T) {
	var ran atomic.Int32
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(&scriptModel{turns: []func(*openresponses.Emitter) error{callTurn("bash", `{}`)}}, "m"),
		agentkit.WithTools(counting("bash", &ran)),
		agentkit.WithPolicy(agentpolicy.Policy{Default: agentpolicy.Ask()}, nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	store := agentsession.NewMemoryStore()
	rec, sess, err := session.Start(t.Context(), store, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	agent := agentturn.New(kit.Config())
	defer rec.Attach(agent)()
	ctl, err := kit.Control(agent)
	if err != nil {
		t.Fatal(err)
	}
	ctx := agentkit.ContextWithRecorder(t.Context(), rec)
	end, err := ctl.Prompt(ctx, openresponses.UserText("run it"))
	if err != nil || end.Reason != agentturn.ReasonInputRequired {
		t.Fatalf("prompt: end %+v, err %v; want the call held", end, err)
	}
	if _, _, err := ctl.Deliver(ctx, openresponses.UserText("and then this")); !errors.Is(err, agentturn.ErrInputRequired) {
		t.Fatalf("Deliver = %v, want ErrInputRequired while the call waits", err)
	}
	if err := ctl.Queue(ctx, agentturn.QueueFollowUp, openresponses.UserText("and that")); err != nil {
		t.Fatal(err)
	}
	live, err := store.Open(t.Context(), sess.ID())
	if err != nil {
		t.Fatal(err)
	}
	var delivered, queued bool
	for _, e := range live.Entries() {
		if q, ok := e.(*agentsession.QueuedEntry); ok {
			line, _ := agentsession.MarshalEntry(q)
			delivered = delivered || strings.Contains(string(line), "and then this") && strings.Contains(string(line), `"deliver":true`)
			queued = queued || strings.Contains(string(line), "and that")
		}
	}
	if !delivered || !queued {
		t.Fatalf("the conversation's session records the delivery %v and the queued item %v; want both before a run takes them", delivered, queued)
	}
}
