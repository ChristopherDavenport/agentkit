package agentkit_test

import (
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
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

// An omitted memory entry's Size is what agentsession means by an
// omitted part's size, the bytes the part would have added: its text,
// as agentmemory.EntryPart renders it, and its separator. Not the
// manifest's Bytes, the content's size, which is less. (agentmemory RFC
// 0001 §8.1)
func TestAnOmittedMemoryEntrysSizeIsThePartsBytes(t *testing.T) {
	const prompt = "Be brief."
	store := memStore(t, manyFacts("mine")...)
	budget := int64(len(prompt)+2*len(agentkit.Separator)+len(agentmemory.Usage())) + 20
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithInstructions(prompt),
		agentkit.WithMemory(store, "user"),
		agentkit.WithInstructionBudget(budget),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	n := 0
	for _, o := range kit.Omitted() {
		if o.Source != agentkit.SourceMemory {
			continue
		}
		n++
		name := strings.TrimPrefix(o.What, "memory/user/")
		e, err := store.Get(t.Context(), "user", name)
		if err != nil {
			t.Fatalf("omission %+v: %v", o, err)
		}
		want := int64(len(agentmemory.EntryPart(*e, store.MaxEntryBytes()).Text) + len(agentsession.PartSeparator))
		if o.Size != want {
			t.Errorf("omitted %s has size %d, want %d, the bytes its part would add (content %d)", o.What, o.Size, want, len(e.Content))
		}
	}
	if n == 0 {
		t.Fatal("no memory entry was omitted")
	}
}

// A block the budget drops over an empty store is recorded as dropped,
// agentmemory's LeftOut, so the manifest says the block was left out
// and a write composed from it is refused after a restart too: with no
// entry to list as omitted, the drop was the empty manifest of a block
// shown empty. (agentmemory RFC 0001 §7.6, #95)
func TestADroppedBlockOverAnEmptyStoreSaysSo(t *testing.T) {
	const prompt = "be brief"
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithInstructions(prompt),
		agentkit.WithMemory(agentmemory.NewMemStore(), "user"),
		agentkit.WithInstructionBudget(int64(len(prompt)+len(agentkit.Separator)+40)),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	if got := kit.Config().Instructions; got != prompt {
		t.Fatalf("instructions = %q, want the block dropped", got)
	}
	if man := kit.MemoryManifest(); !man.LeftOut || !man.Dropped() {
		t.Fatalf("manifest = %+v, want it to say the block was left out", man)
	}
}

// Each memory part carries the trust agentmemory gave it, in
// agentsession's form, so the session records an entry a model wrote
// as untrusted and the renderer's own lines as reading nothing.
// (agentmemory RFC 0001 §7.2, Trust)
func TestTheMemoryPartsCarryTheirTrust(t *testing.T) {
	sessions := agentsession.NewMemoryStore()
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(&scriptModel{}, "m"),
		agentkit.WithMemory(memStore(t, agentmemory.Entry{Scope: "user", Name: "a", Content: "remember"}), "user"),
		agentkit.WithSession(sessions, agentsession.Header{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	byID := map[string]agentkit.Part{}
	for _, p := range kit.Parts() {
		byID[p.ID] = p
	}
	entry, title := byID[agentmemory.PartID("user", "a")], byID[agentkit.PartMemory]
	if entry.Trust == nil || len(entry.Trust.Reads) != 1 || entry.Trust.Reads[0] != "untrusted" {
		t.Errorf("the entry part's trust = %+v, want untrusted", entry.Trust)
	}
	if title.Trust == nil || len(title.Trust.Reads) != 0 {
		t.Errorf("the title part's trust = %+v, want reading nothing", title.Trust)
	}

	agent := agentturn.New(kit.Config(), kit.AgentOptions()...)
	defer kit.Attach(agent)()
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("hi")); err != nil {
		t.Fatal(err)
	}
	recorded := false
	for _, e := range kit.Session().Entries() {
		line, err := agentsession.MarshalEntry(e)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(line), `"id":"memory/user/a"`) && strings.Contains(string(line), `"trust":{"reads":["untrusted"]}`) {
			recorded = true
		}
	}
	if !recorded {
		t.Error("no config entry records the entry part as untrusted")
	}
}
