package agentkit_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsmd"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// A whole coding agent: a prompt, the repository's AGENTS.md chain, a
// skill catalogue, memory, a policy, some tools, an MCP server, a
// recorded session and compaction. Everything that can fail fails in
// New, so there is one error to handle.
func ExampleNew() {
	ctx := context.Background()
	cwd, _ := os.Getwd()
	home, _ := os.UserHomeDir()

	model := &openresponses.ClientAdapter{Client: openresponses.NewClient(
		"https://api.example.com/v1",
		openresponses.WithAPIKey(os.Getenv("API_KEY")),
	)}
	memory := agentmemory.NewMemStore()
	sessions := agentsession.NewMemoryStore()

	kit, err := agentkit.New(ctx,
		agentkit.WithModel(model, "gpt-5"),
		agentkit.WithInstructions("Be brief."),
		agentkit.WithAgentsMD(cwd, agentsmd.Options{Root: cwd}),
		agentkit.WithOptionalSkills(filepath.Join(home, ".dax", "skills")),
		agentkit.WithMemory(memory, "user", "project"),
		agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{
			Read:    []string{"read"},
			Edit:    []string{"edit"},
			Execute: []string{"bash"},
		}), map[string]agentpolicy.ToolMatcher{
			"bash": {Match: agentpolicy.PrefixMatcher("command")},
		}),
		agentkit.WithMCP("some-server --stdio"),
		agentkit.WithSession(sessions, agentsession.Header{CWD: cwd}),
		agentkit.WithCompaction(60_000),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer kit.Close()

	// Everything the layers left out of the prompt, in one list.
	for _, o := range kit.Omitted() {
		fmt.Printf("not given to the model: %s (%s)\n", o.What, o.Reason)
	}

	agent := agentturn.New(kit.Config())
	defer kit.Attach(agent)()

	end, err := agent.Prompt(ctx, openresponses.UserText("what does this repo do?"))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(end.Reason)
}

// The kit's instruction parts are the shape a session records, so they
// go straight into a config entry: a change to one layer is then
// recorded as a change to one part rather than as a new copy of the
// whole prompt.
func ExampleKit_Parts() {
	kit, err := agentkit.New(context.Background(),
		agentkit.WithModel(stubModel{}, "gpt-5"),
		agentkit.WithInstructions("Be brief."),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer kit.Close()

	for _, p := range kit.Parts() {
		fmt.Printf("%s (%s): %s\n", p.ID, p.Source, p.Text)
	}
	fmt.Println(agentsession.JoinInstructions(kit.Parts()) == kit.Config().Instructions)

	// Output:
	// product (product): Be brief.
	// true
}

// A product that outgrows one of the kit's decisions replaces that
// decision without leaving the kit. Here the order is the product's,
// and everything else stays the kit's.
func ExampleWithOrder() {
	kit, err := agentkit.New(context.Background(),
		agentkit.WithModel(stubModel{}, "gpt-5"),
		agentkit.WithInstructions("Be brief."),
		agentkit.WithMemory(agentmemory.NewMemStore(), "user"),
		agentkit.WithOrder(agentkit.PartMemory, agentkit.PartProduct),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer kit.Close()

	for _, p := range kit.Parts() {
		fmt.Println(p.ID)
	}

	// Output:
	// memory
	// memory/user
	// memory:summary
	// memory:usage
	// product
}

// An in-process child agent is a tool like any other. WithChildAgent
// offers one and, when a session is configured, records the child's own
// run into it: the observer that does the linking is the recorder the
// kit made, which is why the kit is the one that can bind it.
func ExampleWithChildAgent() {
	kit, err := agentkit.New(context.Background(),
		agentkit.WithModel(stubModel{}, "gpt-5"),
		agentkit.WithInstructions("Be brief."),
		agentkit.WithChildAgent(agentturn.Config{
			Name:         "explore",
			Description:  "Delegate a read-only investigation to a sub-agent.",
			Model:        stubModel{},
			ModelName:    "gpt-5",
			Instructions: "You are a read-only explorer. End with a written answer.",
			MaxTurns:     10,
		}),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer kit.Close()

	for _, t := range kit.Config().ResolveTools(context.Background()) {
		fmt.Println(t.Name())
	}

	// Output:
	// explore
}

// WithDeferredTools is the general form, for any tool that has to be
// built after New has opened the session and built the engine. Here a
// tool reports the session it is running in, which does not exist until
// New has opened it.
func ExampleWithDeferredTools() {
	kit, err := agentkit.New(context.Background(),
		agentkit.WithModel(stubModel{}, "gpt-5"),
		agentkit.WithSession(agentsession.NewMemoryStore(), agentsession.Header{CWD: "/tmp"}),
		agentkit.WithDeferredTools(func(k *agentkit.Kit) []agenttool.Tool {
			id := k.SessionID()
			return []agenttool.Tool{agenttool.New("session_id",
				"Report the session this conversation is recorded in.",
				func(context.Context, agenttool.NoArgs) (string, error) {
					return id, nil
				})}
		}),
	)
	if err != nil {
		log.Fatal(err)
	}
	defer kit.Close()

	for _, t := range kit.Config().ResolveTools(context.Background()) {
		fmt.Println(t.Name())
	}

	// Output:
	// session_id
}
