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
// A tool whose name is taken by one the kit offers now is an error, as
// at New, and the server is closed: this is when the product can still
// rename it, with [mcpclient.WithPrefix]. A server added after [Kit.Close],
// or to a kit built with no tool source, whose config has no
// ToolProvider to carry it, is an error too.
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

func (k *Kit) addMCP(ctx context.Context, d mcpDial) (string, error) {
	if k.tools == nil {
		return "", errors.New("agentkit: AddMCP on a kit with no tool source; its config has no ToolProvider to offer the server's tools through")
	}
	// Held across the dial, so servers added at once are numbered and
	// checked in turn.
	k.mcpMu.Lock()
	defer k.mcpMu.Unlock()
	if k.closed.Load() {
		return "", errors.New("agentkit: AddMCP after Close")
	}
	n := k.mcpNext
	remote, err := k.connect(ctx, d, n)
	if err != nil {
		return "", err
	}
	label := "mcp:" + d.label(n)
	src := k.remoteSource(label, remote)

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
		return "", errors.Join(append(errs, remote.Close())...)
	}

	k.tools.add(src)
	k.added = append(k.added, addedMCP{label: label, remote: remote})
	k.mcpNext++
	for _, o := range origins {
		if o.Source == label {
			k.origins = append(k.origins, o)
		}
	}
	return label, nil
}

// RemoveMCP closes a server [Kit.AddMCP] or [Kit.AddMCPTransport]
// connected, by the label it returned, and stops offering its tools
// from the next turn on. A call to one of them already dispatched fails
// with the connection. A label that names no added server, one New
// dialed among them, is an error.
func (k *Kit) RemoveMCP(label string) error {
	k.mcpMu.Lock()
	defer k.mcpMu.Unlock()
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
		return a.remote.Close()
	}
	return fmt.Errorf("agentkit: RemoveMCP: %q is not a server AddMCP connected", label)
}
