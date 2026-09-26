// Package agentkit assembles the nine libraries into an
// [agentturn.Config].
//
// It owns three compositions no single library can own without
// importing its siblings: the order of the instruction parts, the order
// of the hooks that contest one [agentturn.Config] field, and the union
// of the tool sets. Everything else it hands the loop came from a
// library's exported constructor.
//
// The rule the package holds itself to is that [Kit.Config] returns a
// plain [agentturn.Config] whose every field a product could have set
// by hand, with the same values, by calling the same exported
// functions. There are no private seams, no wrapper types a caller
// cannot construct, and no behaviour that exists only when the kit
// assembled it. For each field the kit sets, docs/manual.md names the
// call a product would write instead.
//
//	kit, err := agentkit.New(ctx,
//		agentkit.WithModel(model, "gpt-5"),
//		agentkit.WithInstructions("Be brief."),
//		agentkit.WithAgentsMD(cwd, agentsmd.Options{Root: repoRoot}),
//		agentkit.WithSkills(".dex/skills"),
//		agentkit.WithMemory(store, "user", "project"),
//		agentkit.WithPolicy(policy, matchers),
//		agentkit.WithTools(read, write, edit, bash),
//		agentkit.WithMCP("some-server --stdio"),
//		agentkit.WithSession(sessions, agentsession.Header{CWD: cwd}),
//		agentkit.WithCompaction(60_000),
//	)
//	if err != nil {
//		return err
//	}
//	defer kit.Close()
//
//	for _, o := range kit.Omitted() {
//		log.Printf("not given to the model: %s (%s)", o.What, o.Reason)
//	}
//
//	agent := agentturn.New(kit.Config())
//	defer kit.Attach(agent)()
//
// [New] does the work that can fail: discovery, validation, opening the
// session, dialing MCP. [Kit.Config] is then pure and may be called per
// run. [Kit.Close] releases what New opened.
//
// [Kit.Attach] is the one step a config cannot carry: it subscribes the
// recorder to the agent and returns the unsubscribe, so the doubled
// call above subscribes now and unsubscribes when the scope ends. A
// session configured but never attached records nothing, and nothing
// reports that, so attach where the agent is built. It is a no-op
// without a session, so the line is the same either way.
package agentkit
