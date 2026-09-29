# Ordering

Four layers write instruction text and one string reaches the model.
The order they are joined in is a policy, and until this module there
was nowhere to state it: a product joined the blocks with `"\n\n"` in
whatever order it had them in, and that join was the whole composition.

Every position below is a decision a product reverses with `WithOrder`.
None of them is a default chosen because it was convenient.

## The order

| # | part id | source | why here |
|---|---|---|---|
| 1 | `product` | the product's own prompt | the frame everything else refines |
| 2 | `skills` | `agentskill` | a catalogue, not an instruction |
| 3 | `memory` | `agentmemory` | what the agent knows, before what the repo says |
| 4 | `agentsmd` | `agentsmd` | the repository has the last word |

## The argument

The governing rule is the one the AGENTS.md convention states and
`agentsmd` settled *within* the chain: later text overrides earlier, so
the most specific text goes last. The same argument applies across the
layers and nobody had made it. Here it is.

**`product` is first** because it is the only text the product wrote
about its own agent, and everything after it is a refinement of that
frame rather than a competitor to it. A product prompt that a memory
entry could not override would make the memory tools decorative; a
product prompt that overrode everything would make the other three
layers advisory. First is the position that says "this is the shape of
the agent, now here are the specifics".

**`skills` is second** because it is not an instruction at all. It is a
catalogue: a list of names, descriptions and locations the model
chooses from, plus the paragraph telling it how to fetch one. Nothing
in it contradicts anything, so its position is free, and the position
that costs least is early — a catalogue read before the specifics is a
catalogue read as a menu rather than as a correction.

**`memory` is third, before `agents.md`** because a repository
instruction should be able to override something the agent remembered.
Memory is what this agent learned, across repositories and across
sessions; AGENTS.md is what this tree says about itself. When they
disagree — the agent remembers that a project uses tabs, the repository
it is standing in says spaces — the repository is right, and it is
right by the same rule that makes the nearest AGENTS.md win over the
farthest. Putting memory last would make a remembered habit outrank the
checked-in instruction that contradicts it, which is the wrong failure:
a memory is a guess about what is usually true, and an AGENTS.md is a
statement about what is true here.

**`agents.md` is last** because it is the most specific text there is
and the convention's own rule is that the closest file wins. The part
is itself an ordered chain — `Extra` first, the nearest file last — so
the whole instruction string reads farthest-to-nearest from end to end:
the product's frame, then a catalogue, then what the agent knows, then
what this repository says, then what this directory says.

## Separators

Parts are joined with `agentkit.Separator`, which is
`agentsession.PartSeparator`, which is `"\n\n"`. It is the session
format's separator and not a choice of the kit's, because the parts are
what the session records and `agentsession.ConfigFromRequestParts`
refuses parts whose join is not the instructions the request carried.
A part that renders empty is dropped rather than joined, so a kit with
no memory store does not send a blank line where the block would have
been.

Within the `skills` part the catalogue and the usage paragraph are
joined with the same separator, which is a choice, and a small one: the
two are one part because they are one layer, and a session that
recorded them separately would report a change to the catalogue as a
change to two parts.

## The budget

`WithInstructionBudget(n)` bounds the joined text. It is off by
default, because each layer already has its own bound and a total
budget is a thing a product asks for rather than a thing a kit
imposes.

When it is on, the budget is spent in this order, which is not the
order the parts are joined in:

1. **The product prompt and the skill catalogue are charged first, and
   never trimmed.** No exported call bounds either — `Catalog.Prompt`
   takes no budget and cutting text the product wrote is not the kit's
   to do. If the two of them plus the separators already exceed `n`,
   `New` fails and says so, rather than sending a prompt the caller
   asked not to send.
2. **`agents.md` gets the whole remainder.** It is the repository's
   instruction for this exact tree, the text with the last word, and it
   is bounded in practice by what people are willing to write into
   files. `agentsmd.Options.Budget` already degrades it gracefully: the
   first file that would go over, and every file after it, is left out
   and reported, and no file is ever cut short.
3. **`memory` gets what `agents.md` did not use.** Memory is the
   layer that has to live within what is left, because it is the only
   one that grows on its own: an agent that remembers things
   accumulates them without anyone deciding to write a file.
   `agentmemory.Render` already skips an entry that does not fit, keeps
   considering the ones after it, and lists what it skipped by name so
   the model knows `memory_search` can fetch it.

So the render order is `agents.md`, then `memory`, and the join order
is `memory`, then `agents.md`. A caller who wants the opposite
allocation sets `agentsmd.Options.Budget` and
`agentmemory.WithMaxTotalBytes` itself and leaves the kit's budget at
zero; the kit's budget is a convenience over those two calls and never
a substitute for them.

Three details of the mechanism, all of which the tests pin:

- A layer's own bound and the kit's share both apply, and the smaller
  binds. The kit renders once with the caller's options, and only
  re-renders under its share if that first render was over it.
- `agentsmd.Options.Budget` counts the bytes of the files, and the part
  is those bytes plus the wrapper `agentsmd.Render` puts around each
  one. So the share handed to a second chain is the share less the
  wrapper the first chain's files cost. One pass is enough: a smaller
  budget can only select a prefix of the same files, so the wrapper can
  only shrink.
- A share too small for a layer's own floor buys nothing. The memory
  block always writes a header and one heading per scope, so a share
  under that cannot be met; rather than send a block the budget said
  there was no room for, the kit drops the part and reports every entry
  as omitted.
- The memory part is the block and `agentmemory.Usage()` after it, and
  the paragraph cannot be bounded, so it is paid for first: the block is
  rendered under the share less the paragraph and its separator. A
  share that cannot hold the paragraph drops the part, paragraph and
  all, since the paragraph describes a block that is not there.

## Hooks

The same problem in a different place: `agentturn.Config` has one field
per hook and more than one layer wants each. agentturn added the
`Chain*` functions because the contest was silent — assigning the field
twice keeps the second assignment and loses the first with no error and
no sign. The kit is where they get called, in one order:

| field | order | why |
|---|---|---|
| `BeforeModelCall` | memory re-render, then the guards over each part, then the guards over the whole request, then the product's | the guard must see the text memory injected, or it is guarding a request that is not the one sent; over each part first, so a rewrite lands in the part it belongs to and the parts stay the request that was sent |
| `BeforeToolCall` | the policy engine, then the product's | one engine, keyed by run; decisions fold deny over ask over allow whatever the order, so this one is about which reason the user is shown |
| `OutputGuard` | the guards, then the product's | each sees what the one before it left |
| `ShouldStopAfterTurn` | the policy's guards, then the product's | the chain stops at the first hook that stops the run, so the error on `RunEnd` names the layer that fired |
| `BeforeTurn` | the skill grants' revoke on turn 1, then the product's | under `WithSkillGrantScope` a grant lasts the run that read the skill, so the revoke has to land before anything in the new run is decided |
| `Transform` | the product's, then compaction | `agentturn.ChainTransform`: the fold is over what the product shaped, so a redaction or a filter is never undone by a summary written from the unshaped transcript |

`WithOnFold` is bound to the session recorder when there is one, and is
appended after the caller's compaction options, so a caller who passes
their own `WithOnFold` alongside a session loses theirs. That is the
one place the kit's choice beats the caller's, and it is deliberate: a
fold the session did not record is a session that fails `Verify`.
