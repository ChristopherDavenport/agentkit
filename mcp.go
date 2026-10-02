package agentkit

import (
	"context"
	"errors"
	"fmt"

	"github.com/ChristopherDavenport/agenttool/mcpclient"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// addedMCP is a server [Kit.AddMCP] connected after New.
type addedMCP struct {
	label  string
	remote *mcpclient.Remote
}

// AddMCP starts an MCP server from a command line and offers its tools
// from the next turn on, as [WithMCP] does at [New]: the same stderr
// handling, elicitation when [WithToolElicitor] is set, and the label
// "mcp:#<n> <program>", numbered after the servers already given. It
// returns that label, which [WithToolFilter], [WithToolWrap], a
// [Conflict] and [Kit.Tools] use, and which [Kit.RemoveMCP] takes.
//
// It is for a server the product connects once a session is under way:
// a command that adds one, a sign-in that makes one reachable. A turn
// already running keeps the tools it was offered.
//
// The dial runs off the kit's lock, since it may wait on that sign-in,
// so [Kit.Tools], [Kit.RemoveMCP] and [Kit.Close] answer meanwhile, and
// two servers added at once are dialed at once. Each takes its number
// before it dials, so the two are numbered apart. A server that is
// refused, or whose dial fails, gives its number back unless another
// was numbered meanwhile, so the labels are dense unless servers are
// added at once, when a refusal leaves a gap. The clash check runs when
// the dial ends, against what the kit offers then.
//
// A tool whose name is taken by one the kit offers now is an error, as
// at New, and the server is closed: this is when the product can still
// rename it, with [mcpclient.WithPrefix]. A server added after [Kit.Close],
// or to a kit built with no tool source, whose config has no
// ToolProvider to carry it, is an error too, and so is one whose dial
// Close ended: Close cancels a dial in flight, and a server that
// connected as the kit closed is closed again.
//
// By hand it is a ToolProvider over a list of remotes the product
// appends to under a lock; see docs/manual.md.
func (k *Kit) AddMCP(ctx context.Context, command string, opts ...mcpclient.Option) (string, error) {
	return k.addMCP(ctx, mcpDial{command: command, opts: opts})
}

// AddMCPTransport connects to an MCP server over t and offers its tools
// from the next turn on, as [WithMCPTransport] does at [New]. It is
// [Kit.AddMCP] for a transport the product built, an OAuth-protected
// server's among them.
func (k *Kit) AddMCPTransport(ctx context.Context, t sdk.Transport, opts ...mcpclient.Option) (string, error) {
	return k.addMCP(ctx, mcpDial{transport: t, opts: opts})
}

// errAddAfterClose is AddMCP's error on a kit that has closed, before
// the dial or during it.
var errAddAfterClose = errors.New("agentkit: AddMCP after Close")

func (k *Kit) addMCP(ctx context.Context, d mcpDial) (string, error) {
	if k.tools == nil {
		return "", errors.New("agentkit: AddMCP on a kit with no tool source; its config has no ToolProvider to offer the server's tools through")
	}
	// The number is taken under the lock and the dial made off it: a
	// dial may wait on a sign-in, and Kit.Tools, RemoveMCP and Close
	// must not wait with it.
	k.mcpMu.Lock()
	if k.closed.Load() {
		k.mcpMu.Unlock()
		return "", errAddAfterClose
	}
	n := k.mcpNext
	k.mcpNext++
	k.mcpMu.Unlock()

	// The dial is bounded by the caller's context and ended by Close.
	dctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(k.closeCtx, cancel)()
	remote, err := k.connect(dctx, d, n)
	if err != nil {
		k.mcpMu.Lock()
		k.giveBack(n)
		k.mcpMu.Unlock()
		if k.closeCtx.Err() != nil {
			return "", fmt.Errorf("%w: the kit closed while the server was being dialed: %w", errAddAfterClose, err)
		}
		return "", err
	}
	label := "mcp:" + d.label(n)
	src := k.remoteSource(label, remote)

	k.mcpMu.Lock()
	if k.closed.Load() {
		// Close took the added servers while this one was dialing, so
		// it is closed here, off the lock as the others were.
		k.mcpMu.Unlock()
		return "", errors.Join(fmt.Errorf("%w: the kit closed while the server was being dialed", errAddAfterClose), remote.Close())
	}
	// Checked against what the kit offers now, with the server in its
	// place, so a collision either way, with a tool before it or with a
	// provider's after it, is the product's to resolve now.
	_, origins, conflicts := k.tools.resolveSources(ctx, k.tools.with(src))
	var errs []error
	for _, c := range conflicts {
		if c.Kept == label || c.Dropped == label {
			errs = append(errs, c)
		}
	}
	if len(errs) > 0 {
		k.giveBack(n)
		k.mcpMu.Unlock()
		return "", errors.Join(append(errs, remote.Close())...)
	}
	k.tools.add(src)
	k.added = append(k.added, addedMCP{label: label, remote: remote})
	for _, o := range origins {
		if o.Source == label {
			k.origins = append(k.origins, o)
		}
	}
	k.mcpMu.Unlock()
	return label, nil
}

// giveBack returns the number a server that was not added took, when
// no server was numbered after it, so the next is numbered as if the
// refused one had never been. Under mcpMu.
func (k *Kit) giveBack(n int) {
	if k.mcpNext == n+1 {
		k.mcpNext = n
	}
}

// RemoveMCP closes a server [Kit.AddMCP] or [Kit.AddMCPTransport]
// connected, by the label it returned, and stops offering its tools
// from the next turn on. A label that names no added server, one New
// dialed among them, is an error.
//
// The server leaves the kit's lists at once and is closed after, off
// the lock, so [Kit.Tools] and the other MCP methods answer while
// RemoveMCP waits for the close. What the close waits for is
// mcpclient's (agenttool v0.0.14): with [WithToolElicitor] set, so the
// client offers elicitation, it ends every call in flight to the server
// with [mcpclient.ErrClosed] and waits, a few seconds at most, for the
// requests telling the server its open questions were cancelled;
// without it, the close waits for every call in flight to the server to
// finish, however long the server takes. A call already dispatched
// fails with the connection either way.
func (k *Kit) RemoveMCP(label string) error {
	k.mcpMu.Lock()
	for i, a := range k.added {
		if a.label != label {
			continue
		}
		k.tools.remove(label)
		k.added = append(k.added[:i:i], k.added[i+1:]...)
		kept := k.origins[:0:0]
		for _, o := range k.origins {
			if o.Source != label {
				kept = append(kept, o)
			}
		}
		k.origins = kept
		k.mcpMu.Unlock()
		return a.remote.Close()
	}
	k.mcpMu.Unlock()
	return fmt.Errorf("agentkit: RemoveMCP: %q is not a server AddMCP connected", label)
}
