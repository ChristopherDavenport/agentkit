package agentkit_test

import (
	"context"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// serveRich runs an MCP server with a tool, a resource and a prompt
// over an in-memory transport.
func serveRich(t *testing.T) sdk.Transport {
	t.Helper()
	srv := sdk.NewServer(&sdk.Implementation{Name: "rich", Version: "0"}, nil)
	sdk.AddTool(srv, &sdk.Tool{Name: "lookup", Description: "looks up"},
		func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{}, nil, nil
		})
	srv.AddResource(&sdk.Resource{URI: "file:///notes.txt", Name: "notes", MIMEType: "text/plain"},
		func(_ context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
			return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: req.Params.URI, MIMEType: "text/plain", Text: "remember the milk"}}}, nil
		})
	srv.AddPrompt(&sdk.Prompt{Name: "greet", Description: "says hello"},
		func(context.Context, *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
			return &sdk.GetPromptResult{Messages: []*sdk.PromptMessage{{Role: "user", Content: &sdk.TextContent{Text: "hello"}}}}, nil
		})
	return serveServer(t, srv)
}

// A kit product reaches the MCP servers the kit dialed, as a product
// that dialed them by hand holds them: by label, for the server's
// resources, prompts and the rest of what mcpclient offers beside its
// tools, New's servers and an added one alike, until RemoveMCP. (#113)
func TestTheKitsMCPServersAreReachableByLabel(t *testing.T) {
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithMCPTransport(serveRich(t)),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	added, err := kit.AddMCPTransport(t.Context(), serveMCP(t, "other"))
	if err != nil {
		t.Fatal(err)
	}

	servers := kit.MCPServers()
	if len(servers) != 2 || servers[1].Label != added {
		t.Fatalf("servers = %+v, want New's then the added one", servers)
	}
	remote, ok := kit.MCP(servers[0].Label)
	if !ok || remote != servers[0].Remote {
		t.Fatalf("MCP(%q) = %v, %v; want New's server", servers[0].Label, remote, ok)
	}
	resources, err := remote.Session().ListResources(t.Context(), nil)
	if err != nil || len(resources.Resources) != 1 || resources.Resources[0].URI != "file:///notes.txt" {
		t.Fatalf("resources = %+v, %v", resources, err)
	}
	prompts, err := remote.Session().ListPrompts(t.Context(), nil)
	if err != nil || len(prompts.Prompts) != 1 || prompts.Prompts[0].Name != "greet" {
		t.Fatalf("prompts = %+v, %v", prompts, err)
	}

	if err := kit.RemoveMCP(added); err != nil {
		t.Fatal(err)
	}
	if _, ok := kit.MCP(added); ok || len(kit.MCPServers()) != 1 {
		t.Fatal("a removed server is still listed")
	}
}
