# Contributing

Issues and pull requests are welcome.

## Before you start

This module assembles the nine libraries into an `agentturn.Config`. It
owns three compositions — the order of the instruction parts, the order
of the hooks that contest a field, and the union of the tool sets — and
nothing else. A change that invents behaviour rather than assembling it
belongs in the library that should have had it.

`README.md` and `docs/` are the design; read them first, and read
[`docs/ordering.md`](docs/ordering.md) before changing an order.

For anything larger than a bug fix, open an issue first so the shape of
the change can be discussed before you spend time on it.

## The rule

`Config()` returns a plain `agentturn.Config`. Every field it sets a
product could have set by hand, with the same values, by calling the
same exported functions. No private seams, no wrapper types a caller
cannot construct, no behaviour that exists only when the kit assembled
it.

That rule is mechanically enforced, so a change that breaks it fails
rather than merely reading wrong:

- A new field the kit sets needs a line in
  [`docs/manual.md`](docs/manual.md) naming the manual call, or
  `TestEveryFieldTheKitSetsIsDocumented` fails.
- A new field on `agentturn.Config` needs a line there too, even if the
  kit leaves it alone, or `TestEveryConfigFieldIsNamedByTheManual`
  fails.
- A new composition has to keep `TestTheManualPathIsTheSamePath`
  passing, which means it has to be writable with exported calls alone.

If a line in `docs/manual.md` ever has to say "and then something the
kit does privately", the kit has become a framework.

## Dependencies

This is the one module in the workspace that imports every other, so
what it must not grow is a dependency that is neither a sibling, the
MCP SDK, nor the standard library. `make deps` and `make direct`
enforce that; a third-party import here is composition the kit invented
rather than assembled.

Every sibling is required at a **released** version with **no
`replace`**, and `make no-replace` enforces it. The kit is what proves
the released libraries compose, so a replace would let a change that
has not shipped pass CI. Bumping a sibling means releasing that sibling
first.

`examples/a2a` is a nested module because the a2a packages pull in a
gRPC stack that would take the build from 64 packages to 201. It
replaces the root, which is the house convention for a nested module
and not what `make no-replace` forbids.

## Development

Go 1.25 or later is required. The full local check is:

```sh
make check        # fmt, tidy, vet, deps, direct, no-replace,
                  # staticcheck, govulncheck, race tests
```

It covers the root and every nested module.

Tests are offline: no test calls a model, reaches a network, or shells
out. The MCP tests run a server from the SDK over an in-memory
transport. Skills, AGENTS.md files and memory stores are built in
`t.TempDir()` by the helpers in `kit_test.go`.

## Pull requests

- Keep the change focused; unrelated cleanups belong in their own PR.
- Add or update tests. Tests are table-driven and run offline.
- An option that changes behaviour is argued in `docs/` or it does not
  exist.
- Run `make check` before pushing. CI runs the same steps on the
  minimum and current Go versions.
- Note user-visible changes under *Unreleased* in `CHANGELOG.md`.
