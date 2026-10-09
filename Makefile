GO ?= go
# Nested modules with their own go.mod, so their dependencies stay out
# of the root. examples/a2a is the only one: the a2a packages pull in a
# gRPC stack, and the root must stay buildable without it. Keeping the
# example here anyway means CI compiles and vets the composition the
# docs describe, so it cannot rot.
SUBMODULES = examples/a2a

# Runs $(1) in every nested module, skipping one whose go line the
# running toolchain is too old for. examples/a2a needs go 1.26 (its
# golang.org/x/net does) while the root's floor is 1.25, and the CI leg
# that proves that floor runs with GOTOOLCHAIN=local, so it builds,
# vets and tests the root and says what it left out. tidy-check, lint
# and vuln do not skip: they run on a current Go, where a skip would
# hide a failure.
define in_submodules
@for m in $(SUBMODULES); do \
  if ! out=$$(cd $$m && $(GO) list -m 2>&1); then \
    case "$$out" in *"requires go >="*) echo "skipping $$m: $$out"; continue;; esac; \
    echo "$$out"; exit 1; \
  fi; \
  (cd $$m && $(1)) || exit 1; \
done
endef

STATICCHECK ?= $(GO) run honnef.co/go/tools/cmd/staticcheck@latest
GOVULNCHECK ?= $(GO) run golang.org/x/vuln/cmd/govulncheck@latest

.PHONY: build deps direct no-replace test vet fmt tidy tidy-check lint vuln check release clean

build:
	$(GO) build ./...
	$(call in_submodules,$(GO) build ./...)

# Every other module in the workspace has a deps target that refuses a
# dependency it did not name. This one is the inverse: agentkit is the
# one module that imports every sibling, so what it must not grow is a
# dependency that is neither a sibling, the MCP SDK the sibling
# mcpclient's transports are typed in, nor the standard library. A
# third-party import here is composition the kit invented rather than
# assembled, which is the failure the whole design is against.
#
# The list below is every import that reaches the build, and each entry
# says whose it is. Nothing here is imported by this module's own code:
# go.yaml.in is agentskill's frontmatter parser, and the rest arrive
# through the MCP SDK, whose Transport types are the argument
# WithMCPTransport takes.
deps:
	@deps=$$($(GO) list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./... \
	  | grep -v '^github.com/ChristopherDavenport/' \
	  | grep -v '^go.yaml.in/yaml/' \
	  | grep -v '^github.com/modelcontextprotocol/go-sdk' \
	  | grep -v '^github.com/google/jsonschema-go' \
	  | grep -v '^github.com/segmentio/asm' \
	  | grep -v '^github.com/segmentio/encoding' \
	  | grep -v '^github.com/yosida95/uritemplate' \
	  | grep -v '^golang.org/x/' || true); \
	  test -z "$$deps" || { echo "module depends on: $$deps"; exit 1; }

# The one import this module's own files make outside the siblings and
# the standard library, so a new third-party import shows up as a diff
# here rather than passing under the transitive allowlist above.
direct:
	@direct=$$($(GO) list -f '{{join .Imports "\n"}}' ./... | sort -u \
	  | grep '\.' \
	  | grep -v '^github.com/ChristopherDavenport/' \
	  | grep -v '^github.com/modelcontextprotocol/go-sdk/mcp$$' || true); \
	  test -z "$$direct" || { echo "this module's code imports: $$direct"; exit 1; }

# Every sibling is required at a released version, with no replace: the
# kit is the module that proves the released libraries compose, so it
# has to be built against the versions a consumer would fetch. A replace
# here would let a change that has not shipped pass CI.
# The root alone. A nested module under SUBMODULES must replace the
# root, because the root has no released version for it to require;
# that replace is the house convention, not the thing this forbids.
no-replace:
	@! grep -q '^replace' go.mod \
	  || { echo "go.mod has a replace; the kit builds against released siblings only"; exit 1; }

test:
	$(GO) test -race ./...
	$(call in_submodules,$(GO) test -race ./...)

vet:
	$(GO) vet ./...
	$(call in_submodules,$(GO) vet ./...)

tidy:
	$(GO) mod tidy
	@for m in $(SUBMODULES); do (cd $$m && $(GO) mod tidy) || exit 1; done

# Fails when go mod tidy would change go.mod or go.sum, without writing,
# so a stray dependency shows up in make check and not only in CI's diff.
tidy-check:
	$(GO) mod tidy -diff
	@for m in $(SUBMODULES); do (cd $$m && $(GO) mod tidy -diff) || exit 1; done

fmt:
	gofmt -l . && test -z "$$(gofmt -l .)"

lint:
	$(STATICCHECK) ./...
	@for m in $(SUBMODULES); do (cd $$m && $(STATICCHECK) ./...) || exit 1; done

vuln:
	$(GOVULNCHECK) ./...
	@for m in $(SUBMODULES); do (cd $$m && $(GOVULNCHECK) ./...) || exit 1; done

# Everything CI runs.
check: fmt tidy-check vet deps direct no-replace lint vuln test

MODULE := $(shell $(GO) list -m)
NOTES := $(shell mktemp)

# Cut a release: the changelog's Unreleased section is dated, everything
# is checked, one commit is made, the root is tagged VERSION with the
# changelog section as the message, and the branch and tag are pushed.
# TRAILER, when set, is appended to the commit message.
#
# The changelog is dated through a temp file rather than sed -i, which is
# a GNU-ism: BSD sed reads the argument after -i as a backup suffix, so
# the GNU spelling fails outright on macOS, where these releases are cut.
# The temp file is removed if sed dies, so a failed run leaves nothing
# untracked behind for the clean-tree gate to trip over next time.
release:
	@test -n "$(VERSION)" || { echo "usage: make release VERSION=vX.Y.Z"; exit 1; }
	@grep -q '^## Unreleased$$' CHANGELOG.md || { echo "CHANGELOG.md has no Unreleased section"; exit 1; }
	@test -z "$$(git status --porcelain)" || { echo "working tree is not clean"; exit 1; }
	@! grep -q '^replace' go.mod || { echo "go.mod has a replace; the kit releases against released siblings only"; exit 1; }
	sed 's/^## Unreleased$$/## $(VERSION) - '"$$(date +%F)"'/' CHANGELOG.md > CHANGELOG.md.tmp \
	  && mv CHANGELOG.md.tmp CHANGELOG.md \
	  || { rm -f CHANGELOG.md.tmp; exit 1; }
	$(MAKE) tidy
	$(MAKE) check
	git add -A && git commit -q -m "Release $(VERSION)" $(if $(TRAILER),-m "$(TRAILER)")
	@awk -v v="$(VERSION)" '/^## /{p=($$2==v)} p' CHANGELOG.md | sed '1s/.*/$(VERSION)/' > $(NOTES)
	git tag -a $(VERSION) -F $(NOTES)
	@rm -f $(NOTES)
	git push origin HEAD
	git push origin $(VERSION)

clean:
	rm -rf .cache
