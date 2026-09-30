package agentkit

import (
	"fmt"
	"testing"

	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
)

// The bounded map keeps the last renderedRuns keys put, whatever mix of
// repeats and deletes came before, and drops the oldest first.
func TestBoundedKeepsTheNewestKeys(t *testing.T) {
	var b bounded[int]
	for i := range 5 * renderedRuns {
		k := fmt.Sprint(i % (2 * renderedRuns))
		b.put(k, i)
		if i%3 == 0 {
			b.delete(fmt.Sprint((i + 7) % (2 * renderedRuns)))
		}
		if len(b.m) > renderedRuns {
			t.Fatalf("after %d puts the map holds %d keys", i+1, len(b.m))
		}
		if v, ok := b.get(k); !ok || v != i {
			t.Fatalf("the key just put is %v, %v", v, ok)
		}
	}
	if len(b.order) > 2*renderedRuns {
		t.Fatalf("order holds %d marks", len(b.order))
	}
	var fresh bounded[int]
	for i := range renderedRuns + 1 {
		fresh.put(fmt.Sprint(i), i)
	}
	if _, ok := fresh.get("0"); ok {
		t.Fatal("the oldest key was kept past the bound")
	}
	if _, ok := fresh.get("1"); !ok {
		t.Fatal("a key within the bound was dropped")
	}
}

// Two conversations a kit serves both have a memory_save call_0, as a
// provider that numbers its calls gives them. Each keeps the render of
// its own run: keyed by call ID alone, the first kept stood for both.
func TestTwoConversationsCallsOfOneIDKeepTheirOwnBase(t *testing.T) {
	k := &Kit{memory: true}
	observe := k.observeVerdicts(&settings{}, false)
	for _, conv := range []string{"conv-a", "conv-b"} {
		ctx := session.ContextWithSessionID(agentturn.ContextWithRunID(t.Context(), "run-"+conv), conv)
		k.mu.Lock()
		k.rendered.put("run-"+conv, agentmemory.Manifest{Entries: []agentmemory.ManifestEntry{{Scope: "user", Name: conv}}})
		k.mu.Unlock()
		observe(ctx, agentpolicy.Verdict{Tool: agentmemory.SaveTool, CallID: "call_0", RunID: "run-" + conv})
	}
	for _, conv := range []string{"conv-a", "conv-b"} {
		ctx := session.ContextWithSessionID(t.Context(), conv)
		m, ok := k.saveBase.get(saveKey(ctx, "call_0"))
		if !ok || len(m.Entries) != 1 || m.Entries[0].Name != conv {
			t.Errorf("%s's call_0 is based on %+v, want its own render", conv, m)
		}
	}
}
