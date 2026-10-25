package agentkit_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentskill"
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
	// fail, when set, fails the writes it returns an error for.
	fail func(agentsession.Entry) error

	mu      sync.Mutex
	cur     map[string]*agentsession.Session
	reopens int
}

func newReopenStore(at func(agentsession.Entry) bool) *reopenStore {
	return &reopenStore{MemoryStore: agentsession.NewMemoryStore(), at: at, cur: map[string]*agentsession.Session{}}
}

func (s *reopenStore) Open(ctx context.Context, id string) (*agentsession.Session, error) {
	// A cancelled context fails an Open, as cas's does.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
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
	if s.fail != nil {
		if err := s.fail(e); err != nil {
			return agentsession.Result{}, err
		}
	}
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

// reopenedKit is a kit recording into store, its agent prompted twice.
func reopenedKit(t *testing.T, store agentsession.Store) (*agentkit.Kit, *agentkit.Control, string) {
	t.Helper()
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
	var first string
	for _, e := range kit.Session().Entries() {
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
	return kit, ctl, first
}

// rewindEnd picks the run end a rebase into a run writes.
func rewindEnd(e agentsession.Entry) bool {
	r, ok := e.(*agentsession.RunEntry)
	return ok && r.IsEnd() && strings.HasPrefix(r.Ref, "rewind to ")
}

// A head move whose rebase the store reported with Reopen, and which
// fails after it, puts the head back on the session the store opened
// again, cancelled or not: the undo opens it past the cancel.
func TestControlContinueFromUndoesOnAReopenedSession(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		name := "failed"
		if cancelled {
			name = "cancelled"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			store := newReopenStore(rewindEnd)
			kit, ctl, first := reopenedKit(t, store)
			prevLeaf := kit.Session().Leaf()
			before := len(ctl.State().Transcript)
			refused := false
			store.fail = func(e agentsession.Entry) error {
				if l, ok := e.(*agentsession.LabelEntry); ok && l.Label != nil && *l.Label == agentsession.LeafLabel && !refused {
					refused = true
					if cancelled {
						cancel()
						return context.Canceled
					}
					return errors.New("store away")
				}
				return nil
			}
			if err := ctl.ContinueFrom(ctx, first); err == nil {
				t.Fatal("the move succeeded past a refused mark")
			}
			if store.reopens != 1 {
				t.Fatalf("reopens = %d, want the rewind's run end", store.reopens)
			}
			live := openSession(t, store, kit.SessionID())
			if live.Leaf() != prevLeaf {
				t.Fatalf("the store's leaf is %s after the undo, want %s", live.Leaf(), prevLeaf)
			}
			// What a reopen after a crash would find: the leaf the entries
			// record, not one moved in memory alone.
			read, err := store.Read(t.Context(), kit.SessionID())
			if err != nil {
				t.Fatal(err)
			}
			if read.Leaf() != prevLeaf {
				t.Fatalf("the recorded leaf is %s after the undo, want %s", read.Leaf(), prevLeaf)
			}
			if kit.Session() != live {
				t.Error("Session is not the session the store opened again")
			}
			if n := len(ctl.State().Transcript); n != before {
				t.Errorf("transcript has %d items after the undo, want %d", n, before)
			}
			if _, err := ctl.Prompt(t.Context(), openresponses.UserText("three")); err != nil {
				t.Fatal(err)
			}
			if err := live.VerifyRecords(live.Leaf()); err != nil {
				t.Errorf("verify records: %v", err)
			}
		})
	}
}

// A head move into a run has the rebase close that run, and when the
// store reports the end with Reopen, the move reads the branch, and
// marks the head, on the session the store opened again; Session is
// that session from then on.
func TestControlContinueFromFollowsAReopenedSession(t *testing.T) {
	store := newReopenStore(rewindEnd)
	kit, ctl, first := reopenedKit(t, store)
	opened := kit.Session()

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

// A Reopen the store reports for the leaf label a head move appends
// is the kit's to follow: Session is the session the store opened
// again.
func TestControlContinueFromFollowsAReopenOnItsMark(t *testing.T) {
	store := newReopenStore(func(e agentsession.Entry) bool {
		l, ok := e.(*agentsession.LabelEntry)
		return ok && l.Label != nil && *l.Label == agentsession.LeafLabel
	})
	kit, ctl, first := reopenedKit(t, store)
	if err := ctl.ContinueFrom(t.Context(), first); err != nil {
		t.Fatalf("continue from the first prompt: %v", err)
	}
	if store.reopens != 1 {
		t.Fatalf("reopens = %d, want the mark's", store.reopens)
	}
	if live := openSession(t, store, kit.SessionID()); kit.Session() != live {
		t.Error("Session is not the session the store opened again")
	}
}

// A head move whose context is cancelled before it starts moves
// nothing, on the record or in the agent.
func TestControlContinueFromCancelledBeforeItStartsMovesNothing(t *testing.T) {
	store := newReopenStore(rewindEnd)
	kit, ctl, first := reopenedKit(t, store)
	entries := len(kit.Session().Entries())
	before := len(ctl.State().Transcript)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := ctl.ContinueFrom(ctx, first); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if n := len(openSession(t, store, kit.SessionID()).Entries()); n != entries || store.reopens != 0 {
		t.Errorf("a cancelled move wrote %d entries, %d reopens", n-entries, store.reopens)
	}
	if n := len(ctl.State().Transcript); n != before {
		t.Errorf("transcript has %d items, want %d", n, before)
	}
}

// The revocation of a skill's grants before a head move appends its
// verdicts, and when the store reports one with Reopen the move
// rebases the session the store opened again rather than be refused.
func TestControlContinueFromRebasesAfterARevocationsReopen(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "digging", "danger")
	var ran atomic.Int32
	armed := false
	store := newReopenStore(func(e agentsession.Entry) bool {
		c, ok := e.(*agentsession.CustomEntry)
		if armed && ok && c.NS == agentpolicy.VerdictNS {
			armed = false
			return true
		}
		return false
	})
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(&scriptModel{turns: []func(*openresponses.Emitter) error{callTurn(agentskill.ToolName, `{"name":"digging"}`)}}, "test-model"),
		agentkit.WithTools(counting("danger", &ran)),
		agentkit.WithSkills(skills),
		agentkit.WithPolicy(agentpolicy.Policy{Allow: policyRules(t, "skill"), Default: agentpolicy.Ask()}, nil),
		agentkit.WithSkillGrants(func(sk *agentskill.Skill) agentpolicy.Source {
			return agentpolicy.Source{Name: "skill:" + sk.Name, Path: sk.Location, Trusted: true}
		}),
		agentkit.WithSkillGrantScope(),
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
	if _, err := ctl.Prompt(t.Context(), openresponses.UserText("learn")); err != nil {
		t.Fatal(err)
	}
	var user string
	for _, e := range kit.Session().Entries() {
		if it, ok := e.(*agentsession.ItemEntry); ok {
			if m, ok := it.Item.(*openresponses.Message); ok && m.Role == openresponses.RoleUser {
				user = e.Base().ID
				break
			}
		}
	}
	armed = true
	if err := ctl.ContinueFrom(t.Context(), user); err != nil {
		t.Fatalf("continue from the prompt: %v", err)
	}
	if store.reopens != 1 {
		t.Fatalf("reopens = %d, want the revocation's verdict", store.reopens)
	}
	live := openSession(t, store, kit.SessionID())
	if kit.Session() != live {
		t.Error("Session is not the session the store opened again")
	}
	if n := len(ctl.State().Transcript); n != 1 {
		t.Errorf("transcript has %d items after moving to the prompt, want the prompt alone", n)
	}
	if err := live.VerifyRecords(live.Leaf()); err != nil {
		t.Errorf("verify records: %v", err)
	}
}
