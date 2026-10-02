package agentkit

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agentsmd"
	"github.com/ChristopherDavenport/openresponses"
)

// Part is one named block of the instructions: its stable id, its text
// and the layer that produced it. It is [agentsession.InstructionPart]
// itself, which is the shape a session's instructions_parts wants, so a
// product hands [Kit.Parts] straight to
// [agentsession.ConfigFromRequestParts].
type Part = agentsession.InstructionPart

// The identifiers of the parts the kit assembles. A part's id is stable
// across a session, so a config delta can name a part it does not
// repeat. The AGENTS.md part's id is [agentsmd.PartID], which that
// module owns.
const (
	// PartProduct is the product's own prompt, from [WithInstructions].
	PartProduct = "product"
	// PartSkills is the skill catalogue, from [WithSkills].
	PartSkills = "skills"
	// PartMemory is the memory block, from [WithMemory]. The block is
	// several parts, one per piece [agentmemory.RenderParts] returns,
	// and PartMemory is both the ID of the first, the block's title, and
	// the ID [WithOrder] places the whole group by. Every part of the
	// group has an ID that is PartMemory or starts with "memory/" or
	// "memory:".
	PartMemory = "memory"
	// PartMemoryUsage is the last part of the memory group:
	// [agentmemory.Usage], the paragraph that tells the model what the
	// block is and which tool makes which change.
	PartMemoryUsage = "memory:usage"
)

// The Source of each part: the library that produced the text, in the
// harness's own terms, as [agentsession.InstructionPart] asks for.
const (
	SourceProduct  = "product"
	SourceSkills   = "agentskill"
	SourceMemory   = "agentmemory"
	SourceAgentsMD = "agentsmd"
)

// Separator joins the parts, and is [agentsession.PartSeparator]: the
// session format's own, so the joined text of [Kit.Parts] is the
// instructions the model was sent and the request hash covers.
const Separator = agentsession.PartSeparator

// DefaultOrder is the order the parts are joined in, unless [WithOrder]
// says otherwise. The argument for each position is in
// docs/ordering.md; the short form is that later text overrides
// earlier, so the most specific text goes last.
var DefaultOrder = []string{PartProduct, PartSkills, PartMemory, agentsmd.PartID}

// Omission is one thing a layer considered for the instructions and
// left out. The layers each report their own; the kit returns them as
// one list, because a product wants one list and a session wants one
// instructions_omitted.
type Omission struct {
	// Part is the id of the part the omission belongs to.
	Part string
	// Source is the library that reported it.
	Source string
	// What names the thing left out in its layer's own stable key: an
	// absolute path for a file, the part ID an entry would have had,
	// [agentmemory.PartID], for a memory entry, a location for a skill.
	What string
	// Reason is the layer's own word for why.
	Reason string
	// Size is the bytes the thing would have added, zero when the
	// layer does not say.
	Size int64
	// By names what took its place, for an omission that is a
	// shadowing rather than a bound.
	By string
}

// OmittedPart is the omission as the session format records it.
func (o Omission) OmittedPart() agentsession.OmittedPart {
	return agentsession.OmittedPart{
		ID:     o.What,
		Reason: o.Reason,
		Size:   int(o.Size),
		Source: o.Source,
	}
}

func (o Omission) String() string {
	s := fmt.Sprintf("%s: %s (%s)", o.Source, o.What, o.Reason)
	if o.By != "" {
		s += " by " + o.By
	}
	return s
}

// join puts the groups of parts in order and returns them, with the
// text they join to. An empty group is left out; the memory group is
// the only one with more than one part.
func join(order []string, byID map[string][]Part) (string, []Part) {
	var kept []Part
	for _, id := range order {
		kept = append(kept, byID[id]...)
	}
	return agentsession.JoinInstructions(kept), kept
}

// group is the ID [WithOrder] places a part by: [PartMemory] for every
// part of the memory block, and the part's own ID for the rest.
func group(id string) string {
	if id == PartMemory || strings.HasPrefix(id, PartMemory+"/") || strings.HasPrefix(id, PartMemory+":") {
		return PartMemory
	}
	return id
}

// single is the group of one part, or none when the part is empty.
func single(p Part) []Part {
	if p.Text == "" {
		return nil
	}
	return []Part{p}
}

// checkOrder reports whether every configured part appears exactly once
// in the order the caller gave.
func checkOrder(order []string, configured map[string]bool) error {
	seen := map[string]int{}
	for _, id := range order {
		seen[id]++
	}
	for id, n := range seen {
		if n > 1 {
			return fmt.Errorf("agentkit: instruction order names %q %d times", id, n)
		}
	}
	for id := range configured {
		if seen[id] == 0 {
			return fmt.Errorf("agentkit: instruction order omits the configured part %q", id)
		}
	}
	return nil
}

// skillPart discovers the skills and renders the catalogue. The usage
// paragraph is appended only when the catalogue's tool is offered,
// since it tells the model to reach the skills through that tool. It
// returns a nil catalogue when no source is left, every directory
// having been optional and absent.
func skillPart(s *settings, withTool bool) (*agentskill.Catalog, Part, []Omission, error) {
	sources := make([]agentskill.Source, 0, len(s.skillDirs)+len(s.skillSources))
	for _, dir := range s.skillDirs {
		src, err := agentskill.Dir(dir.path)
		if dir.optional && errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, Part{}, nil, fmt.Errorf("agentkit: skills %s: %w", dir.path, err)
		}
		sources = append(sources, src)
	}
	sources = append(sources, s.skillSources...)
	if len(sources) == 0 {
		// Every directory was optional and none is there: no catalogue,
		// so no part, no usage paragraph and no tool that serves nothing.
		return nil, Part{}, nil, nil
	}

	cat, err := agentskill.Discover(sources...)
	if err != nil {
		return nil, Part{}, nil, fmt.Errorf("agentkit: discovering skills: %w", err)
	}

	text := cat.Prompt()
	if withTool && text != "" {
		text += Separator + cat.Usage()
	}

	listed := map[*agentskill.Skill]bool{}
	for _, sk := range cat.Listed() {
		listed[sk] = true
	}
	var omitted []Omission
	for _, sk := range cat.Skills {
		if listed[sk] {
			continue
		}
		omitted = append(omitted, Omission{
			Part:   PartSkills,
			Source: SourceSkills,
			What:   skillKey(sk),
			Reason: unlistedReason(sk, cat.Problems[sk.Location]),
		})
	}
	// Discover names the skill that holds the name a shadowed one
	// wanted, qualified or not, so By is agentskill's answer.
	for _, sk := range cat.Shadowed {
		omitted = append(omitted, Omission{
			Part:   PartSkills,
			Source: SourceSkills,
			What:   skillKey(sk),
			Reason: "shadowed",
			By:     sk.ShadowedBy,
		})
	}
	return cat, Part{ID: PartSkills, Text: text, Source: SourceSkills}, omitted, nil
}

// skillKey names a skill by its location, which is the stable key: a
// name is what a search across sources resolved to, and a skill left
// out may have no name at all.
func skillKey(sk *agentskill.Skill) string {
	if sk.Location != "" {
		return sk.Location
	}
	if sk.Name != "" {
		return sk.Name
	}
	return sk.DirName
}

// unlistedReason says why a loaded skill is not offered, in the
// validator's own words where it has them.
func unlistedReason(sk *agentskill.Skill, problems []agentskill.Problem) string {
	for _, p := range problems {
		if p.Severity == agentskill.Error {
			return p.String()
		}
	}
	switch {
	case sk.Name == "":
		return "no name"
	case sk.Description == "":
		return "no description"
	}
	return "not listed"
}

// memoryPart renders the memory block as parts, one per piece
// [agentmemory.RenderParts] returns, followed by [agentmemory.Usage] as
// [PartMemoryUsage], bounded to limit bytes, the whole group joined,
// when limit is positive. A limit of zero leaves the layer its own
// bound; a negative limit drops the group, since a block the budget
// cannot hold at all is better reported than sent short. The group
// joins to the block [agentmemory.Render] returns and the usage
// paragraph, so the joined instructions are the same text either way;
// what the parts buy is a record in which a write to one entry is that
// entry's part and the summary.
//
// The usage paragraph is always appended to a block that is sent,
// because the memory tools are offered whenever the block is, and it
// is paid for out of the limit first: it cannot be bounded, and a
// block without it leaves the model the tools and no word on them. A
// block the budget drops takes the paragraph with it, as the skill
// catalogue's usage goes with the catalogue, and dropped is true: the
// tools that write memory are then withheld from the request and
// refused, [memoryWrites], since a model shown no block and no word on
// them would save over entries it was never shown. memory_search stays.
// The dropped render's manifest shows no entry and lists every entry
// the block held among the omitted with no reason, which is the mark
// [droppedRender] reads in a session's record after a restart.
func memoryPart(ctx context.Context, s *settings, limit int64) (group []Part, man agentmemory.Manifest, omitted []Omission, dropped bool, err error) {
	usage := agentmemory.Usage()
	if limit > 0 {
		if limit -= int64(len(Separator) + len(usage)); limit <= 0 {
			limit = -1
		}
	}
	opts := s.memRender
	scopes := s.renderScopes()
	parts, man, err := agentmemory.RenderParts(ctx, s.memStore, scopes, opts...)
	if err != nil {
		return nil, man, nil, false, fmt.Errorf("agentkit: rendering memory: %w", err)
	}
	if limit > 0 && int64(len(agentmemory.JoinParts(parts))) > limit {
		bounded := append(append([]agentmemory.RenderOption(nil), opts...),
			agentmemory.WithMaxTotalBytes(int(limit)))
		bp, bm, err := agentmemory.RenderParts(ctx, s.memStore, scopes, bounded...)
		switch {
		case errors.Is(err, agentmemory.ErrBudget):
			// The share is under the block's floor. The unbounded
			// render's manifest names what the drop below omits.
			limit = -1
		case err != nil:
			return nil, bm, nil, false, fmt.Errorf("agentkit: rendering memory: %w", err)
		default:
			parts, man = bp, bm
		}
	}
	size := int64(len(agentmemory.JoinParts(parts)))
	if limit < 0 || (limit > 0 && size > limit) {
		// The block has a floor it cannot go under, a header and one
		// heading per scope, so a share below that floor buys nothing.
		// Report every entry rather than send a block the budget said
		// there was no room for.
		parts, dropped = nil, true
		man.Omitted = append(man.Omitted, man.Entries...)
		man.Entries = nil
	}

	if size > 0 && len(parts) > 0 {
		group = make([]Part, 0, len(parts)+1)
		for _, p := range parts {
			group = append(group, Part{ID: p.ID, Text: p.Text, Source: SourceMemory})
		}
		group = append(group, Part{ID: PartMemoryUsage, Text: usage, Source: SourceMemory})
	}

	omitted = make([]Omission, 0, len(man.Omitted))
	for _, e := range man.Omitted {
		reason := e.Reason
		if reason == "" {
			reason = agentmemory.OmitBudget
		}
		omitted = append(omitted, Omission{
			Part:   PartMemory,
			Source: SourceMemory,
			What:   agentmemory.PartID(e.Scope, e.Name),
			Reason: reason,
			Size:   int64(e.Bytes),
		})
	}
	return group, man, omitted, dropped, nil
}

// memoryWrites are the memory tools that write: withheld from a request
// whose render the budget dropped, and refused in its run.
var memoryWrites = map[string]bool{
	agentmemory.SaveTool:   true,
	agentmemory.PatchTool:  true,
	agentmemory.ForgetTool: true,
}

// withoutMemoryWrites returns tools without the [memoryWrites], tools
// itself when it holds none.
func withoutMemoryWrites(tools openresponses.Tools) openresponses.Tools {
	if !slices.ContainsFunc(tools, isMemoryWrite) {
		return tools
	}
	return slices.DeleteFunc(slices.Clone(tools), isMemoryWrite)
}

func isMemoryWrite(t openresponses.Tool) bool {
	f, ok := t.(*openresponses.FunctionTool)
	return ok && memoryWrites[f.Name]
}

// errMemoryDropped is what a memory write gets in a run whose render the
// instruction budget dropped: the model was shown no block, so a write
// could replace an entry it never saw.
var errMemoryDropped = errors.New("the memory block is not shown this turn, because the instruction budget has no room for it, so memory cannot be changed: a write could replace an entry you have not seen; " + agentmemory.SearchTool + " still reads it")

// agentsMDPart reads the AGENTS.md chain and renders it, bounded to
// limit bytes when limit is positive.
//
// [agentsmd.Options.Budget] bounds the bytes of the files, and the
// rendered part is those bytes plus the wrapper agentsmd puts around
// each one. So the bound handed to a second chain is the share less the
// wrapper the first chain's files cost. That is safe without a third
// pass: a smaller budget can only select a prefix of the same files, so
// the wrapper can only shrink, and the rendered part comes out at or
// under the share.
func agentsMDPart(s *settings, limit int64) (Part, []Omission, error) {
	res, err := agentsmd.Chain(s.agentsMDPath, s.agentsMD)
	if err != nil {
		return Part{}, nil, fmt.Errorf("agentkit: reading the AGENTS.md chain: %w", err)
	}
	text := agentsmd.Render(res.Files)

	if limit > 0 && int64(len(text)) > limit {
		wrapper := int64(len(text))
		for _, f := range res.Files {
			wrapper -= int64(len(f.Content))
		}
		opts := s.agentsMD
		opts.Budget = limit - wrapper
		if opts.Budget > 0 {
			if res, err = agentsmd.Chain(s.agentsMDPath, opts); err != nil {
				return Part{}, nil, fmt.Errorf("agentkit: reading the AGENTS.md chain: %w", err)
			}
			text = agentsmd.Render(res.Files)
		}
	}
	if limit < 0 || (limit > 0 && int64(len(text)) > limit) {
		for _, f := range res.Files {
			res.Omitted = append(res.Omitted, agentsmd.Omitted{
				Path:   f.Path,
				Size:   int64(len(f.Content)),
				Reason: agentsmd.OverBudget,
			})
		}
		res.Files, text = nil, ""
	}

	omitted := make([]Omission, 0, len(res.Omitted))
	for _, o := range res.Omitted {
		omitted = append(omitted, Omission{
			Part:   agentsmd.PartID,
			Source: SourceAgentsMD,
			What:   o.Path,
			Reason: o.Reason.String(),
			Size:   o.Size,
			By:     o.By,
		})
	}
	return Part{ID: agentsmd.PartID, Text: text, Source: SourceAgentsMD}, omitted, nil
}
