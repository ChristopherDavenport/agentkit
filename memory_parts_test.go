package agentkit_test

import (
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentmemory"
)

// agentmemory reserves the part IDs memory, memory/… and memory:… to
// the parts its renderer writes, and a product that records parts of
// its own beside them gives them IDs outside that space (agentmemory
// RFC 0001 §7.2). The usage paragraph is the kit's part, so its ID is
// outside it, and WithOrder still places it with the block.
func TestTheUsagePartIsOutsideAgentmemorysIDs(t *testing.T) {
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithInstructions("Be brief."),
		agentkit.WithMemory(memStore(t, agentmemory.Entry{Scope: "user", Name: "a", Content: "remember"}), "user"),
		agentkit.WithOrder(agentkit.PartMemory, agentkit.PartProduct),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	id := agentkit.PartMemoryUsage
	if id == "memory" || strings.HasPrefix(id, "memory/") || strings.HasPrefix(id, "memory:") {
		t.Errorf("PartMemoryUsage = %q, inside agentmemory's reserved IDs", id)
	}
	parts := kit.Parts()
	if n := len(parts); n < 2 || parts[n-2].ID != agentkit.PartMemoryUsage || parts[n-1].ID != agentkit.PartProduct {
		var ids []string
		for _, p := range parts {
			ids = append(ids, p.ID)
		}
		t.Fatalf("parts = %v, want the usage part last of the memory group, before the product's", ids)
	}
}
