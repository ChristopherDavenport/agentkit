package agentkit

import (
	"errors"
	"os/exec"
	"strings"
	"testing"

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
