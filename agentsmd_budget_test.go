package agentkit_test

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentkit"
	"github.com/ChristopherDavenport/agentsmd"
)

// lines is n lines of the form "<word> line NN\n".
func lines(word string, n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "%s line %02d\n", word, i)
	}
	return b.String()
}

// agentsMDPartOf is the kit's AGENTS.md part, empty when it was not
// joined.
func agentsMDPartOf(kit *agentkit.Kit) string {
	for _, p := range kit.Parts() {
		if p.ID == agentsmd.PartID {
			return p.Text
		}
	}
	return ""
}

// agentsMDOmitted is the kit's omissions from agentsmd.
func agentsMDOmitted(kit *agentkit.Kit) []agentkit.Omission {
	var om []agentkit.Omission
	for _, o := range kit.Omitted() {
		if o.Source == agentkit.SourceAgentsMD {
			om = append(om, o)
		}
	}
	return om
}

// kitShare is a kit whose only parts are a prompt and the AGENTS.md
// chain at dir, under a budget that leaves the chain share bytes.
func kitShare(t *testing.T, dir string, opts agentsmd.Options, share int64) *agentkit.Kit {
	t.Helper()
	const prompt = "be brief"
	budget := int64(len(prompt)+len(agentkit.Separator)) + share
	kit, err := agentkit.New(t.Context(),
		agentkit.WithModel(stubModel{}, "m"),
		agentkit.WithInstructions(prompt),
		agentkit.WithAgentsMD(dir, opts),
		agentkit.WithInstructionBudget(budget),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { kit.Close() })
	if n := int64(len(kit.Config().Instructions)); n > budget {
		t.Fatalf("instructions are %d bytes, over the %d-byte budget", n, budget)
	}
	return kit
}

// A share that binds is agentsmd's RenderedBudget, which keeps the
// longest run of whole files that renders within it. The share less
// the first chain's wrapper, as the kit used to hand on, would have
// paid for the wrapper of a file the share leaves out and dropped the
// middle file too.
func TestABindingShareKeepsTheLongestPrefixThatRendersWithinIt(t *testing.T) {
	root := t.TempDir()
	a := writeFile(t, filepath.Join(root, "AGENTS.md"), lines("root", 8))
	b := writeFile(t, filepath.Join(root, "x", "AGENTS.md"), lines("mid", 8))
	c := writeFile(t, filepath.Join(root, "x", "y", "AGENTS.md"), "near\n")
	dir := filepath.Join(root, "x", "y")

	two := agentsmd.Render([]agentsmd.File{{Path: a, Content: lines("root", 8)}, {Path: b, Content: lines("mid", 8)}})
	three := agentsmd.Render([]agentsmd.File{{Path: a, Content: lines("root", 8)}, {Path: b, Content: lines("mid", 8)}, {Path: c, Content: "near\n"}})
	share := int64(len(two) + 5)
	if share >= int64(len(three)) {
		t.Fatalf("the fixture's share %d holds all three files (%d)", share, len(three))
	}

	kit := kitShare(t, dir, agentsmd.Options{Root: root}, share)
	if got := agentsMDPartOf(kit); got != two {
		t.Fatalf("the %s part:\n%s\nwant the first two files whole:\n%s", agentsmd.PartID, got, two)
	}
	want := []agentkit.Omission{{Part: agentsmd.PartID, Source: agentkit.SourceAgentsMD, What: c, Reason: "over budget", Size: 5}}
	if got := agentsMDOmitted(kit); !slices.Equal(got, want) {
		t.Fatalf("omitted = %+v, want %+v", got, want)
	}
}

// Under Truncate, a share that binds cuts the first file over it at a
// line, against the rendered size, and the part is what agentsmd.Chain
// gives by hand with the share as RenderedBudget: the manual path. The
// cut file is reported once, truncated, with the bytes cut off.
func TestABindingShareCutsAFileUnderTruncate(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "AGENTS.md"), lines("root", 4))
	sub := writeFile(t, filepath.Join(root, "sub", "AGENTS.md"), lines("sub", 200))
	near := writeFile(t, filepath.Join(root, "sub", "deep", "AGENTS.md"), "near\n")
	dir := filepath.Join(root, "sub", "deep")
	opts := agentsmd.Options{Root: root, Truncate: true}

	whole, err := agentsmd.Chain(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	share := int64(len(agentsmd.Render(whole.Files)) / 2)

	kit := kitShare(t, dir, opts, share)

	byHand := opts
	byHand.RenderedBudget = share
	chain, err := agentsmd.Chain(dir, byHand)
	if err != nil {
		t.Fatal(err)
	}
	got := agentsMDPartOf(kit)
	if want := agentsmd.Render(chain.Files); got != want {
		t.Fatalf("the %s part:\n%s\nagentsmd.Chain by hand:\n%s", agentsmd.PartID, got, want)
	}
	if int64(len(got)) > share {
		t.Fatalf("the part is %d bytes, over the %d-byte share", len(got), share)
	}
	for _, want := range []string{"root line 03\n", "sub line 00\n", "bytes of " + sub + " left out over the budget]"} {
		if !strings.Contains(got, want) {
			t.Errorf("the part does not hold %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "sub line 199") {
		t.Errorf("the part holds the cut file's last line:\n%s", got)
	}

	om := agentsMDOmitted(kit)
	if len(om) != 2 {
		t.Fatalf("omitted = %+v, want the cut file and the file after it", om)
	}
	cut, after := om[0], om[1]
	kept := strings.Index(got, "[agentsmd: ") - strings.Index(got, "sub line 00\n")
	if cut.What != sub || cut.Reason != "truncated" || cut.Size != int64(len(lines("sub", 200))-kept) {
		t.Errorf("cut = %+v, want %s truncated with %d bytes cut off", cut, sub, len(lines("sub", 200))-kept)
	}
	if after.What != near || after.Reason != "over budget" || after.Size != 5 {
		t.Errorf("after = %+v, want %s over budget, 5 bytes", after, near)
	}
}

// A share with no room leaves the part out. A file the caller's own
// Budget would have cut under Truncate sends nothing, so it is reported
// once, over budget, with its whole size: not truncated with the bytes
// cut off, and not with the cut part's size, marker and all.
func TestAShareWithNoRoomReportsACutFileWhole(t *testing.T) {
	root := t.TempDir()
	first := writeFile(t, filepath.Join(root, "AGENTS.md"), lines("root", 2))
	big := writeFile(t, filepath.Join(root, "sub", "AGENTS.md"), lines("sub", 40))
	near := writeFile(t, filepath.Join(root, "sub", "deep", "AGENTS.md"), "near\n")
	dir := filepath.Join(root, "sub", "deep")
	opts := agentsmd.Options{Root: root, Truncate: true, Budget: 200}

	// The fixture: the caller's options alone cut the middle file.
	alone, err := agentsmd.Chain(dir, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(alone.Omitted, func(o agentsmd.Omitted) bool { return o.Reason == agentsmd.Truncated }) {
		t.Fatalf("the caller's options cut nothing: %+v", alone.Omitted)
	}

	kit := kitShare(t, dir, opts, 0)
	if got := kit.Config().Instructions; got != "be brief" {
		t.Fatalf("instructions = %q, want only the prompt", got)
	}
	want := []agentkit.Omission{
		{Part: agentsmd.PartID, Source: agentkit.SourceAgentsMD, What: big, Reason: "over budget", Size: int64(len(lines("sub", 40)))},
		{Part: agentsmd.PartID, Source: agentkit.SourceAgentsMD, What: near, Reason: "over budget", Size: 5},
		{Part: agentsmd.PartID, Source: agentkit.SourceAgentsMD, What: first, Reason: "over budget", Size: int64(len(lines("root", 2)))},
	}
	if got := agentsMDOmitted(kit); !slices.Equal(got, want) {
		t.Fatalf("omitted = %+v\nwant %+v", got, want)
	}
}
