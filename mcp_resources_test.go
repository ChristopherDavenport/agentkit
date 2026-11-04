package agentkit_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agenttool/mcpclient"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// serveResources runs an MCP server with three resources and a URI
// template over an in-memory transport.
func serveResources(t *testing.T) sdk.Transport {
	t.Helper()
	srv := sdk.NewServer(&sdk.Implementation{Name: "docs", Version: "0"}, nil)
	read := func(_ context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: req.Params.URI, MIMEType: "text/plain", Text: "the text of " + req.Params.URI}}}, nil
	}
	for _, name := range []string{"a", "b", "c"} {
		srv.AddResource(&sdk.Resource{URI: "file:///" + name + ".txt", Name: name, MIMEType: "text/plain"}, read)
	}
	srv.AddResourceTemplate(&sdk.ResourceTemplate{URITemplate: "docs://pages/{page}", Name: "page", MIMEType: "text/plain"}, read)
	return serveServer(t, srv)
}

// Under WithMCPResourceTools each server that advertises resources is
// offered mcpclient's list_resources and read_resource beside its tools,
// under its prefix, so the model can list them and follow a
// resource_link; a read through a template returns the resource's text.
// Without the option they are not offered. (agenttool RFC 0001 MCP
// Resources, #115)
func TestResourceToolsAreOfferedForAServerWithResources(t *testing.T) {
	for _, on := range []bool{false, true} {
		opts := []agentkit.Option{
			agentkit.WithModel(stubModel{}, "m"),
			agentkit.WithMCPTransport(serveResources(t), mcpclient.WithPrefix("docs")),
			agentkit.WithMCPTransport(serveMCP(t, "plain")),
		}
		if on {
			opts = append(opts, agentkit.WithMCPResourceTools())
		}
		kit, err := agentkit.New(t.Context(), opts...)
		if err != nil {
			t.Fatal(err)
		}
		defer kit.Close()
		byName := map[string]agenttool.Tool{}
		for _, tool := range kit.Config().ResolveTools(t.Context()) {
			byName[tool.Name()] = tool
		}
		read, offered := byName["docs__read_resource"]
		_, listed := byName["docs__list_resources"]
		if !on {
			if offered || listed {
				t.Fatal("resource tools offered without WithMCPResourceTools")
			}
			continue
		}
		if !offered || !listed || len(byName) != 3 {
			names := make([]string, 0, len(byName))
			for n := range byName {
				names = append(names, n)
			}
			t.Fatalf("tools = %v, want the server's resource tools beside the plain server's tool", names)
		}
		var origin string
		for _, o := range kit.Tools() {
			if o.Name == "docs__read_resource" {
				origin = o.Source
			}
		}
		if !strings.HasPrefix(origin, "mcp:#1 ") {
			t.Errorf("Kit.Tools names read_resource's source %q, want the server's", origin)
		}
		args, _ := json.Marshal(map[string]string{"uri": "docs://pages/intro"})
		res, err := read.Execute(t.Context(), agenttool.Call{ID: "c1", Args: args})
		if err != nil {
			t.Fatal(err)
		}
		out, _ := json.Marshal(res.Output)
		if !strings.Contains(string(out), "the text of docs://pages/intro") {
			t.Fatalf("read through the template = %s", out)
		}
	}
}
