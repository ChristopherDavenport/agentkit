module github.com/ChristopherDavenport/agentkit/examples/a2a

go 1.25.0

// The a2a packages are nested modules on a2aproject/a2a-go. They live
// here rather than in the root so that the root stays buildable
// without a gRPC stack; see the package doc.
require (
	github.com/ChristopherDavenport/agentkit v0.0.0
	github.com/ChristopherDavenport/agenttool v0.0.13
	github.com/ChristopherDavenport/agentturn v0.0.14
	github.com/ChristopherDavenport/agentturn/front/a2a v0.0.14
	github.com/ChristopherDavenport/agentturn/tools/a2a v0.0.14
	github.com/a2aproject/a2a-go v0.3.15
)

require (
	github.com/ChristopherDavenport/agentsession v0.0.18
	github.com/ChristopherDavenport/agentturn/session v0.0.14
	github.com/ChristopherDavenport/openresponses v0.0.12
)

require (
	github.com/ChristopherDavenport/agentmemory v0.0.8 // indirect
	github.com/ChristopherDavenport/agentpolicy v0.0.9 // indirect
	github.com/ChristopherDavenport/agentskill v0.0.9 // indirect
	github.com/ChristopherDavenport/agentsmd v0.0.2 // indirect
	github.com/ChristopherDavenport/agenttool/mcpclient v0.0.13 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/modelcontextprotocol/go-sdk v1.8.0 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/oauth2 v0.36.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	golang.org/x/time v0.15.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260706201446-f0a921348800 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260706201446-f0a921348800 // indirect
	google.golang.org/grpc v1.84.0 // indirect
	google.golang.org/protobuf v1.36.11 // indirect
)

// The example builds against the root in this checkout, not a release,
// so CI compiles it against the code it documents. A product copies
// RecordEach rather than requiring this module.
replace github.com/ChristopherDavenport/agentkit => ../..
