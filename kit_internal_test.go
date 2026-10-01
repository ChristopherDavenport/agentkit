package agentkit

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// WithMCPTransport's label names what the transport reaches, so two
// servers dialed over transports the product built are told apart in a
// Conflict and in Kit.Tools, and arguments, a URL's user, path and
// query, where credentials are put, stay out of it. (#16)
func TestATransportIsLabelledByWhatItReaches(t *testing.T) {
	for _, tc := range []struct {
		transport sdk.Transport
		want      string
	}{
		{&sdk.CommandTransport{Command: exec.Command("some-server", "--api-key=secret")}, "#1 some-server"},
		{&sdk.StreamableClientTransport{Endpoint: "https://me:secret@mcp.example.com/s/key123/mcp?token=abc#x"}, "#1 https://mcp.example.com"},
		{&sdk.SSEClientTransport{Endpoint: "http://localhost:8080/sse?key=k"}, "#1 http://localhost:8080"},
		{&sdk.InMemoryTransport{}, "#1 *mcp.InMemoryTransport"},
	} {
		if got := (mcpDial{transport: tc.transport}).label(0); got != tc.want {
			t.Errorf("label = %q, want %q", got, tc.want)
		}
	}
}

// failingWriter fails every write, as a closed log file would.
type failingWriter struct{ n int }

func (w *failingWriter) Write([]byte) (int, error) {
	w.n++
	return 0, errors.New("closed")
}

// A server's stderr reaches the writer and its tail is kept for New's
// error, and a writer that fails does not stop the copy, which would
// leave the server blocked on a full pipe. (#16)
func TestAServersStderrIsPassedOnAndItsTailKept(t *testing.T) {
	w := &failingWriter{}
	tail := &stderrTail{w: w}
	long := strings.Repeat("x", stderrTailBytes)
	for _, line := range []string{long, "\n401: token expired\n"} {
		if n, err := tail.Write([]byte(line)); err != nil || n != len(line) {
			t.Fatalf("Write = %d, %v; want every byte taken", n, err)
		}
	}
	if w.n != 2 {
		t.Fatalf("the writer saw %d writes, want 2", w.n)
	}
	got := tail.String()
	if !strings.HasSuffix(got, "401: token expired") || len(got) > stderrTailBytes {
		t.Fatalf("tail = %d bytes ending %q, want at most %d ending with the last line", len(got), got[max(0, len(got)-30):], stderrTailBytes)
	}
	if (*stderrTail)(nil).String() != "" {
		t.Fatal("a nil tail is not empty")
	}
}

// A held save after a restart is based on the render its run was shown,
// folded from the path. A render of another run between the call's run
// start and the call may be the last record, so the fold refuses rather
// than base the save on it. (#56)
func TestFoldAtCallRefusesARenderThatMayBeAnotherRuns(t *testing.T) {
	record := func(id, who string) agentsession.Entry {
		_, data := agentmemory.Manifest{Entries: []agentmemory.ManifestEntry{{Scope: "user", Name: who}}}.Record()
		return &agentsession.CustomEntry{EntryBase: agentsession.EntryBase{ID: id}, NS: agentmemory.ManifestNS, Data: data}
	}
	start := func(run string) agentsession.Entry {
		return &agentsession.RunEntry{RunID: run, Phase: agentsession.RunStart}
	}
	end := func(run string) agentsession.Entry {
		return &agentsession.RunEntry{RunID: run, Phase: agentsession.RunEnd}
	}
	call := &agentsession.ItemEntry{Item: &openresponses.FunctionCall{Name: agentmemory.SaveTool, CallID: "save"}}
	for _, tc := range []struct {
		name string
		path []agentsession.Entry
		want string // the entry the save is based on, "" for refused
	}{
		{"one run", []agentsession.Entry{start("a"), record("m1", "a"), call}, "a"},
		{"a run before it", []agentsession.Entry{start("b"), record("m0", "b"), end("b"), start("a"), record("m1", "a"), call}, "a"},
		{"no render of its own", []agentsession.Entry{start("b"), record("m0", "b"), end("b"), start("a"), call}, "b"},
		{"another run open", []agentsession.Entry{start("a"), record("m1", "a"), start("b"), record("m2", "b"), call}, ""},
		{"another run in between", []agentsession.Entry{start("a"), record("m1", "a"), start("b"), record("m2", "b"), end("b"), call}, ""},
		{"no run entries", []agentsession.Entry{record("m1", "a"), call}, "a"},
	} {
		got, ok := foldAtCall(tc.path, "save")
		switch {
		case tc.want == "" && ok:
			t.Errorf("%s: based on %+v, want refused", tc.name, got)
		case tc.want != "" && (!ok || len(got.Entries) != 1 || got.Entries[0].Name != tc.want):
			t.Errorf("%s: based on %+v (%v), want %s's render", tc.name, got, ok, tc.want)
		}
	}
}
