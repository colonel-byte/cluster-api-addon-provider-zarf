# Markdown line formatting

## Do not hard-wrap lines in Markdown documents

Markdown files should NOT have hard-wrapped or truncated lines. Instead, write full paragraphs and list items on single, continuous lines without manual line breaks, letting editors and renderers handle line wrapping dynamically.

- Write entire paragraphs or list items on a single line without manual line breaks / hard wraps.
- Let editors and rendering tools handle line wrapping dynamically.

## Pad table cells so the columns line up

Tables are written in their aligned form, so that the source reads as a table and a reviewer can see the columns without rendering the file. Pad every cell with spaces to the width of the widest cell in its column, keep one space of padding inside each pipe, and run the dashes of the delimiter row out to that same width. Every line of the table then has the same length.

Write this:

```markdown
| Path                                   | What it holds     |
| -------------------------------------- | ----------------- |
| `.spec.config.registries[N].auth.user` | Registry username |
```

Not this:

```markdown
| Path | What it holds |
| --- | --- |
| `.spec.config.registries[N].auth.user` | Registry username |
```

Adding a row that is wider than the column repads the whole table, which is expected - the alignment is part of the content, not a one-time cleanup.

# Working in this repository

This repository is `cluster-api-addon-provider-zarf` (CAAPZ), a Cluster API add-on provider that deploys [Zarf](https://github.com/zarf-dev/zarf) packages onto CAPI workload clusters. A platform team declares a `ZarfPackageProxy` on the management cluster; the controller matches it to clusters, pulls the package from an OCI registry, verifies its Cosign signature, and deploys it over the wire via Zarf's Go SDK, tracking per-cluster state in `ZarfPackageRelease`. [`README.md`](README.md) is the full pitch and [`docs/dev/architecture.md`](docs/dev/architecture.md) is the design an agent needs to hold to make a correct change. It is a controller, not a CLI: the binary takes manager flags and runs forever, and there is no interactive surface.

It runs in the management cluster of air-gapped enterprise environments. At runtime it may reach the management cluster API, the workload cluster APIs (via the `<cluster-name>-kubeconfig` secrets CAPI maintains), and an internal OCI registry - nothing public. Everything it deploys is staged into that registry ahead of time as a self-contained Zarf package.

## Layout

| Path                     | What lives there                                                                             |
| ------------------------ | -------------------------------------------------------------------------------------------- |
| `api/v1alpha1/`          | The CRD Go types - `ZarfPackageProxy`, `ZarfPackageRelease` - and their generated deepcopy   |
| `cmd/`                   | `cmd/cluster-api-addon-provider-zarf`, the controller manager's `main`                       |
| `internal/controller/`   | The reconcilers, private to this module                                                      |
| `config/`                | Kustomize manifests - generated CRDs under `config/crd/bases`, RBAC, manager deployment      |
| `magefiles/`             | The mage task runner's source - build, test, and generate targets                            |
| `docs/`                  | The mdBook source. Generated subpaths must not be hand-edited                                |
| `test/e2e/`              | The e2e suite - a separate, deliberately unvendored Go module; driven by the `e2e:*` targets |
| `docs/agent/choice-*.md` | Records of non-obvious decisions and the tradeoffs behind them - read before reversing one   |

Several directories carry their own `AGENTS.md` with rules specific to that directory. Check the closest one before editing. Keep this table in sync when adding or removing one -- add a row for a new `AGENTS.md`, and remove its row when the file goes away.

| `AGENTS.md`                                  | Covers                                                   |
| -------------------------------------------- | -------------------------------------------------------- |
| [`docs/AGENTS.md`](docs/AGENTS.md)           | Markdown formatting, generated pages                     |
| [`.github/AGENTS.md`](.github/AGENTS.md)     | Scorecard-safe workflow changes, PR description format   |
| [`magefiles/AGENTS.md`](magefiles/AGENTS.md) | Target layout, the vendored offline build, pin ownership |

`CLAUDE.md` in the repository root is a symlink to this file, so a Claude Code session started there loads these rules without being asked. That covers the root and nothing else: the directory-specific files in the table above are not symlinked, so a session whose working directory is one of them starts with none of their rules in context. The root is where a session normally starts, so this is usually moot - but do not treat "nothing was loaded for me" as "there are no rules here", and do not grep a rules file for the one thing you thought to ask about. Open the closest `AGENTS.md` and read it before writing anything in that directory.

## Building and testing

Build and test targets run through mage, not a Makefile. `magefiles/` itself does not build with a plain `go build` - it needs mage's special build tag - so drive it through the `mage` CLI or `go run ./magefiles/core <namespace>:<target>`. The latter form needs only the Go toolchain and works on a host with no `mage` binary installed; it's what CI and the pre-commit hooks use.

```sh
go run ./magefiles/core -l                  # list every target
go run ./magefiles/core generate:document   # regenerate docs/index.md, docs/security.md, docs/SUMMARY.md
go run ./magefiles/core dev:checkTables     # check Markdown table padding
go run ./magefiles/core dev:vendor          # go mod tidy, then go mod vendor
go run ./magefiles/core dev:verifyVendor    # go mod verify, then a GOPROXY=off build
go run ./magefiles/core test:envtest        # the tests that need a real API server
go run ./magefiles/core e2e:up              # kind + CAPI/CAPD + local registry topology
go run ./magefiles/core e2e:run             # the e2e suite, against the running topology
go run ./magefiles/core e2e:down            # tear the topology down
```

Plain Go commands work for anything mage doesn't wrap - `go build ./...`, `go vet ./...` - but exclude `./magefiles/...` from those, since it fails a bare build for the reason above.

Two test invocations are forbidden. A bare `go test -tags=envtest ./...`: the envtest suites need `CAPI_CRD_DIR` and `KUBEBUILDER_ASSETS` set to generated and downloaded artifacts, and `test:envtest` is what produces them. And a bare `go test` inside `test/e2e/`: that suite talks to a live topology only `e2e:up` builds (docker, kind, and kubectl on `PATH`), and `e2e:run` is what points it there - without `E2E_KUBECONFIG` every test skips, which reads as green while asserting nothing. Plain `go test ./api/... ./cmd/... ./internal/...` (no tag) stays runnable with nothing installed; `test/e2e` is a separate module, so `./...` from the root never reaches it.

### The build is vendored and offline, and that is a requirement

`vendor/` is committed, and the build has to work on a machine that cannot reach a module proxy. The Go toolchain picks `-mod=vendor` automatically when `vendor/` is present, which makes this invisible until it isn't: **a dependency added without re-vendoring builds fine locally and fails only where it matters.**

So a change to the dependency graph is not finished until `vendor/` is rewritten and committed in the same change. Three things enforce it, and each catches the same mistake at a different distance from the author:

- **`go run ./magefiles/core dev:vendor`** rewrites it - `go mod tidy` then `go mod vendor`.
- **A `verify-vendor` pre-commit hook** runs `dev:verifyVendor` whenever `go.mod`, `go.sum`, or `vendor/` changes.
- **[`.github/workflows/check-go-mod.yaml`](.github/workflows/check-go-mod.yaml)** re-runs tidy and vendor and fails on any diff, then runs `dev:verifyVendor` - which builds with `GOFLAGS=-mod=vendor GOPROXY=off`, the invocation a release has to survive.

Two consequences. **A new dependency is a bigger decision than it looks**, because it is carried by every clone from then on; the cost of a dependency is what it links, not what you call, so check `go list -deps ./...` before adding an import. And **`vendor/` is generated** - marked `linguist-generated` in [`.gitattributes`](.gitattributes), and excluded from every content-reading pre-commit hook, because a hook that reformats a vendored file makes the next vendor run look like a change nobody made.

## Keeping `.gitattributes` in sync with the generators

Everything a generator writes is marked `linguist-generated` in [`.gitattributes`](.gitattributes), which is what collapses it in a GitHub diff and keeps it out of the repository's language statistics. A generated file that is not marked shows up as hand-written work in every review, and a thousand-line regenerated page buries the twenty lines that were actually written.

So a change to what a generator produces is a change to `.gitattributes` as well. That means all three of:

- **a new generator, or a new output path** - add the path. A generator that writes three files beside each input needs all three marked; a pattern covering only the first leaves the others looking hand-written.
- **a generator that writes into a new subdirectory** - widen the pattern rather than adding a sibling entry, where the directory depth is the only difference.
- **a generator removed, or its output moved** - delete the entry. A pattern matching nothing is not an error anybody sees.

Patterns are grouped by the mage target that writes them, with the target named in a comment, so the file reads as a list of what each generator owns.

Hand-written files inside a generated tree stay unmarked. Mark the pattern the generator writes, not the directory it writes into.

Check a specific file rather than reasoning about the patterns:

```sh
git check-attr linguist-generated -- docs/commands/example.md
```

## Keeping `README.md` current

[`README.md`](README.md) is the repository's front page and the pitch: what the project does, what it supports, how to install it, and one example per entry point. It is also the source for the book's landing page, so a stale README is stale in two places.

Treat it as part of the change, not a follow-up:

- **a feature lands** - add it where a reader would look for it. A new command belongs in `Usage Examples` with a runnable snippet; a new install channel in `Installation`; a new config file or schema in `Configuration and Schemas`.
- **a feature is removed or renamed** - delete or rename every mention, including the example that exercised it. A documented flag that no longer exists is worse than no documentation.
- **behaviour changes under an existing heading** - fix the prose and the snippet together. Commands shown in `README.md` are copied and pasted; they should work as written against the current build.

The generated reference under `docs/` comes from the code, so it corrects itself on the generate target. `README.md` does not - it is hand-written, and nothing fails if it drifts. After editing it, run that target so the landing page matches.

## Say so when you notice user-facing drift, even out of scope

Documentation and user-facing strings drift silently. Nothing fails, no test goes red, and the generated pages under `docs/` regenerate the stale text faithfully - so a string that describes a capability the code removed can survive several releases, looking authoritative the whole time.

So drift noticed is drift reported. When work in one corner of the tree turns up a stale user-facing surface somewhere else - a flag description, a `Long` or example string, a `jsonschema` example, a module doc block, a README snippet, a key missing from the sample config - name it in the change description, with the file and what is wrong. Do not quietly widen the change to fix it, and do not stay silent because it is out of scope: the operator deciding whether it is worth a follow-up is the point, and an unfixed drift they know about is a fine state to leave the tree in.

Two things make this worth a rule rather than a habit:

- **the stale copy is usually upstream of several generated files**, so what looks like one wrong line in `docs/` is one wrong line in a Go string plus four regenerated pages, and a reader who corrects the page by hand loses the fix on the next generate run. Chase the wording back to the string that produced it before reporting where it lives.
- **a vendored string can be the source.** Where a flag description comes from a vendored upstream package, `vendor/` cannot be edited - the fix is a constant in this repository's own strings package and a changed reference.

## A doc's claims are checkable, so check them

The section above is about wording that has gone stale. This one is about the harder case: a document that is confidently, verifiably wrong, because the thing it describes no longer exists. A dead reference reads exactly like a live one. `mdbook build` does not fail on a dead relative link, `go vet` never opens a Markdown file, and no test asserts that a path, symbol, flag, or module named in prose is still in the tree - so these survive indefinitely, and the longer they survive the more authoritative they look.

The cost is not spread evenly. A wrong count in an internal design note costs a reader a minute of confusion. A command printed in `docs/guides/` that cannot run costs an operator an afternoon, because they will assume they typed it wrong before they assume the guide is wrong. Weight the checking accordingly.

So when a change touches something a document describes, check what that document says about it. Each of these is one command:

| Failure mode                                   | How to check it                                                                                                                    |
| ---------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------- |
| A path, file, or directory that moved or went  | `ls`/`find` the path as written                                                                                                    |
| A Go symbol that was renamed or unexported     | `grep -rn --include='*.go' '<name>' .`, or `go doc`. An unexported-to-exported rename is the easy one to miss                      |
| A module asserted to be present or absent      | `grep -n '<module>' go.mod vendor/modules.txt`, and `go list -deps ./cmd/cluster-api-addon-provider-zarf` for whether it is linked |
| A mage target that does not exist              | `go run ./magefiles/core -l`                                                                                                       |
| A manager flag that does not exist             | `grep -rn '<flag>' cmd/ internal/`. Check the `flag.String` registration, not your memory                                          |
| A quoted error or log string                   | `grep -rn '<fragment>'` for the literal. Quoting these from memory is how a guide ends up printing an error the code does not emit |
| A relative link between pages                  | `ls` the target. Nothing in the build catches a dead one                                                                           |
| A count, version, or pin given as current fact | Read the thing being counted. A number that was measured once and never re-measured is the default state of every count in a tree  |

### Flag it, and say whether it should be removed rather than fixed

Report what you find the way the section above says: name the file, the line, and what is actually true, and leave the decision with the operator rather than quietly widening the change. The one thing to add is a recommendation on **removal**, because that is the call a reader cannot make for themselves and the one that is easy to get wrong in both directions.

- **A dead reference inside a live document** is a fix: correct it, or delete the sentence if what it pointed at is gone and nothing replaced it. An orphaned heading with no body, left behind when its content was removed, is the same case.
- **A document whose entire subject was removed** is not a deletion. `docs/agent/choice-*.md` are constraints rather than history, and the reasoning in one usually still explains something that is still here - a Dependabot ignore entry, a Scorecard annotation, the shape of a package. Mark it superseded, name the change that superseded it, and leave the argument in place. Deleting it loses the answer to "why is this odd thing still configured".
- **A configuration entry a removed document justified** is a separate question with its own blast radius, and it is not yours to fold into a documentation change. A stale Scorecard annotation or Dependabot ignore is doing nothing, but removing it changes what CI measures. Flag it and let the operator decide.

When a change removes a module, moves a package out of the tree, or renames a type, the documentation sweep is part of it - and `grep -rn '<old-name>' --include='*.md' .` is the whole technique.

## Pinned versions, digests, and checksums

Pins live all over the tree - container image digests in Dockerfiles, package versions, upstream tags in a pins file, checksums in Go constants - but the machinery that moves them is mage and Renovate. Every pin needs one of those owning it, or a written bump procedure where neither fits. Before adding a pinned value anywhere, read the rule and the worked examples in [`magefiles/AGENTS.md`](magefiles/AGENTS.md#every-pin-needs-an-owner).

## Committing changes

Run the hooks over the whole tree before every commit and fix anything they flag. Prefer [`prek`](https://github.com/j178/prek) - `prek run --all-files` - if it is on `PATH`; it reads the same [`.pre-commit-config.yaml`](.pre-commit-config.yaml) and runs the same hooks, just faster. Fall back to `pre-commit run --all-files` when it isn't. Several of the hooks rewrite files (`end-of-file-fixer`, `trailing-whitespace`, `keep-sorted`, `addlicense`, doc/schema generators) - re-stage after the run. Don't skip hooks with `--no-verify` or `SKIP=`.

Two gaps worth knowing about, because a green hook run is weaker evidence than it looks:

- **A lint hook that reads the staged diff only sees uncommitted changes**, so a clean `prek run --all-files` does not mean CI's lint job passes on code that is already committed.
- **`--all-files` skips untracked files.** A new file is invisible to every hook until it is `git add`ed.

## Go test formatting

Do not condense struct literals in table-driven test cases onto single lines (such as `{name: "...", input: "...", want: "..."}`). Format each field on its own line within the struct literal to keep test cases legible and easy to review, diff, and edit.

```go
// Do not condense:
{name: "unconstrained release", release: "", want: true},

// Prefer multi-line:
{
	name:    "unconstrained release",
	release: "",
	want:    true,
},
```

## Writing style

Use `-` (hyphen), not `—` (em dash), wherever a hyphen reads fine.

## Commit messages

Keep commit messages to a short title only - no body paragraph explaining the change. End with a `Co-Authored-By` trailer.

```
test(fuzz): add fuzz targets for cfg, clustercfg, and identify source

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
```

## Pull requests

Before creating or updating a pull request description, read [`.github/pull_request_template.md`](.github/pull_request_template.md) and follow the format rules in [`.github/AGENTS.md`](.github/AGENTS.md#writing-a-pull-request-description).

### Generated output goes in a pull request of its own

A change that adds or re-renders generated artifacts does not share a pull request with the code change that prompted it. Where both are needed, they are two layers of a stacked PR: the generator, template, or format change first, then the rendered output on top of it.

This is about what a reviewer can see. A generator run that touches an existing template re-renders every directory already on disk - so the twenty lines that were actually written end up buried in a diff nobody can read to the end. GitHub collapses `linguist-generated` files, which helps with the file list and not at all with the review: the reasoning that needs checking is in the template and the generator, and it is the thing that disappears.

So:

- **A generator or template change, and the output it renders, are separate layers.** The lower layer carries whatever was reasoned about. The upper layer carries the rendered output and nothing else.
- **A render that is only a re-render is its own pull request.** Bumping a pin, backfilling a release line, refreshing a cached asset: there is no code to review, so there is nothing for it to be mixed with.
- **Check the split before pushing, not after.** `git show --stat <branch> -- <generated-path>` on the lower layer should print nothing. It is easy to leave rendered output in the wrong layer by running the generator while the wrong branch is checked out, and the fix once a reviewer is reading is a rebase of everything above it.

The one thing that does belong with the code is a *fixture* a test reads - those live under `test/`, and the rule does not reach them.

### Naming the branches in a stacked PR

Every branch in a stack is named `stack/<n>-<2-3 word header>`:

```
stack/1-color-tty
stack/2-strict-config
stack/3-verify-flags
```

**`<n>` counts within the stack, from 1.** It is the layer's position, bottom to top, so a nine-layer stack runs `stack/1-...` to `stack/9-...` and the next stack starts again at `stack/1-...`. A stack is self-contained; the number says where a branch sits in its own chain and nothing about any other work. Do not carry on from the highest number seen in the repository's history - that couples unrelated stacks to each other, and it means a branch name cannot be read without knowing the whole branch history.

**`<2-3 word header>`** is the layer's concern in kebab case, not a sentence: `exit-codes`, `flag-shorthands`, `phase-progress`. Two or three words, matching the one-sentence description the layer should already have - a layer whose concern will not fit in three words is usually two layers. Do not restate the stack's topic in every branch; the chain supplies that.

Because the number restarts, two live stacks can both hold a `stack/1-`, and uniqueness rests entirely on the header - `stack/1-role-parameter-gating` and `stack/1-color-tty` coexist happily. Pick a header that names this layer's concern and the clash does not arise; `gh stack add` fails outright on a branch that already exists, so a collision is loud rather than silent.

Names are used verbatim by `gh stack add`, so there is nothing to correct afterwards. Renaming a branch once its PR is open is not worth it: `gh stack` tracks the chain by branch, so the fix is to delete the stack and re-open it, which discards any review already left on it.

## Tracking work in issues

Work here is tracked in issues on the main repository, [`colonel-byte/cluster-api-addon-provider-zarf`](https://github.com/colonel-byte/cluster-api-addon-provider-zarf). Before starting a task, check for a related issue - someone may already have filed it, or filed something that overlaps enough to change the approach:

```sh
gh issue list --repo colonel-byte/cluster-api-addon-provider-zarf --search "<keywords>" --state all
```

If there is no issue covering the work, ask the person whether they would like one submitted before continuing. Don't open one unprompted, and don't treat a missing issue as a reason to stop - they may well say no.

When work is being broken up into a stacked PR, the issue structure should mirror the stack: the parent issue describes the whole change, and each layer of the stack gets a subissue created off that parent. Link each PR in the stack to its own subissue, not to the parent.

Only the top-most PR in the stack closes the parent issue - each lower PR closes its own subissue and nothing more, so the parent stays open until the whole change has landed. Don't move that closing reference to an earlier PR unless the operator asks for it.

## Quirks worth knowing

### Relative-path fixture constants don't move themselves

Test files and YAML fixtures hand-encode `../` depth to reach the repository root or a shared `testdata/` directory. Moving either endpoint of one of these paths changes the required `../` count by exactly the number of levels moved, and nothing catches a wrong count except actually running the test - `go vet` only compiles, it doesn't execute. Recompute depth from the final location of both the file and its target, not from an intermediate state, if the move happens in more than one step.

### A `test/` path is not hidden from Scorecard

OpenSSF Scorecard's `isTestdataFile` logic excludes anything under a `src/test/` or `testdata/` prefix from every check, by path convention. A plain `test/` prefix gets no such exclusion - so don't rely on a `test/`-adjacent path to keep something out of a check. Read `isTestdataFile` in `checks/fileparser/listing.go` upstream if it matters.

### Deploy pods need a large writable cache volume; the controller needs none

A quirk inherited from cluster-api-provider-cargoship, relocated by the deploy-Job model: Zarf packages are whole-platform OCI artifacts, routinely tens of gigabytes, and each deploy Job pulls its package to pod-local disk before deploying. The controller holds no package cache - its registry traffic is manifest-and-signature reads. The `--cache-path` flag (default `/var/lib/caapz/cache`) names the mount path of the `zarf-cache` volume *inside deploy pods*; the volume behind it defaults to an `emptyDir`, which fills a small node disk silently and surfaces as an OCI pull error, not a disk error. Replace it with a PVC via `spec.deployJob.volumes` sized for the largest package plus its extraction - with the operator, not by guess - and steer deploy pods to big-disk nodes with `spec.deployJob.nodeSelector`.

### `docs/agent/choice-*.md` are constraints, not history

When a decision looks reversible from the code alone, check `docs/agent/` first. These are usually the record of a tradeoff already made on purpose, not an oversight waiting to be cleaned up.
