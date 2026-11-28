# Working in `magefiles/`

This directory houses the repository's task automation layer using Mage.

## Layout and organization

The top level of `magefiles/` is strictly for Mage namespace declarations, targets, and flag parsers in `package main`. Do not place heavy domain logic, parsers, HTTP scrapers, code generators, or template walkers directly in `package main` files at the root of `magefiles/`.

All implementation logic belongs in modular Go packages under `magefiles/pkg/`:

| Path                      | Purpose                                                                   |
| ------------------------- | ------------------------------------------------------------------------- |
| `magefiles/dev.go`        | Mage entrypoint for the `Dev` namespace (`dev:checkTables`, `dev:vendor`) |
| `magefiles/generate.go`   | Mage entrypoint for the `Generate` namespace (`generate:document`)        |
| `magefiles/core/core.go`  | Bootstrapping main package for `go run ./magefiles/core`                  |
| `magefiles/pkg/gen/docs/` | The book's generators - the copied-in pages, and the table of contents    |
| `magefiles/pkg/osv/`      | The `osv-scanner.toml` overrides written beside vendored non-Go manifests |
| `magefiles/pkg/util/`     | Shared helpers - currently the aligned-table check                        |

## Target structure rules

1. **Keep root namespace targets thin.** Root Mage methods should only perform argument defaults, environmental setup, or high-level status output, delegating immediately to the appropriate package under `magefiles/pkg/`.
2. **Encapsulate and test.** Logic under `magefiles/pkg/` should be written as standard Go packages with unit test files (`*_test.go`) adhering to the table-driven test formatting guidelines in the root [`AGENTS.md`](../AGENTS.md).

## The build tag, and why `go run ./magefiles/core` is the spelling to use

Every file at the root of `magefiles/` carries `//go:build mage`, which is how Mage finds targets and why a plain `go build ./...` does not compile this directory. `magefiles/core/core.go` carries no tag: it is an ordinary `main` that calls `mage.Main()`, so `go run ./magefiles/core <namespace>:<target>` runs the same targets as the `mage` CLI without needing it installed.

Use that form in CI, in hooks, and in documentation. It needs only the Go toolchain, and - because this repository vendors - it needs no network either.

## Everything builds from `vendor/`, with no network

`vendor/` is committed, and the build is expected to work on a machine that cannot reach a proxy. The Go toolchain selects `-mod=vendor` automatically when `vendor/` is present, so this is usually invisible - which is the problem: a dependency added without re-vendoring builds fine locally and fails only where it matters.

So a change to the dependency graph is not finished until `vendor/` is rewritten and committed in the same change:

```sh
go run ./magefiles/core dev:vendor        # go mod tidy, go mod vendor, then the osv overrides
go run ./magefiles/core dev:verifyVendor  # go mod verify, a GOPROXY=off build, then the overrides
```

### `go mod vendor` deletes two files that have to be there

`go mod vendor` copies two Python `requirements` files into `vendor/` from modules that ship them in their own repositories, and OpenSSF Scorecard scores every advisory against those packages against this repository. A generated `osv-scanner.toml` sits beside each of them to stop that, and `go mod vendor` wipes both every time it runs - so `dev:vendor` ends with `dev:writeOSVOverrides`, and anything else that re-vendors has to call it too. `dev:verifyVendor` fails when one is missing, has drifted from the generator, or when `vendor/` grows a lockfile for an ecosystem nothing covers yet. The list and the reasoning are in [`magefiles/pkg/osv/osv.go`](pkg/osv/osv.go) and [`docs/agent/choice-osv-vendor-overrides.md`](../docs/agent/choice-osv-vendor-overrides.md).

`dev:verifyVendor` is the one that proves it: it builds with `GOFLAGS=-mod=vendor GOPROXY=off`, which is the invocation a release has to survive. [`.github/workflows/check-go-mod.yaml`](../.github/workflows/check-go-mod.yaml) runs the same target, and a `verify-vendor` pre-commit hook runs it whenever `go.mod`, `go.sum`, or `vendor/` changes.

One deliberate exception: `test/e2e/` is a separate Go module and is **not** vendored. It is a dev/CI-only surface that needs the network anyway (kind images, clusterctl, CAPD), and vendoring it would bloat every clone to protect an invariant - offline buildability - that does not apply to it. Being a nested module, it is invisible to the root `./...` and to `dev:verifyVendor` by construction. Do not vendor it for consistency's sake; this line is the record that its absence is on purpose.

Two consequences worth stating:

- **A new dependency is a bigger decision than it looks**, because it lands in `vendor/` and is carried by every clone of the repository from then on. The cost of a dependency is what it links, not what you call - check with `go list -deps ./...` before adding an import.
- **`vendor/` is generated, and marked so.** It is `linguist-generated` in [`.gitattributes`](../.gitattributes), and every content-reading pre-commit hook excludes it. A hook that reformats a vendored file makes the next `go mod vendor` look like a change nobody made.

### A new `.gitignore` pattern can silently empty part of `vendor/`

This is the failure worth knowing before writing one. `go mod vendor` copies whatever the dependencies ship, which includes files matching patterns written for this repository's own build output. An **unanchored** `dist/`, `bin/`, `*.out` or `*.tar.gz` matches at every depth, so it swallows a vendored package's files as readily as a local build artifact - and nothing reports it. The tree builds locally, because the files are on disk. It fails after a fresh clone, because they were never committed.

So a new ignore pattern is anchored with a leading `/` unless it genuinely has to match at every depth, and [`.gitignore`](../.gitignore) ends with `!/vendor/**` as the backstop. Two details about that line, both easy to get wrong:

- **It has to be last.** Git takes the last matching pattern, so a negation placed above the patterns it overrides does nothing.
- **`!/vendor/` is not the same thing.** The bare form un-ignores the directory entry and leaves every file inside it to be matched against the patterns above, which is no protection at all.

The backstop has a reach limit: a vendored module's **own** `.gitignore`, copied in by `go mod vendor`, takes precedence over any parent pattern for files in its own directory, so `!/vendor/**` cannot override it. When an upstream module ignores a file it also ships (go-lzo's `CHANGELOG.md` was one), the fix is `git add -f` on that file - once tracked, re-vendoring keeps it.

Check a change rather than reasoning about it - this lists every vendored file git is dropping, and should print nothing:

```sh
git ls-files --others --ignored --exclude-standard -- vendor/
```

`dev:verifyVendor` runs this same check and fails on any output, so the pre-commit hook and `check-go-mod.yaml` catch both forms without anyone remembering to run it.

## The e2e targets are documented step by step, so changing one changes a page

[`docs/dev/e2e-walkthrough.md`](../docs/dev/e2e-walkthrough.md) enumerates what `e2e:up`, `e2e:run` and `e2e:move` do - **by step name, in order** - because the thing an operator or a contributor needs from those targets is what each one is responsible for and what it leaves behind. That makes the page tightly coupled to `pkg/e2e/`, in three places that all go stale silently:

- **The step tables.** Each row is a `{"name", fn}` entry from the `steps` slice in `Up` or `Run`. Adding, removing, renaming or reordering one leaves the page describing a flow that no longer exists, and the step names are what a failure prints (`==> <name>`), so a reader matching an error against the page finds nothing.
- **The environment table.** It lists every `E2E_*` variable the targets export to the suite. A new one that only exists in code is invisible to someone trying to run a single test by hand, which is the page's main worked example.
- **The artifact table.** It lists what lands in `dist/e2e/`. A new file - or a renamed one, as `workload.yaml` became `workload-v<version>.yaml` - makes the "start genuinely clean" section wrong about what needs deleting.

So treat it the way [`.github/AGENTS.md`](../.github/AGENTS.md) treats `docs/workflows/`: the page is part of the change, not a follow-up. The same applies to the failure table - when a new environment assumption gets encoded in a step (a CNI, a taint, a StorageClass, a Pod Security label all have), the symptom it produces when absent belongs in that table, because the next person to meet it will search for the symptom and not the cause.

Nothing in the build checks any of this. `mdbook build` does not read Go, and no test asserts that a step name in a table still exists in the slice - so the check is `grep -n '{"' magefiles/pkg/e2e/*.go` against the tables before opening the pull request.

## Every pin needs an owner

A pinned value is a version, tag, digest, or checksum written into the tree that names something living upstream: a container image digest, a package version, an upstream tag, a cloud image's `sha256`. Pinning is usually the right default - a build has to be reproducible - but a pin with nothing watching it goes stale silently. Nothing fails; the tree just keeps installing a version that stopped being current, and often stopped being patched, months ago.

So a new pin is not done when it is written. It is done when something can move it. Pick one of two owners and wire it up in the same change:

- **A mage target that rewrites the pin in place** - when the current value has to be *discovered* (queried from repodata, resolved from a registry, read from an upstream checksum file) and then written back into one or more files. The target does the query, the rewrite, and the reporting; the human runs it and reviews the diff. Where the pinned value is a whole list, the target needs a second half that *tells you to run it* - a verify target that re-fetches and exits non-zero on drift - because nobody spots a changed digest by reading a thousand-line file.
- **A [`.github/renovate.json5`](../.github/renovate.json5) entry** - when the pin sits in a file format Renovate already parses, so it costs a `packageRules` entry rather than a generator. `gomod`, `github-actions`, `pre-commit` and `npm` cover most of it; a pin in a Go constant, a Dockerfile, or a bespoke JSON manifest is reachable through the `customManagers` regexes, by annotating the line above it:

```go
// renovate: datasource=github-releases depName=owner/repo
const toolVersion = "1.2.3"
```

Prefer the Renovate entry when the format is already supported or can be annotated: it is a few lines, it opens the pull request unprompted, and it needs nobody to remember. Reach for a mage target when the value has to be computed rather than looked up.

The rule holds for values pinned outside `magefiles/` too, which is most of them. This section lives here because the automation does.
