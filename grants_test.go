package agentkit_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// skillWithTools writes a skill whose frontmatter carries an
// allowed-tools field.
func skillWithTools(t *testing.T, root, name, allowed string) string {
	t.Helper()
	writeFile(t, filepath.Join(root, name, "SKILL.md"),
		"---\nname: "+name+"\ndescription: what "+name+" is for\nallowed-tools: "+allowed+"\n---\n\ndo the thing\n")
	return root
}

func readSkill(t *testing.T, tool agenttool.Tool, name string) {
	t.Helper()
	args, err := json.Marshal(map[string]string{"name": name})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Execute(t.Context(), agenttool.Call{ID: "call-1", Args: args}); err != nil {
		t.Fatalf("reading the skill: %v", err)
	}
}

func skillTool(t *testing.T, kit *agentkit.Kit) agenttool.Tool {
	t.Helper()
	for _, tool := range kit.Config().ResolveTools(t.Context()) {
		if tool.Name() == agentskill.ToolName {
			return tool
		}
	}
	t.Fatal("the skill tool is not offered")
	return nil
}

func TestReadingATrustedSkillGrantsItsAllowedTools(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "digging", "Bash(git status:*)")

	var reports []agentkit.SkillGrant
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithSkills(skills),
		agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{Execute: []string{"Bash"}}),
			map[string]agentpolicy.ToolMatcher{
				"Bash": {Match: agentpolicy.PrefixMatcher("command")},
			}),
		agentkit.WithSkillGrants(func(sk *agentskill.Skill) agentpolicy.Source {
			return agentpolicy.Source{Name: "skill:" + sk.Name, Path: sk.Location, Trusted: true}
		}),
		agentkit.WithSkillGrantReport(func(g agentkit.SkillGrant) { reports = append(reports, g) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	if got := len(kit.Engine().Grants()); got != 0 {
		t.Fatalf("grants before the read = %d, want 0", got)
	}
	readSkill(t, skillTool(t, kit), "digging")

	if len(reports) != 1 {
		t.Fatalf("reports = %v, want one", reports)
	}
	if len(reports[0].Granted) != 1 || reports[0].Granted[0].Tool != "Bash" {
		t.Fatalf("granted = %v, want the skill's Bash rule", reports[0].Granted)
	}
	if got := len(kit.Engine().Grants()); got == 0 {
		t.Fatal("the engine holds no grant after the read")
	}

	// A second read grants again and says so: GrantSet under one
	// source replaces the set, so the engine still holds one.
	readSkill(t, skillTool(t, kit), "digging")
	if len(reports) != 2 {
		t.Fatalf("reports = %d after a second read, want 2", len(reports))
	}
	if got := len(kit.Engine().Grants()); got != 1 {
		t.Fatalf("grants after a second read = %d, want the one set", got)
	}
}

func TestAnUntrustedSkillGrantsNothing(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "digging", "Bash(git status:*)")

	var reports []agentkit.SkillGrant
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithSkills(skills),
		agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{Execute: []string{"Bash"}}),
			map[string]agentpolicy.ToolMatcher{
				"Bash": {Match: agentpolicy.PrefixMatcher("command")},
			}),
		// No source function: the default is untrusted, so the allow
		// rules are withheld.
		agentkit.WithSkillGrants(nil),
		agentkit.WithSkillGrantReport(func(g agentkit.SkillGrant) { reports = append(reports, g) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	readSkill(t, skillTool(t, kit), "digging")
	if len(reports) != 1 {
		t.Fatalf("reports = %v, want one", reports)
	}
	if len(reports[0].Granted) != 0 {
		t.Fatalf("granted = %v from an untrusted source, want none", reports[0].Granted)
	}
}

func TestWithoutSkillGrantsTheToolIsTheCatalogueTool(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "digging", "Bash(git status:*)")

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithSkills(skills),
		agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{Execute: []string{"Bash"}}), nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	readSkill(t, skillTool(t, kit), "digging")
	if got := len(kit.Engine().Grants()); got != 0 {
		t.Fatalf("grants = %d without WithSkillGrants, want 0", got)
	}
}

// agentskill refuses "Bash()" rather than widening it to every shell
// command. The kit inherits that refusal, and reports it before the run
// rather than at the first read.
func TestASkillWhoseAllowedToolsWillNotParseIsAnOmission(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "digging", "Bash()")

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithSkills(skills),
		agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{Execute: []string{"Bash"}}), nil),
		agentkit.WithSkillGrants(nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	var found bool
	for _, o := range kit.Omitted() {
		if strings.Contains(o.Reason, "allowed-tools") {
			found = true
		}
	}
	if !found {
		t.Fatalf("omissions = %v, want one naming the allowed-tools that will not parse", kit.Omitted())
	}
}

// The granting wrapper must be the catalogue's tool in every way the
// loop and a scheduler can observe, or wrapping it has changed what the
// model is offered and how the batch runs. agenttool.Wrap is what keeps
// it so; this pins that the kit uses it.
func TestTheGrantingWrapperIsTheSameToolToTheLoop(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "digging", "Bash(git status:*)")

	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithSkills(skills),
		agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{Execute: []string{"Bash"}}), nil),
		agentkit.WithSkillGrants(nil),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	cat, err := agentskill.DiscoverDirs(skills)
	if err != nil {
		t.Fatal(err)
	}
	bare, wrapped := cat.Tool(), skillTool(t, kit)

	if wrapped.Name() != bare.Name() || wrapped.Description() != bare.Description() {
		t.Errorf("name or description differ: %q/%q", wrapped.Name(), bare.Name())
	}
	if string(wrapped.Parameters()) != string(bare.Parameters()) {
		t.Errorf("parameters differ:\n%s\n%s", wrapped.Parameters(), bare.Parameters())
	}
	if agenttool.IsStrict(wrapped) != agenttool.IsStrict(bare) {
		t.Error("strictness differs")
	}
	if agenttool.IsSequential(wrapped) != agenttool.IsSequential(bare) {
		t.Error("sequentiality differs")
	}
	if agenttool.ResourceOf(wrapped) != agenttool.ResourceOf(bare) {
		t.Error("the resource differs")
	}
	if agenttool.AnnotationsOf(wrapped) != agenttool.AnnotationsOf(bare) {
		t.Error("the annotations differ")
	}
	// Confined is the fifth optional interface a Tool may implement.
	// The catalogue's tool does not implement it today, so this compares
	// two falses; it is here so that the day it does, the wrapper
	// dropping it fails the build rather than quietly telling a policy
	// the call claims no sandbox.
	gotOK, gotBy := agenttool.ConfinedBy(t.Context(), wrapped, nil)
	wantOK, wantBy := agenttool.ConfinedBy(t.Context(), bare, nil)
	if gotOK != wantOK || gotBy != wantBy {
		t.Errorf("confinement differs: %v/%q, want %v/%q", gotOK, gotBy, wantOK, wantBy)
	}

	gotDef, wantDef := agenttool.Definition(wrapped), agenttool.Definition(bare)
	got, err := json.Marshal(gotDef)
	if err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(wantDef)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("the function tool the model is offered differs:\n%s\n%s", got, want)
	}
}

// A revoke ends a grant, and the next read of the skill puts it back:
// the kit keeps no memory of what it granted that would swallow the
// second read.
func TestASkillReadAfterARevokeIsGrantedAgain(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "digging", "Bash(git status:*)")

	var reports []agentkit.SkillGrant
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithSkills(skills),
		agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{Execute: []string{"Bash"}}),
			map[string]agentpolicy.ToolMatcher{
				"Bash": {Match: agentpolicy.PrefixMatcher("command")},
			}),
		agentkit.WithSkillGrants(func(sk *agentskill.Skill) agentpolicy.Source {
			return agentpolicy.Source{Name: "skill:" + sk.Name, Path: sk.Location, Trusted: true}
		}),
		agentkit.WithSkillGrantReport(func(g agentkit.SkillGrant) { reports = append(reports, g) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	readSkill(t, skillTool(t, kit), "digging")
	if got := len(kit.Engine().Grants()); got != 1 {
		t.Fatalf("grants after the read = %d, want 1", got)
	}
	if n := kit.RevokeSkillGrants(t.Context()); n != 1 {
		t.Fatalf("RevokeSkillGrants removed %d rules, want the skill's one", n)
	}
	if got := len(kit.Engine().Grants()); got != 0 {
		t.Fatalf("grants after the revoke = %d, want 0", got)
	}
	readSkill(t, skillTool(t, kit), "digging")
	if got := len(kit.Engine().Grants()); got != 1 {
		t.Fatalf("grants after reading again = %d, want 1", got)
	}
	if len(reports) != 2 {
		t.Fatalf("reports = %d, want one per read", len(reports))
	}
}

// Under WithSkillGrantScope a grant lasts the run that read the skill:
// the next run starts without it. Without the option it outlives the
// run.
func TestSkillGrantScopeEndsAGrantWhenTheNextRunStarts(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "digging", "Bash(git status:*)")
		model := &scriptModel{turns: []func(*openresponses.Emitter) error{
			callTurn(agentskill.ToolName, `{"name":"digging"}`),
		}}
		opts := []agentkit.Option{
			agentkit.WithModel(model, "m"),
			agentkit.WithSkills(skills),
			agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{
				Read:    []string{agentskill.ToolName},
				Execute: []string{"Bash"},
			}), map[string]agentpolicy.ToolMatcher{
				"Bash": {Match: agentpolicy.PrefixMatcher("command")},
			}),
			agentkit.WithSkillGrants(func(sk *agentskill.Skill) agentpolicy.Source {
				return agentpolicy.Source{Name: "skill:" + sk.Name, Path: sk.Location, Trusted: true}
			}),
		}
		if scoped {
			opts = append(opts, agentkit.WithSkillGrantScope())
		}
		kit, err := agentkit.New(t.Context(), opts...)
		if err != nil {
			t.Fatal(err)
		}
		defer kit.Close()

		agent := agentturn.New(kit.Config())
		if _, err := agent.Prompt(t.Context(), openresponses.UserText("dig")); err != nil {
			t.Fatal(err)
		}
		if got := len(kit.Engine().Grants()); got != 1 {
			t.Fatalf("scoped=%v: grants after the run that read the skill = %d, want 1", scoped, got)
		}
		if _, err := agent.Prompt(t.Context(), openresponses.UserText("again")); err != nil {
			t.Fatal(err)
		}
		want := 1
		if scoped {
			want = 0
		}
		if got := len(kit.Engine().Grants()); got != want {
			t.Fatalf("scoped=%v: grants after the next run = %d, want %d", scoped, got, want)
		}
	}
}

// A Resume after an approval is the same task going on, not a new
// message, so the scope keeps the grant: every run's first turn is
// turn 1, a Resume's too, and revoking there would take the grant away
// in the middle of the task.
func TestSkillGrantScopeKeepsAGrantAcrossAResume(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "digging", "Bash(git status:*)")
	model := &scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn(agentskill.ToolName, `{"name":"digging"}`),
		callTurn("Bash", `{"command":"rm -rf x"}`),
	}}
	bash := agenttool.New("Bash", "run a command",
		func(context.Context, struct {
			Command string `json:"command"`
		}) (string, error) {
			return "", nil
		})
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "m"),
		agentkit.WithSkills(skills),
		agentkit.WithTools(bash),
		agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{
			Read:    []string{agentskill.ToolName},
			Execute: []string{"Bash"},
		}), map[string]agentpolicy.ToolMatcher{
			"Bash": {Match: agentpolicy.PrefixMatcher("command")},
		}),
		agentkit.WithSkillGrants(func(sk *agentskill.Skill) agentpolicy.Source {
			return agentpolicy.Source{Name: "skill:" + sk.Name, Trusted: true}
		}),
		agentkit.WithSkillGrantScope(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	agent := agentturn.New(kit.Config())
	end, err := agent.Prompt(t.Context(), openresponses.UserText("dig"))
	if err != nil {
		t.Fatal(err)
	}
	if end.Reason != agentturn.ReasonInputRequired || len(end.Pending) != 1 {
		t.Fatalf("reason = %q, pending = %d; want the rm held", end.Reason, len(end.Pending))
	}
	answers, err := kit.Engine().Release(t.Context(), end,
		agentturn.Approve(end.Pending[0].Call.CallID).WithBy(agentpolicy.ByHuman))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Resume(t.Context(), answers...); err != nil {
		t.Fatal(err)
	}
	if got := len(kit.Engine().Grants()); got != 1 {
		t.Fatalf("grants after the Resume = %d, want the skill's grant kept", got)
	}
}

// Under WithSkillGrantScope a user's message ends the grant however it
// arrives: as a follow-up the run takes after its answer, as a steer
// between turns, or with a developer note after it. The model's own
// output and an approval's outputs do not. (#24)
func TestSkillGrantScopeEndsAGrantAtAnyNewUserMessage(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "digging", "Bash(git status:*)")
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithSkills(skills),
		agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{Execute: []string{"Bash"}}),
			map[string]agentpolicy.ToolMatcher{
				"Bash": {Match: agentpolicy.PrefixMatcher("command")},
			}),
		agentkit.WithSkillGrants(func(sk *agentskill.Skill) agentpolicy.Source {
			return agentpolicy.Source{Name: "skill:" + sk.Name, Trusted: true}
		}),
		agentkit.WithSkillGrantScope(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	call := &openresponses.FunctionCall{CallID: "call-1", Name: "Bash", Arguments: `{"command":"git status"}`}
	output := openresponses.NewFunctionCallOutput("call-1", "clean")
	for _, tc := range []struct {
		name    string
		turn    int
		tail    agentturn.Transcript
		revoked bool
	}{
		{"a follow-up after the answer", 4, agentturn.Transcript{openresponses.AssistantText("done"), openresponses.UserText("also clean the build")}, true},
		{"a steer between turns", 2, agentturn.Transcript{call, output, openresponses.UserText("stop, use git clean")}, true},
		{"a prompt with a developer note after it", 1, agentturn.Transcript{openresponses.UserText("release"), openresponses.DeveloperText("cwd is /repo")}, true},
		{"a prompt with an item reference after it", 1, agentturn.Transcript{openresponses.UserText("release"), &openresponses.ItemReference{ID: "item-1"}}, true},
		{"a Resume's answered calls", 1, agentturn.Transcript{call, output}, false},
		{"the model's own turn", 3, agentturn.Transcript{call, output, openresponses.AssistantText("checking")}, false},
	} {
		readSkill(t, skillTool(t, kit), "digging")
		if len(kit.Engine().Grants()) != 1 {
			t.Fatalf("%s: the read granted nothing", tc.name)
		}
		tr := append(agentturn.Transcript{openresponses.UserText("dig")}, tc.tail...)
		if _, err := kit.Config().BeforeTurn(t.Context(), agentturn.TurnStartInfo{Turn: tc.turn, Transcript: tr}); err != nil {
			t.Fatal(err)
		}
		if revoked := len(kit.Engine().Grants()) == 0; revoked != tc.revoked {
			t.Errorf("%s: revoked = %v, want %v", tc.name, revoked, tc.revoked)
		}
	}
}

// A root skill and a qualified one of the same name are two grants and
// two reports: the default source keys on the listed name, so reading
// one does not replace the other's set. (#25)
func TestAQualifiedSkillIsAGrantOfItsOwn(t *testing.T) {
	root := t.TempDir()
	skillWithTools(t, filepath.Join(root, "skills"), "deploy", "Bash(kubectl:*)")
	skillWithTools(t, filepath.Join(root, "apps", "web", "skills"), "deploy", "Bash(npm:*)")
	top, err := agentskill.Dir(filepath.Join(root, "skills"))
	if err != nil {
		t.Fatal(err)
	}
	web, err := agentskill.Dir(filepath.Join(root, "apps", "web", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	web.Qualifier = "apps/web"

	var reports []string
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithSkillSources(top, web),
		agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{Execute: []string{"Bash"}}),
			map[string]agentpolicy.ToolMatcher{
				"Bash": {Match: agentpolicy.PrefixMatcher("command")},
			}),
		agentkit.WithSkillGrants(nil),
		agentkit.WithSkillGrantReport(func(g agentkit.SkillGrant) { reports = append(reports, g.Skill) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	tool := skillTool(t, kit)
	readSkill(t, tool, "deploy")
	readSkill(t, tool, "apps/web:deploy")

	if want := []string{"deploy", "apps/web:deploy"}; strings.Join(reports, ",") != strings.Join(want, ",") {
		t.Fatalf("reports name %v, want %v", reports, want)
	}
	sources := map[string]bool{}
	for _, r := range kit.Engine().Withheld() {
		sources[r.Source.Name] = true
	}
	if !sources["agentskill:deploy"] || !sources["agentskill:apps/web:deploy"] {
		t.Fatalf("withheld rules come from %v, want one source per skill", sources)
	}
}
