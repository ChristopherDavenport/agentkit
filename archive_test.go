package agentkit_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"
)

// An archive scope is written and searched by the memory tools and
// never rendered: no request's instructions hold a passage, the usage
// paragraph names the scope as searchable and not shown, memory_search
// finds a passage, and memory_insert adds one. WithMemory's scopes were
// both rendered and given to the tools, so an archive was rendered
// into every request until the budget dropped the block. (agentmemory
// RFC 0001 §6, §7.1, #107)
func TestAnArchiveScopeIsSearchedAndWrittenAndNeverRendered(t *testing.T) {
	store := memStore(t,
		agentmemory.Entry{Scope: "user", Name: "editor", Content: "uses vim"},
		agentmemory.Entry{Scope: "archive", Name: "passage-1", Content: "the launch code is swordfish"},
	)
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithMemory(store, "user"),
		agentkit.WithMemoryArchive("archive"),
		agentkit.WithMemoryTools(agentmemory.WithInsert("archive")),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	req := &openresponses.Request{Instructions: kit.Config().Instructions}
	if err := kit.Config().BeforeModelCall(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(req.Instructions, "swordfish") || !strings.Contains(req.Instructions, "uses vim") {
		t.Fatalf("instructions:\n%s\nwant the user scope rendered and the archive not", req.Instructions)
	}
	usage, ok := partByID(kit.Parts(), agentkit.PartMemoryUsage)
	if !ok || !strings.Contains(usage.Text, `"archive"`) {
		t.Fatalf("the usage paragraph does not name the archive: %q", usage.Text)
	}
	call := func(name, args string) string {
		t.Helper()
		tool, ok := kit.LookupTool(name)
		if !ok {
			t.Fatalf("%s is not offered", name)
		}
		res, err := tool.Execute(t.Context(), agenttool.Call{ID: "c-" + name, Args: json.RawMessage(args)})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out, _ := json.Marshal(res.Output)
		return string(out)
	}
	if out := call(agentmemory.SearchTool, `{"query":"swordfish"}`); !strings.Contains(out, "passage-1") {
		t.Fatalf("memory_search = %s, want the archived passage", out)
	}
	call(agentmemory.InsertTool, `{"scope":"archive","content":"the vault opens at noon"}`)
	entries, err := store.List(t.Context(), "archive")
	if err != nil || len(entries) != 2 {
		t.Fatalf("archive entries = %d, %v; want the insert kept", len(entries), err)
	}
}
