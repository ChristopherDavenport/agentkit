package agentkit_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// One kit serving several conversations, each its own session: a skill
// read in one is granted there, under that conversation's grant scope,
// and decides no other conversation's calls; a read in another is
// granted there, and ending one conversation's grants ends no other's.
// (#44, #18)
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
	inCtx := func(rec *session.Recorder) context.Context {
		return agentkit.ContextWithRecorder(session.ContextWithSessionID(t.Context(), rec.SessionID()), rec)
	}
	// The engine holds the grant under the conversation that read the
	// skill, and decides no one else's calls by it; the second
	// conversation's call did not end it.
	if got := kit.GrantScope(inCtx(alice)); got != alice.SessionID() {
		t.Fatalf("the grant scope of the first conversation = %q, want its session %q", got, alice.SessionID())
	}
	grantsOf := func(rec *session.Recorder) int {
		return len(kit.Engine().GrantsFor(agentpolicy.ContextWithGrantScope(t.Context(), kit.GrantScope(inCtx(rec)))))
	}
	if a, b := grantsOf(alice), grantsOf(bob); a != 1 || b != 0 {
		t.Fatalf("grants in force: %d for the owner's conversation, %d for the other's; want 1 and 0", a, b)
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
	if a, b := revoked(alice), revoked(bob); a != 0 || b != 0 {
		t.Fatalf("revocations recorded: %d in the owner's session, %d in the other's; want none, the second conversation ended nothing", a, b)
	}

	// The second conversation reads the skill and is granted it, a grant
	// of its own, reported under its own scope.
	reports = nil
	carol, end := serve(callTurn(agentskill.ToolName, `{"name":"release"}`), callTurn("Bash", `{"command":"git log"}`))
	if end.Reason == agentturn.ReasonInputRequired || !slices.Equal(ran, []string{"git status", "git log"}) {
		t.Fatalf("a read in another conversation did not grant it the skill: %q, ran %v", end.Reason, ran)
	}
	if len(reports) != 1 || reports[0].Err != nil || reports[0].Scope != carol.SessionID() || len(reports[0].Granted) == 0 {
		t.Fatalf("a read in another conversation reported %+v, want a grant under scope %s", reports, carol.SessionID())
	}
	if a, c := grantsOf(alice), grantsOf(carol); a != 1 || c != 1 || len(kit.Engine().Grants()) != 2 {
		t.Fatalf("grants in force: %d and %d, %d in all; want one for each of two conversations", a, c, len(kit.Engine().Grants()))
	}

	// Ending one conversation ends its grants and no other's.
	if n := kit.RevokeSkillGrants(inCtx(alice)); n != 1 {
		t.Fatalf("RevokeSkillGrants removed %d rules, want the one conversation's one", n)
	}
	if a, c := grantsOf(alice), grantsOf(carol); a != 0 || c != 1 {
		t.Fatalf("grants in force after the first conversation ended: %d and %d, want 0 and 1", a, c)
	}
	if a := revoked(alice); a != 1 {
		t.Fatalf("revocations recorded in the ended conversation's session: %d, want 1", a)
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

// Under the scope a conversation's grant ends with that conversation's
// next message, and another conversation's message ends nothing of it:
// the grants are each their conversation's, so the kit's one engine
// serves a second conversation's first message without touching the
// first's grant. (#59, #18)
func TestAConversationsMessageEndsOnlyItsOwnGrants(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "release", "Bash(git:*)")
	sessions := agentsession.NewMemoryStore()
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
		agentkit.WithSkillGrantScope(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	ctxOf := func(rec *session.Recorder) context.Context {
		return agentkit.ContextWithRecorder(session.ContextWithSessionID(t.Context(), rec.SessionID()), rec)
	}
	serve := func(rec *session.Recorder, turns ...func(*openresponses.Emitter) error) {
		t.Helper()
		cfg := kit.Config()
		cfg.Model = &scriptModel{turns: turns}
		cfg.ToolRecorder = rec.RecordFunc()
		agent := agentturn.New(cfg)
		defer rec.Attach(agent)()
		if _, err := agent.Prompt(ctxOf(rec), openresponses.UserText("go")); err != nil {
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
	inForce := func(rec *session.Recorder) int {
		ctx := ctxOf(rec)
		return len(kit.Engine().GrantsFor(agentpolicy.ContextWithGrantScope(ctx, kit.GrantScope(ctx))))
	}
	alice, bob := start(), start()
	serve(alice, callTurn(agentskill.ToolName, `{"name":"release"}`))
	serve(bob, callTurn(agentskill.ToolName, `{"name":"release"}`))
	if a, b := inForce(alice), inForce(bob); a != 1 || b != 1 {
		t.Fatalf("grants in force after each read the skill: %d and %d, want one each", a, b)
	}

	// Bob's next message ends Bob's grant, and Alice's stays.
	serve(bob, textTurn("hello"))
	if a, b := inForce(alice), inForce(bob); a != 1 || b != 0 {
		t.Fatalf("grants in force after Bob's next message: %d for Alice, %d for Bob; want 1 and 0", a, b)
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
	if a, b := revoked(alice), revoked(bob); a != 0 || b != 1 {
		t.Fatalf("revocations recorded: %d in Alice's session, %d in Bob's; want 0 and 1", a, b)
	}
	serve(alice, textTurn("hello"))
	if a := inForce(alice); a != 0 {
		t.Fatalf("grants in force after Alice's next message: %d, want 0", a)
	}
}

// A child agent runs under a grant scope of its own, under its
// conversation's: what its parent read grants the child nothing, a skill
// the child reads is granted to the child alone, and the parent's next
// message ends the child's grant with the parent's. (#18)
func TestAChildAgentHasAGrantScopeOfItsOwn(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "release", "Bash(git:*)")
	sessions := agentsession.NewMemoryStore()
	var (
		kit    *agentkit.Kit
		ran    []string
		scopes []string
	)
	bash := agenttool.New("Bash", "run a command",
		func(_ context.Context, in struct {
			Command string `json:"command"`
		}) (string, error) {
			ran = append(ran, in.Command)
			return "", nil
		})
	probe := agenttool.New("probe", "report the grant scope",
		func(ctx context.Context, _ agenttool.NoArgs) (string, error) {
			scopes = append(scopes, agentpolicy.GrantScopeFromContext(ctx))
			return "", nil
		})
	// The child reads the skill through the kit's own tool, which is the
	// one that grants.
	read := agenttool.New("read_release", "read the release skill",
		func(ctx context.Context, _ agenttool.NoArgs) (string, error) {
			tool, ok := kit.LookupTool(agentskill.ToolName)
			if !ok {
				return "", errors.New("no skill tool")
			}
			_, err := tool.Execute(ctx, agenttool.Call{ID: "call-read", Args: json.RawMessage(`{"name":"release"}`)})
			return "", err
		})
	// The first child tries git status, which asks, and ends; the second
	// reads the skill itself and reports its scope.
	child := &scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn("probe", `{}`),
		callTurn("Bash", `{"command":"git status"}`),
		callTurn("read_release", `{}`),
		callTurn("probe", `{}`),
	}}
	parent := &scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn(agentskill.ToolName, `{"name":"release"}`),
		callTurnID("call-first", "explore", `{"input":"look around"}`),
		callTurnID("call-second", "explore", `{"input":"read the skill"}`),
		callTurn("Bash", `{"command":"git log"}`),
	}}
	var err error
	kit, err = agentkit.New(t.Context(),
		agentkit.WithModel(parent, "m"),
		agentkit.WithSkills(skills),
		agentkit.WithTools(bash),
		agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{
			Read:    []string{agentskill.ToolName, "probe", "read_release", "explore"},
			Execute: []string{"Bash"},
		}), map[string]agentpolicy.ToolMatcher{
			"Bash": {Match: agentpolicy.PrefixMatcher("command")},
		}),
		agentkit.WithSkillGrants(trustedSkills),
		agentkit.WithSkillGrantScope(),
		agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}),
		agentkit.WithChildAgent(agentturn.Config{
			Name:        "explore",
			Description: "delegate",
			Model:       child,
			ModelName:   "m",
			Tools:       []agenttool.Tool{probe, bash, read},
			// The child's calls are decided by the kit's engine, as a
			// product that shares its policy with its children does.
			BeforeToolCall: func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
				return kit.Engine().BeforeToolCall()(ctx, info)
			},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	agent := agentturn.New(kit.Config())
	defer kit.Attach(agent)()
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("go")); err != nil {
		t.Fatal(err)
	}

	parentScope := kit.SessionID()
	firstScope := parentScope + "/" + agentsession.SubsessionID(parentScope, "call-first")
	childScope := parentScope + "/" + agentsession.SubsessionID(parentScope, "call-second")
	if len(scopes) != 2 || scopes[0] != firstScope || scopes[1] != childScope {
		t.Fatalf("the children's tools ran under scopes %q, want each its own, %q and %q", scopes, firstScope, childScope)
	}
	if slices.Contains(ran, "git status") {
		t.Fatalf("the child ran git status under its parent's grant: %v", ran)
	}
	if !slices.Contains(ran, "git log") {
		t.Fatalf("the parent did not run git log under its own grant: %v", ran)
	}
	ctxOf := func(scope string) context.Context { return agentpolicy.ContextWithGrantScope(t.Context(), scope) }
	if p, c := len(kit.Engine().GrantsFor(ctxOf(parentScope))), len(kit.Engine().GrantsFor(ctxOf(childScope))); p != 1 || c != 1 {
		t.Fatalf("grants in force: %d for the parent, %d for the child; want one each, the child's read granted it alone", p, c)
	}

	// The parent's next message ends the child's grant with its own.
	if _, err := agent.Prompt(t.Context(), openresponses.UserText("again")); err != nil {
		t.Fatal(err)
	}
	if p, c := len(kit.Engine().GrantsFor(ctxOf(parentScope))), len(kit.Engine().GrantsFor(ctxOf(childScope))); p != 0 || c != 0 || len(kit.Engine().Grants()) != 0 {
		t.Fatalf("grants in force after the parent's next message: %d, %d, %d in all; want none", p, c, len(kit.Engine().Grants()))
	}
}

// The children of a conversation are the scopes recorded as run under
// it, not the scopes whose names begin with its own: a front that names
// its conversations "user/4", "user/42" and "user/42/chat7" has three
// conversations, and a message in the first ends nothing of the others,
// nor does ending the second end the third's.
func TestAScopeIsNotTheParentOfAScopeItsNameBeginsWith(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "release", "Bash(git:*)")
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
		agentkit.WithSkillGrantScope(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	scopes := []string{"user/4", "user/42", "user/42/chat7"}
	ctxOf := func(scope string) context.Context { return agentpolicy.ContextWithGrantScope(t.Context(), scope) }
	tool := skillTool(t, kit)
	for _, scope := range scopes {
		args := json.RawMessage(`{"name":"release"}`)
		if _, err := tool.Execute(ctxOf(scope), agenttool.Call{ID: "call-" + scope, Args: args}); err != nil {
			t.Fatal(err)
		}
	}
	inForce := func() []int {
		var out []int
		for _, scope := range scopes {
			out = append(out, len(kit.Engine().GrantsFor(ctxOf(scope))))
		}
		return out
	}
	if got := inForce(); !slices.Equal(got, []int{1, 1, 1}) {
		t.Fatalf("grants in force after each read the skill = %v, want one in each", got)
	}

	// A message in user/4 ends user/4's grant alone.
	turn := func(scope string) {
		t.Helper()
		info := agentturn.TurnStartInfo{RunID: "run-" + scope, Transcript: agentturn.Transcript{openresponses.UserText("hello")}}
		if _, err := kit.Config().BeforeTurn(ctxOf(scope), info); err != nil {
			t.Fatal(err)
		}
	}
	turn("user/4")
	if got := inForce(); !slices.Equal(got, []int{0, 1, 1}) {
		t.Fatalf("grants in force after a message in user/4 = %v, want 0, 1, 1", got)
	}
	// Ending user/42 ends user/42's grant alone, and a message in it too.
	if n := kit.RevokeSkillGrants(ctxOf("user/42")); n != 1 {
		t.Fatalf("RevokeSkillGrants(user/42) removed %d rules, want its own one", n)
	}
	if got := inForce(); !slices.Equal(got, []int{0, 0, 1}) {
		t.Fatalf("grants in force after user/42 ended = %v, want 0, 0, 1", got)
	}
	turn("user/42")
	if got := inForce(); !slices.Equal(got, []int{0, 0, 1}) {
		t.Fatalf("grants in force after a message in user/42 = %v, want user/42/chat7's kept", got)
	}
}

// A message the transcript's tail test does not see, a steer delivered
// before an output, still ends the grants of the child agents of its
// conversation: they are bound to the user message their parent's turn
// was under, and the count of the transcript now is higher. The
// conversation holds no grant of its own here, so nothing else would end
// them.
func TestAMessageTheTailMissesEndsTheChildrensGrants(t *testing.T) {
	for _, byCall := range []bool{false, true} {
		t.Run(fmt.Sprintf("RevokeSkillGrants=%v", byCall), func(t *testing.T) {
			skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "release", "Bash(git:*)")
			sessions := agentsession.NewMemoryStore()
			var kit *agentkit.Kit
			read := agenttool.New("read_release", "read the release skill",
				func(ctx context.Context, _ agenttool.NoArgs) (string, error) {
					tool, ok := kit.LookupTool(agentskill.ToolName)
					if !ok {
						return "", errors.New("no skill tool")
					}
					_, err := tool.Execute(ctx, agenttool.Call{ID: "call-read", Args: json.RawMessage(`{"name":"release"}`)})
					return "", err
				})
			child := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurn("read_release", `{}`)}}
			parent := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurn("explore", `{"input":"read the skill"}`)}}
			var err error
			kit, err = agentkit.New(t.Context(),
				agentkit.WithModel(parent, "m"),
				agentkit.WithSkills(skills),
				agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{
					Read: []string{agentskill.ToolName, "read_release", "explore"},
				}), map[string]agentpolicy.ToolMatcher{"Bash": {Match: agentpolicy.PrefixMatcher("command")}}),
				agentkit.WithSkillGrants(trustedSkills),
				agentkit.WithSkillGrantScope(),
				agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}),
				agentkit.WithChildAgent(agentturn.Config{
					Name: "explore", Description: "delegate", Model: child, ModelName: "m",
					Tools: []agenttool.Tool{read},
					BeforeToolCall: func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
						return kit.Engine().BeforeToolCall()(ctx, info)
					},
				}),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer kit.Close()
			agent := agentturn.New(kit.Config())
			defer kit.Attach(agent)()
			if _, err := agent.Prompt(t.Context(), openresponses.UserText("go")); err != nil {
				t.Fatal(err)
			}
			if n := len(kit.Engine().Grants()); n != 1 {
				t.Fatalf("grants in force after the child read the skill = %d, want the child's one", n)
			}

			ctx := agentkit.ContextWithRecorder(session.ContextWithSessionID(t.Context(), kit.SessionID()), kit.Recorder())
			if byCall {
				// Ending the conversation ends its children's grants too.
				if n := kit.RevokeSkillGrants(ctx); n != 1 {
					t.Fatalf("RevokeSkillGrants removed %d rules, want the child's one", n)
				}
			} else {
				// Two user messages and an output last: the tail test sees none.
				transcript := agentturn.Transcript{
					openresponses.UserText("go"),
					openresponses.UserText("a steer"),
					openresponses.NewFunctionCallOutput("call-x", "done"),
				}
				if _, err := kit.Config().BeforeTurn(ctx, agentturn.TurnStartInfo{RunID: "later", Transcript: transcript}); err != nil {
					t.Fatal(err)
				}
			}
			if n := len(kit.Engine().Grants()); n != 0 {
				t.Fatalf("grants in force after the conversation's message or end = %d, want the child's ended", n)
			}
		})
	}
}

// Without skill grants the kit puts no grant scope on a child agent's
// context: a scope the product put on the host's, and grants of its own
// under it, are the child's as they are any tool call's.
func TestAChildKeepsTheProductsOwnGrantScopeWithoutSkillGrants(t *testing.T) {
	var ran []string
	bash := agenttool.New("Bash", "run a command",
		func(_ context.Context, in struct {
			Command string `json:"command"`
		}) (string, error) {
			ran = append(ran, in.Command)
			return "", nil
		})
	var kit *agentkit.Kit
	child := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurn("Bash", `{"command":"git status"}`)}}
	parent := &scriptModel{turns: []func(*openresponses.Emitter) error{callTurn("explore", `{"input":"look"}`)}}
	var err error
	kit, err = agentkit.New(t.Context(),
		agentkit.WithModel(parent, "m"),
		agentkit.WithTools(bash),
		agentkit.WithPolicy(agentpolicy.Suggest(agentpolicy.Tools{
			Read:    []string{"explore"},
			Execute: []string{"Bash"},
		}), map[string]agentpolicy.ToolMatcher{"Bash": {Match: agentpolicy.PrefixMatcher("command")}}),
		agentkit.WithSession(agentsession.NewMemoryStore(), agentsession.Header{CWD: t.TempDir()}),
		agentkit.WithChildAgent(agentturn.Config{
			Name: "explore", Description: "delegate", Model: child, ModelName: "m",
			Tools: []agenttool.Tool{bash},
			BeforeToolCall: func(ctx context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
				return kit.Engine().BeforeToolCall()(ctx, info)
			},
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()

	ctx := agentpolicy.ContextWithGrantScope(t.Context(), "host")
	granted, refused := kit.Engine().GrantSet(ctx, agentpolicy.RuleSet{
		Source: agentpolicy.Source{Name: "product", Trusted: true},
		Allow:  []agentpolicy.Rule{{Tool: "Bash", Spec: "git:*"}},
	})
	if len(granted) != 1 || len(refused) != 0 {
		t.Fatalf("the product's grant: granted %v, refused %v", granted, refused)
	}
	agent := agentturn.New(kit.Config())
	defer kit.Attach(agent)()
	if _, err := agent.Prompt(ctx, openresponses.UserText("go")); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ran, []string{"git status"}) {
		t.Fatalf("the child ran %v, want git status under the product's own grant scope", ran)
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
// as the first hook inside the engine, so the model that reads the
// refusal knows what to do and no reviewer is asked. The engine decides every other call as
// before. (#76)
func TestACallOnlyAnEndedGrantAllowedIsRefusedNamingTheSkill(t *testing.T) {
	type tc struct {
		name     string
		allowed  string // the skill's allowed-tools
		policy   agentpolicy.Policy
		matchers map[string]agentpolicy.ToolMatcher
		opts     []agentpolicy.Option
		hook     func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) // WithBeforeToolCall
		tool     string                                                                         // the tool the model calls after the user's next message
		first    string                                                                         // the args it calls the tool with under the grant
		second   string                                                                         // the args it calls it with after the message
		want     string                                                                         // "blocked", "ran", "asked" or "denied"
	}
	rules := func(text string) []agentpolicy.Rule {
		out, err := agentpolicy.ParseRules(text)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	split := func(_ context.Context, args json.RawMessage) ([]agentpolicy.Subject, error) {
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
	// deferSecondCall asks about the call the model makes after the
	// grant ended, as a product's own hook asks about a call of its own.
	deferSecondCall := func(_ context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
		if info.Call != nil && info.Call.CallID == "call-second" {
			return &agentturn.ToolDecision{Action: agentturn.Defer, Reason: "the product wants to know"}, nil
		}
		return nil, nil
	}
	it := `{"what":"it"}`
	cases := []tc{
		{name: "the filing: a bare ask rule the grant answered", allowed: "act",
			policy: agentpolicy.Policy{Ask: rules("act"), Default: agentpolicy.Allow()},
			tool:   "act", first: it, second: it, want: "blocked"},
		{name: "a policy that allows the tool outright", allowed: "act",
			policy: agentpolicy.Policy{Allow: rules("act"), Default: agentpolicy.Ask()},
			tool:   "act", first: it, second: it, want: "ran"},
		{name: "a policy that denies the tool", allowed: "act",
			policy: agentpolicy.Policy{Deny: rules("act"), Default: agentpolicy.Allow()},
			tool:   "act", first: it, second: it, want: "denied"},
		{name: "a call to another tool", allowed: "act",
			policy: agentpolicy.Policy{Ask: rules("act other"), Default: agentpolicy.Allow()},
			tool:   "other", first: it, second: it, want: "asked"},
		{name: "a subject the ended rule covers", allowed: "bash(uptime:*)",
			policy: agentpolicy.Policy{Ask: rules("bash"), Default: agentpolicy.Allow()}, matchers: bashMatchers,
			tool: "bash", first: `{"command":"uptime"}`, second: `{"command":"uptime"}`, want: "blocked"},
		{name: "a compound command the ended rule does not cover", allowed: "bash(uptime:*)",
			policy: agentpolicy.Policy{Ask: rules("bash"), Default: agentpolicy.Allow()}, matchers: bashMatchers,
			tool: "bash", first: `{"command":"uptime"}`, second: `{"command":"uptime && rm -rf x"}`, want: "asked"},
		// The engine defers only when an ask rule fires. An ask rule with a
		// specifier that does not match the call leaves it allowed by
		// default, and so does a bare ask a carve-out cancels: any
		// specifier naming the tool is the engine's to read.
		{name: "an ask rule with a specifier the call does not match", allowed: "bash(git:*)",
			policy: agentpolicy.Policy{Ask: rules("bash(rm:*)"), Default: agentpolicy.Allow()}, matchers: bashMatchers,
			tool: "bash", first: `{"command":"git status"}`, second: `{"command":"git status"}`, want: "ran"},
		{name: "a bare ask a carve-out cancels", allowed: "bash(uptime:*)",
			policy: agentpolicy.Policy{Ask: rules("bash bash(!uptime:*)"), Default: agentpolicy.Allow()}, matchers: bashMatchers,
			tool: "bash", first: `{"command":"uptime"}`, second: `{"command":"uptime"}`, want: "ran"},
		// The kit asks the engine what it would decide, [agentpolicy.Engine.Would],
		// so an agentpolicy option of the product's, which may change how
		// the engine reads its own state, is read as the engine reads it.
		{name: "an engine option that changes nothing the call needs", allowed: "act",
			policy: agentpolicy.Policy{Ask: rules("act"), Default: agentpolicy.Allow()},
			opts:   []agentpolicy.Option{agentpolicy.WithConfinement(func(context.Context, agenttool.Tool, json.RawMessage) (bool, string) { return false, "" })},
			tool:   "act", first: it, second: it, want: "blocked"},
		{name: "an engine option that lets the call past the ask", allowed: "act",
			policy: agentpolicy.Policy{Ask: rules("act"), Default: agentpolicy.Allow()},
			opts:   []agentpolicy.Option{agentpolicy.WithConfinement(func(context.Context, agenttool.Tool, json.RawMessage) (bool, string) { return true, "sandbox" })},
			tool:   "act", first: it, second: it, want: "ran"},
		{name: "a hook of the product's that allows nothing more", allowed: "act",
			policy: agentpolicy.Policy{Ask: rules("act"), Default: agentpolicy.Allow()},
			opts: []agentpolicy.Option{agentpolicy.WithHooks(func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
				return nil, nil
			})},
			tool: "act", first: it, second: it, want: "blocked"},
		// A hook of the product's that defers is the engine's to decide
		// with: the kit does not answer its question with a refusal naming
		// a skill that would not have helped, and does when an ask rule is
		// behind the verdict as well.
		{name: "a product hook that defers a call the policy allows", allowed: "act",
			policy: agentpolicy.Policy{Allow: rules("act"), Default: agentpolicy.Ask()}, hook: deferSecondCall,
			tool: "act", first: it, second: it, want: "asked"},
		{name: "a product hook that defers beside an ask rule", allowed: "act",
			policy: agentpolicy.Policy{Ask: rules("act"), Default: agentpolicy.Allow()}, hook: deferSecondCall,
			tool: "act", first: it, second: it, want: "blocked"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "clock", c.allowed)
			// The skill tool itself is allowed under every policy here.
			c.policy.Allow = append(c.policy.Allow, rules(agentskill.ToolName)...)
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
			var hooks []agentkit.Option
			if c.hook != nil {
				hooks = append(hooks, agentkit.WithBeforeToolCall(c.hook))
			}
			kit, err := agentkit.New(t.Context(), append(hooks,
				agentkit.WithModel(model, "m"),
				agentkit.WithSkills(skills),
				agentkit.WithTools(tool("act"), tool("other"), tool("bash")),
				agentkit.WithPolicy(c.policy, c.matchers, c.opts...),
				agentkit.WithSkillGrants(func(sk *agentskill.Skill) agentpolicy.Source {
					return agentpolicy.Source{Name: "agentskill:" + sk.ListedName(), Path: sk.Location, Trusted: true}
				}),
				agentkit.WithSkillGrantScope(),
				agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}),
			)...)
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
				// The engine records the hook's decision as the call's
				// verdict, once.
				recorded := 0
				for _, e := range customEntries(openSession(t, sessions, kit.SessionID()), agentpolicy.VerdictNS) {
					if strings.Contains(string(e.Data), "skill clock granted ended") {
						recorded++
					}
				}
				if recorded != 1 {
					t.Fatalf("the session has %d verdicts for the refusal, want one", recorded)
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

// The kit asks the engine what it would decide, Engine.Would, which
// folds the product's hooks in as the decision does, and its contract is
// that the question is no decision: a hook's answer reached that way is
// not observed as a verdict and nothing is remembered as held for it. The
// hook is called once more for the question, the call's own verdict is
// recorded once, and the one deferral the engine holds is the call's own.
func TestTheKitsQuestionToTheEngineIsNoDecision(t *testing.T) {
	for _, c := range []struct {
		name string
		// ask is whether an ask rule names the tool, so the kit's refusal
		// answers the call before the hook the engine folds in after it.
		ask       bool
		wantHook  int  // calls of the hook for the second call
		wantHeld  bool // the engine holds the call as deferred
		wantEnded agentturn.Reason
	}{
		{name: "an ask rule is behind the verdict", ask: true, wantHook: 1, wantHeld: false, wantEnded: agentturn.ReasonDone},
		{name: "a hook's question is the only one", ask: false, wantHook: 2, wantHeld: true, wantEnded: agentturn.ReasonInputRequired},
	} {
		t.Run(c.name, func(t *testing.T) {
			skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "clock", "act")
			var (
				hookCalls int
				verdicts  = map[string]int{}
			)
			asks := func(_ context.Context, info agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
				if info.Call != nil && info.Call.CallID == "call-second" {
					hookCalls++
					return &agentturn.ToolDecision{Action: agentturn.Defer, Reason: "the product wants to know"}, nil
				}
				return nil, nil
			}
			act := agenttool.New("act", "a tool", func(context.Context, agenttool.NoArgs) (string, error) { return "ran", nil })
			rules, err := agentpolicy.ParseRules("act " + agentskill.ToolName)
			if err != nil {
				t.Fatal(err)
			}
			p := agentpolicy.Policy{Allow: rules, Default: agentpolicy.Allow()}
			if c.ask {
				p = agentpolicy.Policy{Ask: rules[:1], Allow: rules[1:], Default: agentpolicy.Allow()}
			}
			model := &scriptModel{turns: []func(*openresponses.Emitter) error{
				callTurn(agentskill.ToolName, `{"name":"clock"}`),
				callTurnID("call-first", "act", `{}`),
				textTurn("done it"),
				callTurnID("call-second", "act", `{}`),
			}}
			kit, err := agentkit.New(t.Context(),
				agentkit.WithModel(model, "m"),
				agentkit.WithSkills(skills),
				agentkit.WithTools(act),
				agentkit.WithPolicy(p, nil),
				agentkit.WithBeforeToolCall(asks),
				agentkit.WithVerdictObserver(func(_ context.Context, v agentpolicy.Verdict) { verdicts[v.CallID]++ }),
				agentkit.WithSkillGrants(trustedSkills),
				agentkit.WithSkillGrantScope(),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer kit.Close()
			agent := agentturn.New(kit.Config())
			if _, err := agent.Prompt(t.Context(), openresponses.UserText("use the skill")); err != nil {
				t.Fatal(err)
			}
			end, err := agent.Prompt(t.Context(), openresponses.UserText("act again"))
			if err != nil {
				t.Fatal(err)
			}
			if end.Reason != c.wantEnded {
				t.Fatalf("run ended %q, want %q", end.Reason, c.wantEnded)
			}
			// Where no ask rule is behind the question, the product's hook
			// is respected: it defers the call, and the kit refuses nothing.
			if c.wantHeld && len(outputsSaying(agent.State().Transcript, "ended with the user's last message")) != 0 {
				t.Fatal("the kit answered a hook's question with a refusal naming a skill")
			}
			if hookCalls != c.wantHook {
				t.Fatalf("the product's hook decided the call %d times, want %d: once for the kit's question when the fold goes on to it", hookCalls, c.wantHook)
			}
			if verdicts["call-second"] != 1 {
				t.Fatalf("the call has %d verdicts, want one, the decision's own; the question records none", verdicts["call-second"])
			}
			if _, held := kit.Engine().Deferred(end.RunID, "call-second"); held != c.wantHeld {
				t.Fatalf("the engine holds the call as deferred = %v, want %v; the question remembers nothing", held, c.wantHeld)
			}
		})
	}
}

// The refusal is a hook inside the engine, not a decision ahead of it:
// chained ahead, its Block left the batch's other calls held for an ask
// nobody was asked, since the engine read the blocked sibling as asking.
// Inside, the engine's fold takes the Block and the allowed sibling runs.
func TestARefusalInABatchDoesNotHoldItsSiblings(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "clock", "act")
	ask, err := agentpolicy.ParseRules("act")
	if err != nil {
		t.Fatal(err)
	}
	allow, err := agentpolicy.ParseRules(agentskill.ToolName + " other")
	if err != nil {
		t.Fatal(err)
	}
	sessions := agentsession.NewMemoryStore()
	runs := map[string]int{}
	tool := func(name string) agenttool.Tool {
		return agenttool.New(name, "a tool", func(context.Context, struct {
			What string `json:"what,omitempty"`
		}) (string, error) {
			runs[name]++
			return name + " ran", nil
		})
	}
	both := func(e *openresponses.Emitter) error {
		if err := callTurnID("call-act", "act", `{"what":"it"}`)(e); err != nil {
			return err
		}
		return callTurnID("call-other", "other", `{"what":"it"}`)(e)
	}
	model := &scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn(agentskill.ToolName, `{"name":"clock"}`),
		callTurnID("call-first", "act", `{"what":"it"}`),
		textTurn("done it"),
		both,
		textTurn("done again"),
	}}
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "m"),
		agentkit.WithSkills(skills),
		agentkit.WithTools(tool("act"), tool("other")),
		agentkit.WithPolicy(agentpolicy.Policy{Ask: ask, Allow: allow, Default: agentpolicy.Allow()}, nil),
		agentkit.WithSkillGrants(trustedSkills),
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
	end, err := agent.Prompt(t.Context(), openresponses.UserText("act again, and the other"))
	if err != nil {
		t.Fatal(err)
	}
	refused := outputsSaying(agent.State().Transcript, "ended with the user's last message")
	if end.Reason != agentturn.ReasonDone || len(end.Pending) != 0 || runs["other"] != 1 || runs["act"] != 1 || len(refused) != 1 {
		t.Fatalf("run ended %q with %d pending, runs %v, refusals %q; want done, the other run beside the refused act", end.Reason, len(end.Pending), runs, refused)
	}
}

// A replayed grant is bound to the transcript the agent is seeded with,
// session.Transcript, not to the path's items: a fold's summary stands
// in there for the messages it folded, as a user-role message of its
// own, so after a restart over a session whose folds reached past the
// last prompt the path's digest would read as stale and the restart's
// first turn would end the grant with "the user's new message" though
// none arrived. A compaction ends nothing by itself across a restart
// either.
func TestAReplayedGrantSurvivesARestartAfterAFold(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "release", "Bash(git:*)")
	sessions := agentsession.NewMemoryStore()
	var ran []string
	bash := agenttool.New("Bash", "run a command",
		func(_ context.Context, in struct {
			Command string `json:"command"`
		}) (string, error) {
			ran = append(ran, in.Command)
			return strings.Repeat("output ", 600), nil
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
			agentkit.WithCompaction(1500),
			agentkit.WithCompactionModel(&scriptModel{}),
			sess,
		)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = kit.Close() })
		return kit
	}
	turns := []func(*openresponses.Emitter) error{callTurn(agentskill.ToolName, `{"name":"release"}`)}
	for i := range 12 {
		turns = append(turns, callTurnID(fmt.Sprintf("call-%d", i), "Bash", `{"command":"git status"}`))
	}
	turns = append(turns, callTurnID("call-rm", "Bash", `{"command":"rm -rf build"}`))
	first := build(&scriptModel{turns: turns}, agentkit.WithSession(sessions, agentsession.Header{CWD: t.TempDir()}))
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
	sess := openSession(t, sessions, first.SessionID())
	folds := 0
	for _, e := range sess.Path(sess.Leaf()) {
		if _, ok := e.(*agentsession.CompactionEntry); ok {
			folds++
		}
	}
	if folds == 0 {
		t.Fatal("no fold was recorded; the test needs the outputs to exceed the budget")
	}
	seeded, err := session.Transcript(sess)
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range seeded {
		if m, ok := it.(*openresponses.Message); ok && m.Role == openresponses.RoleUser && m.Text() == "cut the release" {
			t.Fatal("the seeded transcript still holds the prompt; the fold did not reach past it")
		}
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
		t.Errorf("grants after the Resume = %d, want the replayed grant kept: no user message arrived", got)
	}
	if end.Reason == agentturn.ReasonInputRequired || ran[len(ran)-1] != "git log" {
		t.Errorf("git log did not run under the grant: ended %q, ran %v", end.Reason, ran[max(0, len(ran)-3):])
	}
	for _, item := range resumed.State().Transcript {
		if m, ok := item.(*openresponses.Message); ok && m.Role == openresponses.RoleDeveloper && strings.Contains(m.Text(), "ended with the user's new message") {
			t.Errorf("the restart's first turn was told a grant ended: %q", m.Text())
		}
	}
}

// An ended grant is kept by the source it was made under and forgotten
// when the catalogue no longer lists its skill, at ReloadSkills: a skill
// deleted after its grant ended could be read again under no name, so
// its ended grant would have refused its tool for the rest of the
// conversation. The engine asks instead, as it does for any call.
func TestAnEndedGrantOfASkillNoLongerListedRefusesNothing(t *testing.T) {
	skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "clock", "act")
	ask, err := agentpolicy.ParseRules("act")
	if err != nil {
		t.Fatal(err)
	}
	allow, err := agentpolicy.ParseRules(agentskill.ToolName)
	if err != nil {
		t.Fatal(err)
	}
	runs := 0
	act := agenttool.New("act", "a tool", func(context.Context, struct {
		What string `json:"what,omitempty"`
	}) (string, error) {
		runs++
		return "act ran", nil
	})
	model := &scriptModel{turns: []func(*openresponses.Emitter) error{
		callTurn(agentskill.ToolName, `{"name":"clock"}`),
		callTurnID("call-first", "act", `{"what":"it"}`),
		textTurn("done it"),
		textTurn("noted"),
		callTurnID("call-third", "act", `{"what":"it"}`),
	}}
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(model, "m"),
		agentkit.WithSkills(skills),
		agentkit.WithTools(act),
		agentkit.WithPolicy(agentpolicy.Policy{Ask: ask, Allow: allow, Default: agentpolicy.Allow()}, nil),
		agentkit.WithSkillGrants(trustedSkills),
		agentkit.WithSkillGrantScope(),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer kit.Close()
	agent := agentturn.New(kit.Config())
	for _, text := range []string{"use the skill", "thanks"} {
		if _, err := agent.Prompt(t.Context(), openresponses.UserText(text)); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(kit.Engine().Grants()); got != 0 {
		t.Fatalf("grants after the second message = %d, want the scope to have ended the skill's", got)
	}
	if err := os.RemoveAll(filepath.Join(skills, "clock")); err != nil {
		t.Fatal(err)
	}
	if err := kit.ReloadSkills(t.Context()); err != nil {
		t.Fatal(err)
	}
	end, err := agent.Prompt(t.Context(), openresponses.UserText("act once more"))
	if err != nil {
		t.Fatal(err)
	}
	refused := outputsSaying(agent.State().Transcript, "ended with the user's last message")
	if end.Reason != agentturn.ReasonInputRequired || len(end.Pending) != 1 || len(refused) != 0 || runs != 1 {
		t.Fatalf("run ended %q with %d pending, refusals %q, act ran %d times; want the engine's ask for a skill no longer listed", end.Reason, len(end.Pending), refused, runs)
	}
}

// A skill whose allowed-tools were emptied or broken and reloaded: the
// read the refusal asks for grants nothing, and the ended grant was
// cleared only by a grant, so the call was refused again, forever. The
// read itself clears it now, whatever it grants, and the engine asks as
// it would for any skill with no rules. (#76)
func TestAReadThatGrantsNothingStillEndsTheRefusal(t *testing.T) {
	for _, edited := range []string{"", "Bash(("} {
		t.Run("allowed-tools "+edited, func(t *testing.T) {
			skills := skillWithTools(t, filepath.Join(t.TempDir(), "skills"), "clock", "act")
			ask, _ := agentpolicy.ParseRules("act")
			allow, _ := agentpolicy.ParseRules(agentskill.ToolName)
			runs := 0
			act := agenttool.New("act", "a tool", func(context.Context, struct {
				What string `json:"what,omitempty"`
			}) (string, error) {
				runs++
				return "act ran", nil
			})
			model := &scriptModel{turns: []func(*openresponses.Emitter) error{
				callTurn(agentskill.ToolName, `{"name":"clock"}`),
				callTurnID("call-first", "act", `{"what":"it"}`),
				textTurn("done it"),
				textTurn("noted"),
				callTurnID("call-reread", agentskill.ToolName, `{"name":"clock"}`),
				callTurnID("call-third", "act", `{"what":"it"}`),
			}}
			kit, err := agentkit.New(t.Context(),
				agentkit.WithModel(model, "m"),
				agentkit.WithSkills(skills),
				agentkit.WithTools(act),
				agentkit.WithPolicy(agentpolicy.Policy{Ask: ask, Allow: allow, Default: agentpolicy.Allow()}, nil),
				agentkit.WithSkillGrants(trustedSkills),
				agentkit.WithSkillGrantScope(),
			)
			if err != nil {
				t.Fatal(err)
			}
			defer kit.Close()
			agent := agentturn.New(kit.Config())
			for _, text := range []string{"use the skill", "thanks"} {
				if _, err := agent.Prompt(t.Context(), openresponses.UserText(text)); err != nil {
					t.Fatal(err)
				}
			}
			// The skill is edited: its allowed-tools are gone (or broken).
			body := "---\nname: clock\ndescription: what clock is for\n---\n\ndo the thing\n"
			if edited != "" {
				body = "---\nname: clock\ndescription: what clock is for\nallowed-tools: " + edited + "\n---\n\ndo the thing\n"
			}
			if err := os.WriteFile(filepath.Join(skills, "clock", "SKILL.md"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := kit.ReloadSkills(t.Context()); err != nil {
				t.Fatal(err)
			}
			end, err := agent.Prompt(t.Context(), openresponses.UserText("act once more"))
			if err != nil {
				t.Fatal(err)
			}
			refused := outputsSaying(agent.State().Transcript, "ended with the user's last message")
			if end.Reason != agentturn.ReasonInputRequired || len(refused) != 0 || runs != 1 {
				t.Fatalf("run ended %q, refusals %q, act ran %d; want the engine's ask after the skill was read again and grants nothing", end.Reason, refused, runs)
			}
		})
	}
}
