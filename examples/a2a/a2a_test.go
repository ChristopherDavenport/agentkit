package a2a

import (
	"context"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	fronta2a "github.com/ChristopherDavenport/agentturn/front/a2a"
	"github.com/ChristopherDavenport/openresponses/echo"
	"github.com/a2aproject/a2a-go/a2a"
	"github.com/a2aproject/a2a-go/a2asrv"
	"github.com/a2aproject/a2a-go/a2asrv/eventqueue"
)

// A conversation's session is released when its task ends, so another
// store over the same root, the agentsession CLI or a second replica,
// can open it, and the conversation's next message still resumes it.
// (#37)
func TestRecordEachReleasesEachConversation(t *testing.T) {
	root := t.TempDir()
	store, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	kit, err := agentkit.New(t.Context(), agentkit.WithModel(&echo.Adapter{}, "echo"))
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	_, exec := Serve(t.Context(), kit, "https://me.example/a2a", "1.0", RecordEach(kit, store, root), fronta2a.WithConversationStore(&fronta2a.MemoryStore{}))

	queues := eventqueue.NewInMemoryManager()
	send := func(task, conv string) {
		t.Helper()
		q, err := queues.GetOrCreate(context.Background(), a2a.TaskID(task))
		if err != nil {
			t.Fatal(err)
		}
		msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.TextPart{Text: "hello from " + task})
		req := &a2asrv.RequestContext{Message: msg, TaskID: a2a.TaskID(task), ContextID: conv}
		if err := exec.Execute(t.Context(), req, q); err != nil {
			t.Fatal(err)
		}
	}
	send("t1", "conv-a")
	send("t2", "conv-a")
	send("t3", "conv-b")

	other, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	n := 0
	for sum, err := range store.List(t.Context(), agentsession.ListFilter{}) {
		if err != nil {
			t.Fatal(err)
		}
		n++
		s, err := other.Open(t.Context(), sum.Header.ID)
		if err != nil {
			t.Errorf("a second store cannot open session %s after its tasks ended: %v", sum.Header.ID, err)
			continue
		}
		if runs := len(s.Entries()); runs == 0 {
			t.Errorf("session %s is empty", sum.Header.ID)
		}
	}
	if n != 2 {
		t.Fatalf("sessions = %d, want one per conversation", n)
	}
}
