# agentkit

Assembly for the nine libraries: one call that turns a product's
choices into an `agentturn.Config`, and the one module in the workspace
that imports every other. The design of record is `README.md` and
`docs/`; read them before writing code, and read `docs/ordering.md`
before changing an order.

## The rule

`Config()` returns a plain `agentturn.Config`. Every field it sets a
product could have set by hand, with the same values, by calling the
same exported functions. No private seams, no wrapper types a caller
cannot construct, no behaviour that exists only when the kit assembled
it.

Every change is measured against that rule. A new field the kit sets
needs a line in `docs/manual.md` naming the manual call, or
`TestEveryFieldTheKitSetsIsDocumented` fails. A new composition needs
`TestTheManualPathIsTheSamePath` to still pass, which means it has to
be writable with exported calls alone.

## Module

- Module path: `github.com/ChristopherDavenport/agentkit`.
- Go 1.25 is the floor. The package name is `agentkit`.
- One module, no nested ones. Every other library in the workspace
  keeps its dependencies out through nested modules; this one has
  nothing to keep out, because importing every sibling is the point.
- Every sibling is required at a **released** version with **no
  `replace`**. `make no-replace` enforces it. The kit is what proves
  the released libraries compose, so a replace would let a change that
  has not shipped pass CI. Bumping a sibling means releasing that
  sibling first.
- `make deps` and `make direct` bound what else may arrive: this
  module's own code imports the siblings, `modelcontextprotocol/go-sdk`
  for the transport type `WithMCPTransport` takes, and the standard
  library. A third-party import here is composition the kit invented
  rather than assembled.

## Siblings

- `../openresponses`: the wire package. Copy its conventions.
- `../agentturn`: the loop. `Config` is the thing this module builds;
  the `Chain*` functions are how it fills a contested field.
- `../agentturn/session`: the recorder. Started by `WithSession`,
  attached by `Kit.Attach`, and bound to `compact.WithOnFold`.
- `../agentsession`: the format. `Part` is its `InstructionPart` and
  `Separator` is its `PartSeparator`, so `Kit.Parts` goes straight to
  `ConfigFromRequestParts`.
- `../agentsmd`, `../agentskill`, `../agentmemory`: the three layers
  that write instruction text.
- `../agentpolicy`: the engine, the guards, and `GrantSet`, which is
  where a skill's `allowed-tools` lands.
- `../agenttool`, `../agenttool/mcpclient`: the tool contract and the
  fourth tool source.
- `../agenttui`: the terminal client. It does not import this module
  and this module knows nothing about it.

## Conventions

Mirror `../agentsmd`: a `Makefile` with `build`, `deps`, `direct`,
`no-replace`, `test`, `vet`, `fmt`, `tidy`, `tidy-check`, `lint`,
`vuln`, `check` and `release` targets, the same CI shape, a
`CHANGELOG.md` in Keep a Changelog form, annotated `v*` tags. `make
check` must pass before any commit.

Tests are offline: no test calls a model, reaches a network, or shells
out. The MCP tests run a server from the SDK over an in-memory
transport. Skills, AGENTS.md files and memory stores are built in
`t.TempDir()` by the helpers in `kit_test.go`.

## Done

`dax` is the test, measured on the only quantity the kit moves: the
library wiring, not the module. An earlier version of this file asked
for dax to get dramatically smaller overall, and that bar was
unmeetable — wiring is 231 of dax's 1,655 lines, so perfecting all of
it could not dominate the total. The rewrite in
`../agentstudies/pi-coding-agent` took those 231 lines to 166.

So: 166 wiring lines is the baseline, and a change that raises it owes
an argument. What the acceptance test is really for is the defects it
surfaces — it found three, including an ordering the kit had inverted,
which is what `WithChildAgent` and `WithDeferredTools` exist to fix.
