package agentkit_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentpolicy"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
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

// A restart between a held call and its approval keeps the grant the
// skill read before it: the kit over the resumed session grants the
// skill again at New, so the approval's Resume goes on under it, as it
// does without the restart. (#35)
func TestASkillGrantSurvivesARestartBeforeTheApproval(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "release", "Bash(git:*)")
	sessions := agentsession.NewMemoryStore()
	var ran []string
	bash := agenttool.New("Bash", "run a command",
		func(_ context.Context, in struct {
			Command string `json:"command"`
		}) (string, error) {
			ran = append(ran, in.Command)
			return "", nil
		})
	build := func(model agentturn.Model, sess agentkit.Option) *agentkit.Kit {
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
				return agentpolicy.Source{Name: "skill:" + sk.Name, Path: sk.Location, Trusted: true}
			}),
			agentkit.WithSkillGrantScope(),
			sess,
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = kit.Close() })
		return kit
	}

	first := build(&scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn(agentskill.ToolName, `{"name":"release"}`),
		callTurn("Bash", `{"command":"rm -rf build"}`),
	}}, agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}))
	agent := agentturn.New(first.Config(), first.AgentOptions()...)
	unsubscribe := first.Attach(agent)
	end, err := agent.Prompt(t.Context(), openresponses.UserText("cut the release"))
	unsubscribe()
	if err != nil {
		t.Fatal(err)
	}
	if end.Reason != agentturn.ReasonInputRequired {
		t.Fatalf("reason = %q, want rm -rf held", end.Reason)
	}

	// The restart: a new kit over the session, and the approval.
	second := build(&scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn("Bash", `{"command":"git status"}`),
	}}, agentkit.WithResumedSession(sessions, first.SessionID()))
	if got := len(second.Engine().Grants()); got != 1 {
		t.Fatalf("grants in force after the restart = %d, want the skill's", got)
	}
	resumed := agentturn.New(second.Config(), second.AgentOptions()...)
	defer second.Attach(resumed)()
	pending := resumed.State().Pending
	if len(pending) != 1 {
		t.Fatalf("pending = %+v, want the held rm", pending)
	}
	end, err = resumed.Resume(t.Context(), agentturn.Approve(pending[0].Call.CallID).WithBy(agentpolicy.ByHuman))
	if err != nil {
		t.Fatal(err)
	}
	if end.Reason == agentturn.ReasonInputRequired {
		t.Fatalf("git status was held after the restart; ran %v", ran)
	}
	if !slices.Equal(ran, []string{"rm -rf build", "git status"}) {
		t.Fatalf("ran %v, want the approved rm and then git status under the grant", ran)
	}
}

// Under the scope, a read before the path's last user message is not
// granted again at New: that message revoked it.
func TestARestartDoesNotRegrantAReadTheScopeRevoked(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "release", "Bash(git:*)")
	sessions := agentsession.NewMemoryStore()
	for _, scoped := range []bool{true, false} {
		opts := func(model agentturn.Model, sess agentkit.Option) []agentkit.Option {
			o := []agentkit.Option{
				agentkit.WithModel(model, "m"),
				agentkit.WithSkills(skills),
				agentkit.WithPolicy(agentpolicy.FullAuto(agentpolicy.Tools{Read: []string{agentskill.ToolName}}),
					map[string]agentpolicy.ToolMatcher{"Bash": {Match: agentpolicy.PrefixMatcher("command")}}),
				agentkit.WithSkillGrants(func(sk *agentskill.Skill) agentpolicy.Source {
					return agentpolicy.Source{Name: "skill:" + sk.Name, Trusted: true}
				}),
				sess,
			}
			if scoped {
				o = append(o, agentkit.WithSkillGrantScope())
			}
			return o
		}
		first, err := agentkit.New(t.Context(), opts(&scriptModel{turns: []func(*openresponses.Emitter) error{
			callTurn(agentskill.ToolName, `{"name":"release"}`),
		}}, agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}))...)
		if err != nil {
			t.Fatal(err)
		}
		agent := agentturn.New(first.Config(), first.AgentOptions()...)
		unsubscribe := first.Attach(agent)
		for _, msg := range []string{"cut the release", "thanks"} {
			if _, err := agent.Prompt(t.Context(), openresponses.UserText(msg)); err != nil {
				t.Fatal(err)
			}
		}
		unsubscribe()
		_ = first.Close()

		second, err := agentkit.New(t.Context(), opts(&scriptModel{}, agentkit.WithResumedSession(sessions, first.SessionID()))...)
		if err != nil {
			t.Fatal(err)
		}
		want := 1
		if scoped {
			want = 0
		}
		if got := len(second.Engine().Grants()); got != want {
			t.Errorf("scoped=%v: grants after the restart = %d, want %d", scoped, got, want)
		}
		_ = second.Close()
	}
}

// A restart does not bring back a grant the product revoked, nor grant
// a skill the model never read that now holds the name it read.
func TestARestartRegrantsOnlyWhatTheJournalLeftInForce(t *testing.T) {
	root := t.TempDir()
	repo := skillWithTools(t, filepath.Join(root, "repo"), "release", "Bash(git:*)")
	other := skillWithTools(t, filepath.Join(root, "other"), "release", "Bash(rm:*)")
	sessions := agentsession.NewMemoryStore()
	opts := func(model agentturn.Model, sess agentkit.Option, dirs ...string) []agentkit.Option {
		return []agentkit.Option{
			agentkit.WithModel(model, "m"),
			agentkit.WithSkills(dirs...),
			agentkit.WithPolicy(agentpolicy.FullAuto(agentpolicy.Tools{Read: []string{agentskill.ToolName}}),
				map[string]agentpolicy.ToolMatcher{"Bash": {Match: agentpolicy.PrefixMatcher("command")}}),
			agentkit.WithSkillGrants(func(sk *agentskill.Skill) agentpolicy.Source {
				return agentpolicy.Source{Name: "skill:" + sk.Name, Trusted: true}
			}),
			sess,
		}
	}
	readOnce := func(revoke bool) string {
		t.Helper()
		kit, err := agentkit.New(t.Context(), opts(&scriptModel{turns: []func(*openresponses.Emitter) error{
			callTurn(agentskill.ToolName, `{"name":"release"}`),
		}}, agentkit.WithSession(sessions, agentsession.Header{CWD: root}), repo)...)
		if err != nil {
			t.Fatal(err)
		}
		defer kit.Close()
		agent := agentturn.New(kit.Config())
		defer kit.Attach(agent)()
		if _, err := agent.Prompt(t.Context(), openresponses.UserText("cut the release")); err != nil {
			t.Fatal(err)
		}
		if revoke && kit.RevokeSkillGrants(t.Context()) == 0 {
			t.Fatal("the revoke removed nothing")
		}
		return kit.SessionID()
	}
	grantsAfterRestart := func(id string, dirs ...string) int {
		t.Helper()
		kit, err := agentkit.New(t.Context(), opts(&scriptModel{}, agentkit.WithResumedSession(sessions, id), dirs...)...)
		if err != nil {
			t.Fatal(err)
		}
		defer kit.Close()
		return len(kit.Engine().Grants())
	}

	if got := grantsAfterRestart(readOnce(false), repo); got != 1 {
		t.Errorf("a read left in force: grants after the restart = %d, want 1", got)
	}
	if got := grantsAfterRestart(readOnce(true), repo); got != 0 {
		t.Errorf("a read the product revoked: grants after the restart = %d, want 0", got)
	}
	if got := grantsAfterRestart(readOnce(false), other, repo); got != 0 {
		t.Errorf("the name now held by a skill the model never read: grants after the restart = %d, want 0", got)
	}
}

// A read under the default untrusted source grants a set that holds no
// rules, and the engine records no revocation of it. The kit records
// one, so a restart after the product revoked, with the skill now
// trusted, does not grant the read's rules.
func TestARevokeOfASetWithNoRulesSurvivesARestart(t *testing.T) {
	root := t.TempDir()
	skills := skillWithTools(t, filepath.Join(root, "skills"), "release", "Bash(git:*)")
	sessions := agentsession.NewMemoryStore()
	build := func(model agentturn.Model, sess agentkit.Option, source func(*agentskill.Skill) agentpolicy.Source) *agentkit.Kit {
		kit, err := agentkit.New(t.Context(),
			agentkit.WithModel(model, "m"),
			agentkit.WithSkills(skills),
			agentkit.WithPolicy(agentpolicy.FullAuto(agentpolicy.Tools{Read: []string{agentskill.ToolName}}),
				map[string]agentpolicy.ToolMatcher{"Bash": {Match: agentpolicy.PrefixMatcher("command")}}),
			agentkit.WithSkillGrants(source),
			sess,
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = kit.Close() })
		return kit
	}
	first := build(&scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn(agentskill.ToolName, `{"name":"release"}`),
	}}, agentkit.WithSession(sessions, agentsession.Header{CWD: root}), nil)
	agent := agentturn.New(first.Config())
	unsubscribe := first.Attach(agent)
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("cut the release")); err != nil {
		t.Fatal(err)
	}
	first.RevokeSkillGrants(t.Context())
	unsubscribe()

	trusted := func(sk *agentskill.Skill) agentpolicy.Source {
		return agentpolicy.Source{Name: "agentskill:" + sk.ListedName(), Path: sk.Location, Trusted: true}
	}
	second := build(&scriptModel{}, agentkit.WithResumedSession(sessions, first.SessionID()), trusted)
	for _, g := range second.Engine().Grants() {
		if len(g.Allow) > 0 {
			t.Fatalf("a restart granted %v for a read the product revoked", g.Allow)
		}
	}
}

// grantedVerdicts counts the verdicts on the session's path that say a
// rule was granted.
func grantedVerdicts(s *agentsession.Session) int {
	n := 0
	for _, c := range customEntries(s, agentpolicy.VerdictNS) {
		if strings.Contains(string(c.Data), `"reason":"granted `) {
			n++
		}
	}
	return n
}

// One kit serving two conversations, each its own session: a skill read
// in the first grants it there, and the first call the kit decides in the
// second revokes that grant before deciding, so the second conversation
// runs nothing under it; a read in the second grants nothing and says
// why. The revocation is written to the first conversation's session.
// (#44)
func TestASkillGrantDoesNotReachAnotherConversation(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "release", "Bash(git:*)")
	sessions := agentsession.NewMemoryStore()
	var ran []string
	bash := agenttool.New("Bash", "run a command",
		func(_ context.Context, in struct {
			Command string `json:"command"`
		}) (string, error) {
			ran = append(ran, in.Command)
			return "", nil
		})
	var reports []agentkit.SkillGrant
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithSkills(skills),
		agentkit.WithTools(bash),
		agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{
			Read:    []string{agentskill.ToolName},
			Execute: []string{"Bash"},
		}), map[string]agentpolicy.ToolMatcher{
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

	// Each conversation as RecordEach serves it: its own session and
	// recorder, on the run's context.
	serve := func(turns ...func(*openresponses.Emitter) error) (*session.Recorder, *agentturn.RunEnd) {
		t.Helper()
		rec, _, err := session.Start(t.Context(), sessions, agentsession.Header{CWD: t.TempDir()}, session.WithInstructionsParts(kit.PartsFor))
		if err != nil {
			t.Fatal(err)
		}
		cfg := kit.Config()
		cfg.Model = &scriptModel{turns: turns}
		cfg.ToolRecorder = rec.RecordFunc()
		agent := agentturn.New(cfg)
		defer rec.Attach(agent)()
		ctx := agentkit.ContextWithRecorder(session.ContextWithSessionID(t.Context(), rec.SessionID()), rec)
		end, err := agent.Prompt(ctx, openresponses.UserText("go"))
		if err != nil {
			t.Fatal(err)
		}
		return rec, end
	}

	alice, end := serve(callTurn(agentskill.ToolName, `{"name":"release"}`), callTurn("Bash", `{"command":"git status"}`))
	if end.Reason == agentturn.ReasonInputRequired || !slices.Equal(ran, []string{"git status"}) {
		t.Fatalf("in the conversation that read the skill git status was not run under the grant: %q, ran %v", end.Reason, ran)
	}
	bob, end := serve(callTurn("Bash", `{"command":"git clean -fdx"}`))
	if end.Reason != agentturn.ReasonInputRequired || len(ran) != 1 {
		t.Fatalf("in another conversation git clean ran under the first one's grant: %q, ran %v", end.Reason, ran)
	}
	if g := kit.Engine().Grants(); len(g) != 0 {
		t.Fatalf("grants after a second conversation = %+v, want none", g)
	}
	revoked := func(rec *session.Recorder) int {
		n := 0
		for _, c := range customEntries(openSession(t, sessions, rec.SessionID()), agentpolicy.VerdictNS) {
			if strings.Contains(string(c.Data), "revoked the rules granted by skill:release") {
				n++
			}
		}
		return n
	}
	if a, b := revoked(alice), revoked(bob); a != 1 || b != 0 {
		t.Fatalf("revocations recorded: %d in the owner's session, %d in the other's; want 1 and 0", a, b)
	}

	reports = nil
	serve(callTurn(agentskill.ToolName, `{"name":"release"}`))
	if len(reports) != 1 || !errors.Is(reports[0].Err, agentkit.ErrSkillGrantConversation) || len(kit.Engine().Grants()) != 0 {
		t.Fatalf("a read in another conversation reported %+v and left %d grants, want ErrSkillGrantConversation and none", reports, len(kit.Engine().Grants()))
	}
}

// A restart grants again only what the read was granted and the skill
// still allows: a skill whose allowed-tools were widened before the
// restart gets nothing it did not have, and one whose instructions
// changed is not the skill the model read and gets nothing. (#45)
func TestARestartNeverWidensAGrant(t *testing.T) {
	for name, edit := range map[string]string{
		"widened allowed-tools": "---\nname: deploy\ndescription: what deploy is for\nallowed-tools: Bash(kubectl:*)\n---\n\ndo the thing\n",
		"changed instructions":  "---\nname: deploy\ndescription: what deploy is for\nallowed-tools: Bash(kubectl get:*)\n---\n\ndo another thing\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "skills")
			skills := skillWithTools(t, root, "deploy", "Bash(kubectl get:*)")
			sessions := agentsession.NewMemoryStore()
			build := func(model agentturn.Model, sess agentkit.Option) *agentkit.Kit {
				kit, err := agentkit.New(t.Context(),
					agentkit.WithModel(model, "m"),
					agentkit.WithSkills(skills),
					agentkit.WithPolicy(agentpolicy.FullAuto(agentpolicy.Tools{Read: []string{agentskill.ToolName}}),
						map[string]agentpolicy.ToolMatcher{"Bash": {Match: agentpolicy.PrefixMatcher("command")}}),
					agentkit.WithSkillGrants(func(sk *agentskill.Skill) agentpolicy.Source {
						return agentpolicy.Source{Name: "skill:" + sk.Name, Trusted: true}
					}),
					sess,
				)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = kit.Close() })
				return kit
			}
			first := build(&scriptModel{turns: []func(*openresponses.Emitter) error{
				callTurn(agentskill.ToolName, `{"name":"deploy"}`),
			}}, agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}))
			agent := agentturn.New(first.Config())
			unsubscribe := first.Attach(agent)
			if _, err := agent.Prompt(t.Context(), openresponses.UserText("deploy")); err != nil {
				t.Fatal(err)
			}
			unsubscribe()

			writeFile(t, filepath.Join(root, "deploy", "SKILL.md"), edit)
			second := build(&scriptModel{}, agentkit.WithResumedSession(sessions, first.SessionID()))
			for _, g := range second.Engine().Grants() {
				if len(g.Allow) > 0 {
					t.Fatalf("a restart granted %v for a read of Bash(kubectl get:*)", g.Allow)
				}
			}
		})
	}
}

// A restart grants again silently: no verdict is recorded for what the
// path already says was granted, and the report is marked Replayed.
// (#46)
func TestARestartRegrantsSilently(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "deploy", "Bash(git:*) Bash(make:*) Bash(ls:*)")
	sessions := agentsession.NewMemoryStore()
	var reports []agentkit.SkillGrant
	build := func(model agentturn.Model, sess agentkit.Option) *agentkit.Kit {
		kit, err := agentkit.New(t.Context(),
			agentkit.WithModel(model, "m"),
			agentkit.WithSkills(skills),
			agentkit.WithPolicy(agentpolicy.FullAuto(agentpolicy.Tools{Read: []string{agentskill.ToolName}}),
				map[string]agentpolicy.ToolMatcher{"Bash": {Match: agentpolicy.PrefixMatcher("command")}}),
			agentkit.WithSkillGrants(func(sk *agentskill.Skill) agentpolicy.Source {
				return agentpolicy.Source{Name: "skill:" + sk.Name, Trusted: true}
			}),
			agentkit.WithSkillGrantReport(func(g agentkit.SkillGrant) { reports = append(reports, g) }),
			sess,
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = kit.Close() })
		return kit
	}
	first := build(&scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn(agentskill.ToolName, `{"name":"deploy"}`),
	}}, agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}))
	agent := agentturn.New(first.Config())
	unsubscribe := first.Attach(agent)
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("deploy")); err != nil {
		t.Fatal(err)
	}
	unsubscribe()
	before := grantedVerdicts(first.Session())
	if before != 3 || len(reports) != 1 || reports[0].Replayed {
		t.Fatalf("the read recorded %d grant verdicts and reported %+v, want 3 and one report of the read", before, reports)
	}

	for i := range 5 {
		reports = nil
		kit := build(&scriptModel{}, agentkit.WithResumedSession(sessions, first.SessionID()))
		if got := len(kit.Engine().Grants()[0].Allow); got != 3 {
			t.Fatalf("restart %d granted %d rules, want 3", i, got)
		}
		if got := grantedVerdicts(openSession(t, sessions, first.SessionID())); got != before {
			t.Fatalf("after restart %d the session holds %d grant verdicts, want the read's %d", i, got, before)
		}
		if len(reports) != 1 || !reports[0].Replayed || len(reports[0].Granted) != 3 {
			t.Fatalf("restart %d reported %+v, want one report marked Replayed", i, reports)
		}
		_ = kit.Close()
	}
}

// trustedSkills is the source function the tests below grant under.
func trustedSkills(sk *agentskill.Skill) agentpolicy.Source {
	return agentpolicy.Source{Name: "skill:" + sk.Name, Path: sk.Location, Trusted: true}
}

// The engine records a grant as it took it, after WithAliases expanded
// it, so a skill's Bash(git:*) is on the record as bash(git:*). The
// replay granted only what matched the skill's own spelling, which under
// an alias is nothing, and a restart silently lost the grant: the task's
// next git call was asked about. (#57)
func TestARestartUnderAnAliasRegrantsWhatTheReadWasGranted(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "release", "Bash(git:*)")
	sessions := agentsession.NewMemoryStore()
	var ran []string
	bash := agenttool.New("bash", "run a command",
		func(_ context.Context, in struct {
			Command string `json:"command"`
		}) (string, error) {
			ran = append(ran, in.Command)
			return "", nil
		})
	var reports []agentkit.SkillGrant
	build := func(model agentturn.Model, sess agentkit.Option) *agentkit.Kit {
		kit, err := agentkit.New(t.Context(),
			agentkit.WithModel(model, "m"),
			agentkit.WithSkills(skills),
			agentkit.WithTools(bash),
			agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{
				Read:    []string{agentskill.ToolName},
				Execute: []string{"bash"},
			}), map[string]agentpolicy.ToolMatcher{
				"bash": {Match: agentpolicy.PrefixMatcher("command")},
			}, agentpolicy.WithAliases(map[string][]string{"Bash": {"bash"}})),
			agentkit.WithSkillGrants(trustedSkills),
			agentkit.WithSkillGrantReport(func(g agentkit.SkillGrant) { reports = append(reports, g) }),
			sess,
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = kit.Close() })
		return kit
	}

	first := build(&scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn(agentskill.ToolName, `{"name":"release"}`),
		callTurn("bash", `{"command":"rm -rf build"}`),
	}}, agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}))
	agent := agentturn.New(first.Config(), first.AgentOptions()...)
	unsubscribe := first.Attach(agent)
	end, err := agent.Prompt(t.Context(), openresponses.UserText("cut the release"))
	unsubscribe()
	if err != nil {
		t.Fatal(err)
	}
	if end.Reason != agentturn.ReasonInputRequired {
		t.Fatalf("reason = %q, want rm -rf held", end.Reason)
	}

	reports = nil
	second := build(&scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn("bash", `{"command":"git status"}`),
	}}, agentkit.WithResumedSession(sessions, first.SessionID()))
	if len(reports) != 1 || !reports[0].Replayed || len(reports[0].Granted) != 1 || reports[0].Granted[0].String() != "bash(git:*)" {
		t.Fatalf("the replay reported %+v, want bash(git:*) granted again", reports)
	}
	resumed := agentturn.New(second.Config(), second.AgentOptions()...)
	defer second.Attach(resumed)()
	pending := resumed.State().Pending
	if len(pending) != 1 {
		t.Fatalf("pending = %+v, want the held rm", pending)
	}
	end, err = resumed.Resume(t.Context(), agentturn.Approve(pending[0].Call.CallID).WithBy(agentpolicy.ByHuman))
	if err != nil {
		t.Fatal(err)
	}
	if end.Reason == agentturn.ReasonInputRequired || !slices.Equal(ran, []string{"rm -rf build", "git status"}) {
		t.Fatalf("after the restart git status was not run under the grant: %q, ran %v", end.Reason, ran)
	}
}

// The default source names the digest of the frontmatter the rules were
// parsed from, so a verdict about the grant says what it was built
// from, and a restart does not grant again a read whose skill's
// frontmatter has changed, although its instructions read the same.
// (#63)
func TestAGrantNamesTheFrontmatterItWasBuiltFrom(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "deploy", "Bash(kubectl:*)")
	var reports []agentkit.SkillGrant
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithSkills(skills),
		agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{Execute: []string{"Bash"}}),
			map[string]agentpolicy.ToolMatcher{"Bash": {Match: agentpolicy.PrefixMatcher("command")}}),
		agentkit.WithSkillGrants(nil),
		agentkit.WithSkillGrantReport(func(g agentkit.SkillGrant) { reports = append(reports, g) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	sk, _ := kit.Catalog().Lookup("deploy")
	readSkill(t, skillTool(t, kit), "deploy")
	var hash string
	for _, set := range kit.Engine().Grants() {
		if set.Source.Name == "agentskill:deploy" {
			hash = set.Source.Hash
		}
	}
	if hash == "" || hash != sk.FrontmatterSHA256() {
		t.Fatalf("the grant's source hash = %q, want the frontmatter's %q", hash, sk.FrontmatterSHA256())
	}

	// The frontmatter rewritten on disk after discovery: the read serves
	// the new body under the frontmatter as loaded, and says so.
	writeFile(t, sk.Location, "---\nname: deploy\ndescription: what deploy is for\nallowed-tools: Bash(*)\n---\n\ndo the thing\n")
	reports = nil
	readSkill(t, skillTool(t, kit), "deploy")
	if len(reports) != 1 || !reports[0].FrontmatterChanged {
		t.Fatalf("a read after the frontmatter changed reported %+v, want FrontmatterChanged", reports)
	}
}

// A front that records each conversation itself and names it with
// session.ContextWithSessionID, as front/a2a's WithRecorderFor example
// does, without ContextWithRecorder, is several conversations to the
// kit: one conversation's read grants nothing to another. It was one
// conversation, and Alice's skill let Bob's model run git clean
// unasked. (#58)
func TestConversationsNamedOnlyByTheirSessionIDAreApart(t *testing.T) {
	for _, own := range []bool{false, true} {
		skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "release", "Bash(git:*)")
		sessions := agentsession.NewMemoryStore()
		var ran []string
		bash := agenttool.New("Bash", "run a command",
			func(_ context.Context, in struct {
				Command string `json:"command"`
			}) (string, error) {
				ran = append(ran, in.Command)
				return "", nil
			})
		opts := []agentkit.Option{
			agentkit.WithModel(stubModel{}, "m"),
			agentkit.WithSkills(skills),
			agentkit.WithTools(bash),
			agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{
				Read:    []string{agentskill.ToolName},
				Execute: []string{"Bash"},
			}), map[string]agentpolicy.ToolMatcher{
				"Bash": {Match: agentpolicy.PrefixMatcher("command")},
			}),
			agentkit.WithSkillGrants(trustedSkills),
		}
		if own {
			opts = append(opts, agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}))
		}
		kit, err := agentkit.New(t.Context(), opts...)
		if err != nil {
			t.Fatal(err)
		}
		defer kit.Close()

		serve := func(turns ...func(*openresponses.Emitter) error) *agentturn.RunEnd {
			t.Helper()
			rec, _, err := session.Start(t.Context(), sessions, agentsession.Header{CWD: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			cfg := kit.Config()
			cfg.Model = &scriptModel{turns: turns}
			cfg.ToolRecorder = rec.RecordFunc()
			agent := agentturn.New(cfg)
			defer rec.Attach(agent)()
			end, err := agent.Prompt(session.ContextWithSessionID(t.Context(), rec.SessionID()), openresponses.UserText("go"))
			if err != nil {
				t.Fatal(err)
			}
			return end
		}
		serve(callTurn(agentskill.ToolName, `{"name":"release"}`), callTurn("Bash", `{"command":"git status"}`))
		end := serve(callTurn("Bash", `{"command":"git clean -fdx"}`))
		if end.Reason != agentturn.ReasonInputRequired || slices.Contains(ran, "git clean -fdx") {
			t.Fatalf("own session %v: in another conversation git clean ran under the first one's grant: %q, ran %v", own, end.Reason, ran)
		}
	}
}

// When another conversation's call ends a kit's grants, the owner's
// session says why and the report is told once, and the owner's own
// read after it names the conversation that ended them, not itself
// twice. (#59)
func TestGrantsEndedByAnotherConversationSayWhy(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "release", "Bash(git:*)")
	sessions := agentsession.NewMemoryStore()
	var reports []agentkit.SkillGrant
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithSkills(skills),
		agentkit.WithTools(namedTool(t, "Bash")),
		agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{
			Read:    []string{agentskill.ToolName},
			Execute: []string{"Bash"},
		}), map[string]agentpolicy.ToolMatcher{
			"Bash": {Match: agentpolicy.PrefixMatcher("command")},
		}),
		agentkit.WithSkillGrants(trustedSkills),
		agentkit.WithSkillGrantReport(func(g agentkit.SkillGrant) { reports = append(reports, g) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	serve := func(rec *session.Recorder, turns ...func(*openresponses.Emitter) error) {
		t.Helper()
		cfg := kit.Config()
		cfg.Model = &scriptModel{turns: turns}
		cfg.ToolRecorder = rec.RecordFunc()
		agent := agentturn.New(cfg)
		defer rec.Attach(agent)()
		ctx := agentkit.ContextWithRecorder(session.ContextWithSessionID(t.Context(), rec.SessionID()), rec)
		if _, err := agent.Prompt(ctx, openresponses.UserText("go")); err != nil {
			t.Fatal(err)
		}
	}
	start := func() *session.Recorder {
		rec, _, err := session.Start(t.Context(), sessions, agentsession.Header{CWD: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		return rec
	}
	alice, bob := start(), start()
	serve(alice, callTurn(agentskill.ToolName, `{"name":"release"}`))
	reports = nil
	serve(bob, callTurn("Bash", `{"command":"git status"}`))

	if len(reports) != 1 || reports[0].Skill != "" || !errors.Is(reports[0].Err, agentkit.ErrSkillGrantConversation) || !strings.Contains(reports[0].Err.Error(), bob.SessionID()) {
		t.Fatalf("the trip reported %+v, want one report naming %s", reports, bob.SessionID())
	}
	named := false
	for _, c := range customEntries(openSession(t, sessions, alice.SessionID()), agentpolicy.VerdictNS) {
		if strings.Contains(string(c.Data), "skill grants ended") && strings.Contains(string(c.Data), bob.SessionID()) {
			named = true
		}
	}
	if !named {
		t.Fatal("the owner's session has no verdict saying which conversation ended its grants")
	}

	reports = nil
	serve(alice, callTurnID("call-again", agentskill.ToolName, `{"name":"release"}`))
	if len(reports) != 1 || !strings.Contains(reports[0].Err.Error(), "decided a call in session \""+bob.SessionID()+"\"") {
		t.Fatalf("the owner's read after the trip reported %+v, want one naming %s", reports, bob.SessionID())
	}
}

// Under the scope, a user message that ends a grant in force tells the
// model so, and how to get it back: a model with the skill's text above
// went straight to the tool and was refused as an ordinary ask. (#61)
func TestTheScopeTellsTheModelAGrantEnded(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "sysinfo", "Bash(uptime:*)")
	model := &scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn(agentskill.ToolName, `{"name":"sysinfo"}`),
	}}
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "m"),
		agentkit.WithSkills(skills),
		agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{
			Read:    []string{agentskill.ToolName},
			Execute: []string{"Bash"},
		}), map[string]agentpolicy.ToolMatcher{
			"Bash": {Match: agentpolicy.PrefixMatcher("command")},
		}),
		agentkit.WithSkillGrants(trustedSkills),
		agentkit.WithSkillGrantScope(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	agent := agentturn.New(kit.Config())
	for _, text := range []string{"what is the date", "and the uptime", "and the kernel"} {
		if _, err := agent.Prompt(t.Context(), openresponses.UserText(text)); err != nil {
			t.Fatal(err)
		}
	}
	notes := func(req openresponses.Request) []string {
		var out []string
		for _, item := range req.Input {
			if m, ok := item.(*openresponses.Message); ok && m.Role == openresponses.RoleDeveloper {
				for _, c := range m.Content {
					if in, ok := c.(*openresponses.InputText); ok {
						out = append(out, in.Text)
					}
				}
			}
		}
		return out
	}
	reqs := model.requests()
	last := notes(reqs[len(reqs)-1])
	if len(last) != 1 || !strings.Contains(last[0], "sysinfo (Bash(uptime:*))") || !strings.Contains(last[0], "read the skill again") {
		t.Fatalf("the notes the model was given = %q, want one naming sysinfo's ended grant", last)
	}
}

// A skill written after New is found by ReloadSkills: the tool serves
// it, the catalogue lists it, and the next config's instructions name
// it. (#62)
func TestReloadSkillsFindsASkillWrittenAfterNew(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "existing", "Read")
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithSkills(skills),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	tool := skillTool(t, kit)
	before := kit.Config().Instructions

	skillWithTools(t, skills, "learned-deploy", "Read")
	args, _ := json.Marshal(map[string]string{"name": "learned-deploy"})
	if _, err := tool.Execute(t.Context(), agenttool.Call{ID: "call-1", Args: args}); err == nil {
		t.Fatal("a skill written after New was served before a reload")
	}
	if err := kit.ReloadSkills(t.Context()); err != nil {
		t.Fatal(err)
	}
	readSkill(t, tool, "learned-deploy")
	if _, ok := kit.Catalog().Lookup("learned-deploy"); !ok {
		t.Fatal("the catalogue does not list the new skill")
	}
	if after := kit.Config().Instructions; after == before || !strings.Contains(after, "learned-deploy") {
		t.Fatalf("the instructions after the reload do not name the new skill:\n%s", after)
	}
	// A recorder asking about a run started with the old config still
	// gets its parts.
	if parts, _ := kit.PartsFor(t.Context(), openresponses.Request{Instructions: before}); parts == nil {
		t.Fatal("the config before the reload is no longer the kit's")
	}
}

// A restart passes over a live read whose skill changed since, which is
// right, but said nothing: the front saw a Replayed report for every
// other skill and nothing for the one it lost. Each pass-over is now
// reported with ErrSkillGrantChanged, FrontmatterChanged saying which
// digest moved, and the session records a verdict. The served
// instructions' digest covers the skill's file list and each file's
// size, so a file that grew by one byte is a change too. (#72)
func TestARestartReportsAReadItPassesOverBecauseTheSkillChanged(t *testing.T) {
	body := func(text string) string {
		return "---\nname: release\ndescription: what release is for\nallowed-tools: Bash(git:*) Bash(make:*)\n---\n\n" + text + "\n"
	}
	cases := []struct {
		name               string
		change             func(t *testing.T, skills string, kit *agentkit.Kit)
		changed            bool
		frontmatterChanged bool
	}{
		{"allowed-tools narrowed", func(t *testing.T, skills string, _ *agentkit.Kit) {
			skillWithTools(t, skills, "release", "Bash(git:*)")
		}, true, true},
		{"a file in the skill grew by one byte", func(t *testing.T, skills string, _ *agentkit.Kit) {
			writeFile(t, filepath.Join(skills, "release", "CHANGELOG.draft"), "notes\n\n")
		}, true, false},
		{"edited and reloaded, not read again", func(t *testing.T, skills string, kit *agentkit.Kit) {
			writeFile(t, filepath.Join(skills, "release", "SKILL.md"), body("do the thing, carefully"))
			if err := kit.ReloadSkills(t.Context()); err != nil {
				t.Fatal(err)
			}
		}, true, false},
		{"unchanged", nil, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			skills := filepath.Join(t.TempDir(), "skills")
			writeFile(t, filepath.Join(skills, "release", "SKILL.md"), body("do the thing"))
			writeFile(t, filepath.Join(skills, "release", "CHANGELOG.draft"), "notes\n")
			sessions := agentsession.NewMemoryStore()
			var reports []agentkit.SkillGrant
			build := func(model agentturn.Model, sess agentkit.Option) *agentkit.Kit {
				kit, err := agentkit.New(t.Context(),
					agentkit.WithModel(model, "m"),
					agentkit.WithSkills(skills),
					agentkit.WithPolicy(agentpolicy.FullAuto(agentpolicy.Tools{Read: []string{agentskill.ToolName}}),
						map[string]agentpolicy.ToolMatcher{"Bash": {Match: agentpolicy.PrefixMatcher("command")}}),
					agentkit.WithSkillGrants(trustedSkills),
					agentkit.WithSkillGrantReport(func(g agentkit.SkillGrant) { reports = append(reports, g) }),
					sess,
				)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = kit.Close() })
				return kit
			}
			first := build(&scriptModel{turns: []func(*openresponses.Emitter) error{
				callTurn(agentskill.ToolName, `{"name":"release"}`),
			}}, agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}))
			agent := agentturn.New(first.Config())
			unsubscribe := first.Attach(agent)
			if _, err := agent.Prompt(t.Context(), openresponses.UserText("cut the release")); err != nil {
				t.Fatal(err)
			}
			unsubscribe()
			if len(reports) != 1 || len(reports[0].Granted) != 2 {
				t.Fatalf("the read reported %+v, want one report granting both rules", reports)
			}
			if tc.change != nil {
				tc.change(t, skills, first)
			}
			_ = first.Close()

			reports = nil
			second := build(&scriptModel{}, agentkit.WithResumedSession(sessions, first.SessionID()))
			if len(reports) != 1 || !reports[0].Replayed || reports[0].Skill != "release" || reports[0].Location == "" {
				t.Fatalf("the restart reported %+v, want one report marked Replayed naming the skill", reports)
			}
			r := reports[0]
			grants := second.Engine().Grants()
			if !tc.changed {
				if r.Err != nil || len(r.Granted) != 2 || len(grants) != 1 {
					t.Fatalf("the control restart reported %+v and holds %d set(s), want the grant made again", r, len(grants))
				}
				return
			}
			if !errors.Is(r.Err, agentkit.ErrSkillGrantChanged) {
				t.Fatalf("Err = %v, want ErrSkillGrantChanged", r.Err)
			}
			if r.FrontmatterChanged != tc.frontmatterChanged {
				t.Fatalf("FrontmatterChanged = %v, want %v: %v", r.FrontmatterChanged, tc.frontmatterChanged, r.Err)
			}
			if len(r.Granted) != 0 || len(r.Refused) != 0 || len(grants) != 0 {
				t.Fatalf("a changed skill was granted again: report %+v, %d set(s) in force", r, len(grants))
			}
			said := false
			for _, c := range customEntries(openSession(t, sessions, first.SessionID()), agentpolicy.VerdictNS) {
				if strings.Contains(string(c.Data), "not granted again the tools of skill release") {
					said = true
				}
			}
			if !said {
				t.Fatal("the session has no verdict saying the read was not granted again")
			}
		})
	}
}

// A read refused with agentskill.ErrSkillChanged, the skill file gone,
// renamed or unparseable since discovery, told the model to "discover
// the skills again" and told the product nothing, though the product is
// the one party that can, through Kit.ReloadSkills. The read is now
// reported with Err wrapping that error; the model still gets the
// error, and the grant from the earlier read stands. (#73)
func TestAReadRefusedBecauseTheSkillChangedIsReported(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "deploy", "Bash(git:*)")
	var ran []string
	bash := agenttool.New("Bash", "run a command",
		func(_ context.Context, in struct {
			Command string `json:"command"`
		}) (string, error) {
			ran = append(ran, in.Command)
			return "", nil
		})
	var reports []agentkit.SkillGrant
	model := &scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn(agentskill.ToolName, `{"name":"deploy"}`),
		callTurn("Bash", `{"command":"git status"}`),
		nil, // the first run's answer
		callTurnID("call-again", agentskill.ToolName, `{"name":"deploy"}`),
	}}
	model.turns[2] = func(e *openresponses.Emitter) error {
		w, err := e.Message(openresponses.PhaseFinalAnswer)
		if err != nil {
			return err
		}
		if err := w.Text("deployed"); err != nil {
			return err
		}
		return w.Close()
	}
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
		agentkit.WithSkillGrants(trustedSkills),
		agentkit.WithSkillGrantReport(func(g agentkit.SkillGrant) { reports = append(reports, g) }),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	agent := agentturn.New(kit.Config())
	if end, err := agent.Prompt(t.Context(), openresponses.UserText("deploy")); err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("the first run ended %+v, %v; want git status run under the grant", end, err)
	}
	if !slices.Equal(ran, []string{"git status"}) || len(reports) != 1 {
		t.Fatalf("ran %v and reported %+v, want git status under one reported grant", ran, reports)
	}
	if err := os.Remove(filepath.Join(skills, "deploy", "SKILL.md")); err != nil {
		t.Fatal(err)
	}

	if _, err := agent.Prompt(t.Context(), openresponses.UserText("deploy again")); err != nil {
		t.Fatal(err)
	}
	told := false
	for _, item := range model.requests()[len(model.requests())-1].Input {
		if out, ok := item.(*openresponses.FunctionCallOutput); ok && strings.Contains(outputText(out), agentskill.ErrSkillChanged.Error()) {
			told = true
		}
	}
	if !told {
		t.Fatal("the model was not told the skill changed")
	}
	if len(reports) != 2 {
		t.Fatalf("reports after the refused read = %+v, want the read's and the refusal's", reports)
	}
	r := reports[1]
	if !errors.Is(r.Err, agentskill.ErrSkillChanged) || r.Skill != "deploy" || r.Location == "" || r.Replayed {
		t.Fatalf("the refused read reported %+v, want Skill, Location and Err wrapping agentskill.ErrSkillChanged", r)
	}
	if got := len(kit.Engine().Grants()); got != 1 {
		t.Fatalf("grants in force after the refused read = %d, want the earlier read's to stand", got)
	}
}

// A front that names conversations with session.ContextWithSessionID
// alone, as agentturn/front/a2a's WithRecorderFor example does, gives
// its kit no recorder, so the kit wrote every grant verdict nowhere.
// After a restart RegrantSkills found the read with no recorded grant,
// granted nothing and returned nil: the approval ran without the tools
// and nothing said why. It now refuses a session no recorder writes,
// and, given one, reports a read the session records no grant for. (#74)
func TestRegrantSkillsSaysWhenTheSessionRecordsNoGrant(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "deploy", "Bash(git:*)")
	sessions := agentsession.NewMemoryStore()
	var reports []agentkit.SkillGrant
	build := func(model agentturn.Model) *agentkit.Kit {
		kit, err := agentkit.New(t.Context(),
			agentkit.WithModel(model, "m"),
			agentkit.WithSkills(skills),
			agentkit.WithTools(namedTool(t, "Bash")),
			agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{
				Read:    []string{agentskill.ToolName},
				Execute: []string{"Bash"},
			}), map[string]agentpolicy.ToolMatcher{
				"Bash": {Match: agentpolicy.PrefixMatcher("command")},
			}),
			agentkit.WithSkillGrants(trustedSkills),
			agentkit.WithSkillGrantReport(func(g agentkit.SkillGrant) { reports = append(reports, g) }),
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = kit.Close() })
		return kit
	}
	// Conversation A, recorded by the front and named on the context, with
	// no ContextWithRecorder: the kit's observer writes nowhere.
	first := build(&scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn(agentskill.ToolName, `{"name":"deploy"}`),
		callTurn("Bash", `{"command":"rm -rf build"}`),
	}})
	rec, _, err := session.Start(t.Context(), sessions, agentsession.Header{CWD: t.TempDir()}, session.WithInstructionsParts(first.PartsFor))
	if err != nil {
		t.Fatal(err)
	}
	cfg := first.Config()
	cfg.ToolRecorder = rec.RecordFunc()
	agent := agentturn.New(cfg)
	detach := rec.Attach(agent)
	end, err := agent.Prompt(session.ContextWithSessionID(t.Context(), rec.SessionID()), openresponses.UserText("clean and check"))
	detach()
	if err != nil || end.Reason != agentturn.ReasonInputRequired {
		t.Fatalf("the run ended %+v, %v; want rm -rf held", end, err)
	}
	if len(reports) != 1 || len(reports[0].Granted) != 1 {
		t.Fatalf("the read reported %+v, want one grant", reports)
	}
	if got := grantedVerdicts(openSession(t, sessions, rec.SessionID())); got != 0 {
		t.Fatalf("A's session holds %d grant verdicts, want none: the kit had nowhere to write them", got)
	}
	_ = first.Close()

	// The restart.
	t.Run("a session no recorder writes is refused", func(t *testing.T) {
		reports = nil
		kit := build(&scriptModel{})
		_, sess, err := session.Resume(t.Context(), sessions, rec.SessionID(), session.WithInstructionsParts(kit.PartsFor))
		if err != nil {
			t.Fatal(err)
		}
		err = kit.RegrantSkills(session.ContextWithSessionID(t.Context(), rec.SessionID()), sess)
		if !errors.Is(err, agentkit.ErrSkillGrantRecorder) {
			t.Fatalf("RegrantSkills on A's context = %v, want ErrSkillGrantRecorder", err)
		}
		if n := len(kit.Engine().Grants()); n != 0 || len(reports) != 0 {
			t.Fatalf("%d set(s) in force and reports %+v after the refusal, want nothing", n, reports)
		}
	})
	t.Run("a read the session records no grant for is reported", func(t *testing.T) {
		reports = nil
		kit := build(&scriptModel{})
		resumed, sess, err := session.Resume(t.Context(), sessions, rec.SessionID(), session.WithInstructionsParts(kit.PartsFor))
		if err != nil {
			t.Fatal(err)
		}
		ctx := agentkit.ContextWithRecorder(session.ContextWithSessionID(t.Context(), rec.SessionID()), resumed)
		if err := kit.RegrantSkills(ctx, sess); err != nil {
			t.Fatal(err)
		}
		if len(reports) != 1 || !reports[0].Replayed || reports[0].Skill != "deploy" || !errors.Is(reports[0].Err, agentkit.ErrSkillGrantUnrecorded) {
			t.Fatalf("RegrantSkills reported %+v, want one Replayed report with ErrSkillGrantUnrecorded", reports)
		}
		if n := len(kit.Engine().Grants()); n != 0 {
			t.Fatalf("%d set(s) in force, want none: the session cannot vouch for the read", n)
		}
	})
}

// textTurn is a model turn that answers text and so ends the run.
func textTurn(text string) func(*openresponses.Emitter) error {
	return func(e *openresponses.Emitter) error {
		w, err := e.Message(openresponses.PhaseFinalAnswer)
		if err != nil {
			return err
		}
		if err := w.Text(text); err != nil {
			return err
		}
		return w.Close()
	}
}

// transferTool is a handoff as a tool: it ends the run after its batch
// so the host can switch the loop to another kit's config.
func transferTool(name string) agenttool.Tool {
	return agenttool.New(name, "hand the conversation to another agent",
		func(context.Context, struct {
			Reason string `json:"reason"`
		}) (agenttool.Result, error) {
			return agenttool.Result{Output: openresponses.FunctionCallOutputData{Text: "transferred"}, Terminate: true}, nil
		})
}

// outputsSaying is the text of every function_call_output in tr that
// contains s.
func outputsSaying(tr agentturn.Transcript, s string) []string {
	var out []string
	for _, item := range tr {
		item, _ = agentturn.Unhide(item)
		if o, ok := item.(*openresponses.FunctionCallOutput); ok && strings.Contains(o.Output.Text, s) {
			out = append(out, o.Output.Text)
		}
	}
	return out
}

// Under the scope a grant lasts until the user's next message, and the
// message counts however it arrived: here it arrives in another kit's
// run. Triage reads the refunds skill under message 1 and hands to
// billing; message 2 is served by billing, which hands back; triage's
// first turn after the handback opens on the transfer's output, not on
// the message, and issued the refund under message 1's grant. (#66)
func TestSkillGrantScopeEndsAGrantAnotherKitReceivedTheMessageFor(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "refunds", "issue_refund")
	sessions := agentsession.NewMemoryStore()
	refunds := 0
	refund := agenttool.New("issue_refund", "refund a charge",
		func(context.Context, struct {
			Customer string `json:"customer"`
		}) (string, error) {
			refunds++
			return "refunded", nil
		})
	triageModel := &scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn(agentskill.ToolName, `{"name":"refunds"}`),
		callTurn("transfer_to_billing", `{"reason":"double charge"}`),
		callTurn("issue_refund", `{"customer":"c-1"}`),
	}}
	triage, err := agentkit.New(t.Context(),
		agentkit.WithModel(triageModel, "m"),
		agentkit.WithSkills(skills),
		agentkit.WithTools(transferTool("transfer_to_billing"), refund),
		agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{
			Read:    []string{agentskill.ToolName, "transfer_to_billing"},
			Execute: []string{"issue_refund"},
		}), nil),
		agentkit.WithSkillGrants(trustedSkills),
		agentkit.WithSkillGrantScope(),
		agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer triage.Close()
	billingModel := &scriptModel{turns: []func(*openresponses.Emitter) error{
		textTurn("the second charge is the duplicate"),
		callTurn("transfer_to_triage", `{"reason":"refund it"}`),
	}}
	billing, err := agentkit.New(t.Context(),
		agentkit.WithModel(billingModel, "m"),
		agentkit.WithTools(transferTool("transfer_to_triage")),
		agentkit.WithRecorder(triage.Recorder()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer billing.Close()

	agent := agentturn.New(triage.Config())
	defer triage.Attach(agent)()
	// Message 1: triage reads the skill and hands off; billing answers.
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("I was double charged; I want a refund.")); err != nil {
		t.Fatal(err)
	}
	if err := agent.SetConfig(billing.Config()); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.Continue(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := len(triage.Engine().Grants()); got != 1 {
		t.Fatalf("grants after message 1 = %d, want the skill's", got)
	}
	// Message 2 reaches billing, which hands back to triage.
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("Please just refund it.")); err != nil {
		t.Fatal(err)
	}
	if err := agent.SetConfig(triage.Config()); err != nil {
		t.Fatal(err)
	}
	end, err := agent.Continue(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := len(triage.Engine().Grants()); got != 0 {
		t.Errorf("grants after message 2 = %d, want 0: the user's message ended them", got)
	}
	// Only the ended grant allowed the refund, so the kit refuses it
	// naming the skill (#76) rather than the engine holding it.
	refused := outputsSaying(agent.State().Transcript, "ended with the user's last message")
	if refunds != 0 || end.Reason != agentturn.ReasonDone || len(refused) != 1 || !strings.Contains(refused[0], "skill refunds granted") {
		t.Errorf("issue_refund ran %d times, run ended %q, refusals %q; want it refused under message 2 naming the refunds skill", refunds, end.Reason, refused)
	}
	var devs []string
	for _, item := range agent.State().Transcript {
		if m, ok := item.(*openresponses.Message); ok && m.Role == openresponses.RoleDeveloper {
			devs = append(devs, m.Text())
		}
	}
	if len(devs) != 1 || !strings.Contains(devs[0], "refunds (issue_refund)") {
		t.Errorf("developer notes = %q, want one naming the refunds grant that ended", devs)
	}
}

// A grant granted again after a restart is bound to the user message
// the session's path ends on, the one the seeded transcript ends on
// too, so the first turn after the restart does not end it: the held
// call's approval runs the task on under the grant. Two messages, so
// the count is not the one a fresh conversation starts at. (#66)
func TestAReplayedGrantIsBoundToThePathsLastUserMessage(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "release", "Bash(git:*)")
	sessions := agentsession.NewMemoryStore()
	var ran []string
	bash := agenttool.New("Bash", "run a command",
		func(_ context.Context, in struct {
			Command string `json:"command"`
		}) (string, error) {
			ran = append(ran, in.Command)
			return "", nil
		})
	build := func(model agentturn.Model, sess agentkit.Option) *agentkit.Kit {
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
			agentkit.WithSkillGrants(trustedSkills),
			agentkit.WithSkillGrantScope(),
			sess,
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = kit.Close() })
		return kit
	}
	first := build(&scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn(agentskill.ToolName, `{"name":"release"}`),
		callTurn("Bash", `{"command":"git status"}`),
		textTurn("clean"),
		callTurnID("call-again", agentskill.ToolName, `{"name":"release"}`),
		callTurnID("call-rm", "Bash", `{"command":"rm -rf build"}`),
	}}, agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}))
	agent := agentturn.New(first.Config(), first.AgentOptions()...)
	unsubscribe := first.Attach(agent)
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("is it clean")); err != nil {
		t.Fatal(err)
	}
	end, err := agent.Prompt(t.Context(), openresponses.UserText("cut the release"))
	unsubscribe()
	if err != nil {
		t.Fatal(err)
	}
	if end.Reason != agentturn.ReasonInputRequired {
		t.Fatalf("reason = %q, want rm -rf held", end.Reason)
	}

	second := build(&scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurnID("call-log", "Bash", `{"command":"git log"}`),
	}}, agentkit.WithResumedSession(sessions, first.SessionID()))
	if got := len(second.Engine().Grants()); got != 1 {
		t.Fatalf("grants in force after the restart = %d, want the skill's", got)
	}
	resumed := agentturn.New(second.Config(), second.AgentOptions()...)
	defer second.Attach(resumed)()
	pending := resumed.State().Pending
	if len(pending) != 1 {
		t.Fatalf("pending = %+v, want the held rm", pending)
	}
	end, err = resumed.Resume(t.Context(), agentturn.Approve(pending[0].Call.CallID).WithBy(agentpolicy.ByHuman))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(second.Engine().Grants()); got != 1 {
		t.Errorf("grants after the Resume = %d, want the replayed grant kept", got)
	}
	if end.Reason == agentturn.ReasonInputRequired || !slices.Equal(ran, []string{"git status", "rm -rf build", "git log"}) {
		t.Fatalf("ran %v, ended %q; want git log run under the grant after the approved rm", ran, end.Reason)
	}
}

// Under the scope, a call that only an ended grant allowed is refused by
// the kit with a reason that names the skill and says to read it again,
// ahead of the engine, so the model that reads the refusal knows what to
// do and no reviewer is asked. The engine decides every other call as
// before. (#76)
func TestACallOnlyAnEndedGrantAllowedIsRefusedNamingTheSkill(t *testing.T) {
	type tc struct {
		name     string
		allowed  string
		policy   agentpolicy.Policy
		matchers map[string]agentpolicy.ToolMatcher
		first    string // the args the model calls the tool with under the grant
		second   string // the args it calls it with after the user's next message
		tool     string
		want     string // "blocked", "ran", "asked" or "denied"
	}
	ask := func(tools ...string) []agentpolicy.Rule {
		rules, err := agentpolicy.ParseRules(strings.Join(tools, " "))
		if err != nil {
			t.Fatal(err)
		}
		return rules
	}
	split := func(args json.RawMessage) ([]agentpolicy.Subject, error) {
		var in struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(args, &in); err != nil {
			return nil, err
		}
		var out []agentpolicy.Subject
		for _, part := range strings.Split(in.Command, " && ") {
			a, _ := json.Marshal(map[string]string{"command": part})
			out = append(out, agentpolicy.Subject{Args: a, Text: part})
		}
		return out, nil
	}
	bashMatchers := map[string]agentpolicy.ToolMatcher{"bash": {Match: agentpolicy.PrefixMatcher("command"), Subjects: split}}
	cases := []tc{
		{"the filing: an ask rule the grant answered", "act", agentpolicy.Policy{Ask: ask("act"), Default: agentpolicy.Allow()}, nil, `{"what":"it"}`, `{"what":"it"}`, "act", "blocked"},
		{"a policy that allows the tool outright", "act", agentpolicy.Policy{Allow: ask("act"), Default: agentpolicy.Ask()}, nil, `{"what":"it"}`, `{"what":"it"}`, "act", "ran"},
		{"a policy that denies the tool", "act", agentpolicy.Policy{Deny: ask("act"), Default: agentpolicy.Allow()}, nil, `{"what":"it"}`, `{"what":"it"}`, "act", "denied"},
		{"a call to another tool", "act", agentpolicy.Policy{Ask: ask("act", "other"), Default: agentpolicy.Allow()}, nil, `{"what":"it"}`, `{"what":"it"}`, "other", "asked"},
		{"a subject the ended rule covers", "bash(uptime:*)", agentpolicy.Policy{Ask: ask("bash"), Default: agentpolicy.Allow()}, bashMatchers, `{"command":"uptime"}`, `{"command":"uptime"}`, "bash", "blocked"},
		{"a compound command the ended rule does not cover", "bash(uptime:*)", agentpolicy.Policy{Ask: ask("bash"), Default: agentpolicy.Allow()}, bashMatchers, `{"command":"uptime"}`, `{"command":"uptime && rm -rf x"}`, "bash", "asked"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "clock", c.allowed)
			// The skill tool itself is allowed under every policy here.
			c.policy.Allow = append(c.policy.Allow, ask(agentskill.ToolName)...)
			sessions := agentsession.NewMemoryStore()
			runs := map[string]int{}
			tool := func(name string) agenttool.Tool {
				return agenttool.New(name, "a tool", func(context.Context, struct {
					What    string `json:"what,omitempty"`
					Command string `json:"command,omitempty"`
				}) (string, error) {
					runs[name]++
					return name + " ran", nil
				})
			}
			firstTool := "act"
			if c.tool == "bash" {
				firstTool = "bash"
			}
			model := &scriptModel{turns: []func(*openresponses.Emitter) error{
				callTurn(agentskill.ToolName, `{"name":"clock"}`),
				callTurnID("call-first", firstTool, c.first),
				textTurn("done it"),
				callTurnID("call-second", c.tool, c.second),
				callTurnID("call-reread", agentskill.ToolName, `{"name":"clock"}`),
				callTurnID("call-third", c.tool, c.second),
			}}
			kit, err := agentkit.New(t.Context(),
				agentkit.WithModel(model, "m"),
				agentkit.WithSkills(skills),
				agentkit.WithTools(tool("act"), tool("other"), tool("bash")),
				agentkit.WithPolicy(c.policy, c.matchers),
				agentkit.WithSkillGrants(func(sk *agentskill.Skill) agentpolicy.Source {
					return agentpolicy.Source{Name: "agentskill:" + sk.ListedName(), Path: sk.Location, Trusted: true}
				}),
				agentkit.WithSkillGrantScope(),
				agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer kit.Close()
			agent := agentturn.New(kit.Config())
			defer kit.Attach(agent)()
			if _, err := agent.Prompt(t.Context(), openresponses.UserText("use the skill")); err != nil {
				t.Fatal(err)
			}
			end, err := agent.Prompt(t.Context(), openresponses.UserText("act again"))
			if err != nil {
				t.Fatal(err)
			}
			tr := agent.State().Transcript
			ours := outputsSaying(tr, "ended with the user's last message")
			switch c.want {
			case "blocked":
				if len(ours) != 1 || !strings.Contains(ours[0], "skill clock granted") || !strings.Contains(ours[0], "read the skill again with the "+agentskill.ToolName+" tool") {
					t.Fatalf("the refusals the model read = %q, want one naming skill clock and the %s tool", ours, agentskill.ToolName)
				}
				if end.Reason != agentturn.ReasonDone || runs[c.tool] != 2 {
					t.Fatalf("run ended %q, %s ran %d times; want done with the call run once under the grant and once after the read", end.Reason, c.tool, runs[c.tool])
				}
				recorded := false
				for _, e := range customEntries(openSession(t, sessions, kit.SessionID()), agentpolicy.VerdictNS) {
					if strings.Contains(string(e.Data), "skill clock granted ended") {
						recorded = true
					}
				}
				if !recorded {
					t.Fatal("the session has no verdict for the refusal")
				}
			case "ran":
				if len(ours) != 0 || end.Reason != agentturn.ReasonDone || runs[c.tool] != 3 {
					t.Fatalf("refusals %q, run ended %q, %s ran %d times; want the engine's own allow", ours, end.Reason, c.tool, runs[c.tool])
				}
			case "asked":
				if len(ours) != 0 || end.Reason != agentturn.ReasonInputRequired || len(end.Pending) != 1 {
					t.Fatalf("refusals %q, run ended %q with %d pending; want the engine's ask", ours, end.Reason, len(end.Pending))
				}
			case "denied":
				denied := outputsSaying(tr, "denied by")
				if len(ours) != 0 || len(denied) == 0 || runs[c.tool] != 0 {
					t.Fatalf("refusals %q, denials %q, %s ran %d times; want the deny's reason alone", ours, denied, c.tool, runs[c.tool])
				}
			}
		})
	}
}
