package agentkit

import (
	"context"
	"fmt"
	"strings"

	"github.com/ChristopherDavenport/agentmemory"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentskill"
	"github.com/ChristopherDavenport/agentsmd"
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
	// PartMemory is the memory block, from [WithMemory].
	PartMemory = "memory"
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
	// absolute path for a file, "scope/name" for a memory entry, a
	// location for a skill.
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

// join renders the parts in order, dropping the empty ones, and returns
// both the text and the parts that carried it.
func join(order []string, byID map[string]Part) (string, []Part) {
	var kept []Part
	var texts []string
	for _, id := range order {
		p, ok := byID[id]
		if !ok || p.Text == "" {
			continue
		}
		kept = append(kept, p)
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, Separator), kept
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
// since it tells the model to reach the skills through that tool.
func skillPart(s *settings, withTool bool) (*agentskill.Catalog, Part, []Omission, error) {
	sources := make([]agentskill.Source, 0, len(s.skillDirs)+len(s.skillSources))
	for _, dir := range s.skillDirs {
		src, err := agentskill.Dir(dir)
		if err != nil {
			return nil, Part{}, nil, fmt.Errorf("agentkit: skills %s: %w", dir, err)
		}
		sources = append(sources, src)
	}
	sources = append(sources, s.skillSources...)

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
	winner := map[string]string{}
	for _, sk := range cat.Skills {
		if sk.Name != "" {
			winner[sk.Name] = skillKey(sk)
		}
	}
	for _, sk := range cat.Shadowed {
		omitted = append(omitted, Omission{
			Part:   PartSkills,
			Source: SourceSkills,
			What:   skillKey(sk),
			Reason: "shadowed",
			By:     winner[sk.Name],
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

// memoryPart renders the memory block, bounded to limit bytes when
// limit is positive. A limit of zero leaves the layer its own bound; a
// negative limit drops the part, since a block the budget cannot hold
// at all is better reported than sent short.
func memoryPart(ctx context.Context, s *settings, limit int64) (Part, agentmemory.Manifest, []Omission, error) {
	opts := s.memRender
	text, man, err := agentmemory.Render(ctx, s.memStore, s.memScopes, opts...)
	if err != nil {
		return Part{}, man, nil, fmt.Errorf("agentkit: rendering memory: %w", err)
	}
	if limit > 0 && int64(len(text)) > limit {
		bounded := append(append([]agentmemory.RenderOption(nil), opts...),
			agentmemory.WithMaxTotalBytes(int(limit)))
		text, man, err = agentmemory.Render(ctx, s.memStore, s.memScopes, bounded...)
		if err != nil {
			return Part{}, man, nil, fmt.Errorf("agentkit: rendering memory: %w", err)
		}
	}
	dropped := limit < 0 || (limit > 0 && int64(len(text)) > limit)
	if dropped {
		// The block has a floor it cannot go under, a header and one
		// heading per scope, so a share below that floor buys nothing.
		// Report every entry rather than send a block the budget said
		// there was no room for.
		text = ""
		man.Omitted = append(man.Omitted, man.Entries...)
		man.Entries = nil
	}

	omitted := make([]Omission, 0, len(man.Omitted))
	for _, e := range man.Omitted {
		reason := e.Reason
		if reason == "" {
			reason = agentmemory.OmitBudget
		}
		omitted = append(omitted, Omission{
			Part:   PartMemory,
			Source: SourceMemory,
			What:   string(e.Scope) + "/" + e.Name,
			Reason: reason,
			Size:   int64(e.Bytes),
		})
	}
	return Part{ID: PartMemory, Text: text, Source: SourceMemory}, man, omitted, nil
}

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
