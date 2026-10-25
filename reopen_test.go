package agentkit_test

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// reopenStore is a store whose Write reports Reopen for the entries at
// picks, as a remote client's does when its session could not follow
// the server: the entry goes to a session rebuilt from what the store
// wrote, at the leaf of the session it held, as the client moves the
// server's head to its leaf before it appends; the store holds the
// rebuilt one from then on, and the session handed out before has
// neither the entry nor anything after it.
type reopenStore struct {
	*agentsession.MemoryStore
	at func(agentsession.Entry) bool

	mu      sync.Mutex
	cur     map[string]*agentsession.Session
	reopens int
}

func newReopenStore(at func(agentsession.Entry) bool) *reopenStore {
	return &reopenStore{MemoryStore: agentsession.NewMemoryStore(), at: at, cur: map[string]*agentsession.Session{}}
}

func (s *reopenStore) Open(ctx context.Context, id string) (*agentsession.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.cur[id]; ok {
		return sess, nil
	}
	return s.MemoryStore.Open(ctx, id)
}

func (s *reopenStore) Read(ctx context.Context, id string) (*agentsession.Session, error) {
	live, err := s.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	return rebuilt(live)
}

func (s *reopenStore) Append(ctx context.Context, id string, e agentsession.Entry) (string, error) {
	r, err := s.Write(ctx, id, e)
	return r.ID, err
}

func (s *reopenStore) Write(ctx context.Context, id string, e agentsession.Entry) (agentsession.Result, error) {
	live, err := s.Open(ctx, id)
	if err != nil {
		return agentsession.Result{}, err
	}
	if !s.at(e) {
		got, err := live.Append(e)
		return agentsession.Result{ID: got}, err
	}
	fresh, err := rebuilt(live)
	if err != nil {
		return agentsession.Result{}, err
	}
	if leaf := live.Leaf(); leaf == "" {
		fresh.ResetLeaf()
	} else if err := fresh.Branch(leaf); err != nil {
		return agentsession.Result{}, err
	}
	got, err := fresh.Append(e)
	if err != nil {
		return agentsession.Result{}, err
	}
	s.mu.Lock()
	s.cur[id] = fresh
	s.reopens++
	s.mu.Unlock()
	return agentsession.Result{ID: got, Reopen: true}, nil
}

// rebuilt is a copy of s read back from its encoding.
func rebuilt(s *agentsession.Session) (*agentsession.Session, error) {
	var buf bytes.Buffer
	if err := agentsession.Write(&buf, s); err != nil {
		return nil, err
	}
	return agentsession.Read(&buf)
}

// A head move into a run has the rebase close that run, and when the
// store reports the end with Reopen, the move reads the branch, and
// marks the head, on the session the store opened again; Session is
// that session from then on.
func TestControlContinueFromFollowsAReopenedSession(t *testing.T) {
	store := newReopenStore(func(e agentsession.Entry) bool {
		r, ok := e.(*agentsession.RunEntry)
		return ok && r.IsEnd() && strings.HasPrefix(r.Ref, "rewind to ")
	})
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(&scriptModel{}, "test-model"),
		agentkit.WithSession(store, agentsession.Header{Records: agentsession.AllRecords}),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { kit.Close() })
	agent := agentturn.New(kit.Config(), kit.AgentOptions()...)
	t.Cleanup(kit.Attach(agent))
	ctl, err := kit.Control(agent)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"one", "two"} {
		if _, err := ctl.Prompt(t.Context(), openresponses.UserText(text)); err != nil {
			t.Fatal(err)
		}
	}
	opened := kit.Session()
	var first string
	for _, e := range opened.Entries() {
		if it, ok := e.(*agentsession.ItemEntry); ok {
			if m, ok := it.Item.(*openresponses.Message); ok && m.Role == openresponses.RoleUser {
				first = e.Base().ID
				break
			}
		}
	}
	if first == "" {
		t.Fatal("no prompt on the record")
	}

	if err := ctl.ContinueFrom(t.Context(), first); err != nil {
		t.Fatalf("continue from the first prompt: %v", err)
	}
	if store.reopens != 1 {
		t.Fatalf("reopens = %d, want the rewind's run end", store.reopens)
	}
	live := openSession(t, store, kit.SessionID())
	if live == opened {
		t.Fatal("the store did not replace the session")
	}
	if kit.Session() != live {
		t.Error("Session is not the session the store opened again")
	}
	if n := len(ctl.State().Transcript); n != 1 {
		t.Errorf("transcript has %d items after moving to the first prompt, want the prompt alone", n)
	}
	leaf, _ := live.Entry(live.Leaf())
	end, ok := leaf.(*agentsession.RunEntry)
	if !ok || !end.IsEnd() || end.Ref != "rewind to "+first || end.Parent != first {
		t.Fatalf("the store's leaf = %+v, want the rewind's run end", leaf)
	}
	marked := false
	for _, e := range live.Entries() {
		if l, ok := e.(*agentsession.LabelEntry); ok && l.Label != nil && *l.Label == agentsession.LeafLabel && l.Target == end.ID {
			marked = true
		}
	}
	if !marked {
		t.Error("no leaf label marks the head where the rewind left it")
	}

	if _, err := ctl.Prompt(t.Context(), openresponses.UserText("three")); err != nil {
		t.Fatal(err)
	}
	if err := live.VerifyRecords(live.Leaf()); err != nil {
		t.Errorf("verify records: %v", err)
	}
}
