# Check for go mod changes

[`.github/workflows/check-go-mod.yaml`](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/blob/main/.github/workflows/check-go-mod.yaml) is what makes the vendored, offline build a requirement rather than a convention. It runs on every pull request and on the merge queue, and it fails for one reason: `go.mod`, `go.sum` and `vendor/` do not agree with each other.

## The three steps

- **`go mod tidy`, `go mod vendor`, then `git diff --exit-code go.mod go.sum vendor/`.** Re-resolves the graph and re-vendors from scratch, then fails if the result differs from what was committed. This catches a dependency added without running `mage dev:vendor`, and a `vendor/` left behind by a hand-edited `go.mod`.
- **`go run ./magefiles/core dev:verifyVendor`.** Runs `go mod verify`, then builds with `GOFLAGS=-mod=vendor GOPROXY=off` - the invocation a release has to survive. This catches a vendor tree that is internally consistent but incomplete.

- **`go mod tidy` then `git diff --exit-code go.mod go.sum`, inside `test/e2e`.** That suite is a separate module and deliberately unvendored, so a nested module is invisible to the root's `./...` and the first step cannot reach it. It drifts anyway, and not because anyone edits it: it requires this repository's own module through a `replace ../..`, so every dependency the root module moves changes what the e2e module's graph resolves to. Tidy only - vendoring it would bloat every clone to protect an offline-build invariant that does not apply to a dev/CI-only surface.

The `go run ./magefiles/core` form rather than `mage`: same targets, builds from `vendor/`, needs no mage binary on the runner.

## Why both

They fail on different mistakes, and the second is the one that costs an afternoon. A `vendor/` missing a file still builds locally, because the file is on disk - it is only a fresh clone that notices. The commonest cause is not a forgotten `go mod vendor` at all, but a `.gitignore` pattern that quietly excluded part of the vendor tree; see "A new `.gitignore` pattern can silently empty part of `vendor/`" in [`magefiles/AGENTS.md`](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/blob/main/magefiles/AGENTS.md).

That check is one command, and it is worth running by hand when a pattern changes:

```sh
git ls-files --others --ignored --exclude-standard -- vendor/
```

It should print nothing.

## The e2e module was unmanaged as well as unchecked

Worth knowing because the two halves hid each other: `config:recommended` ships an `ignorePaths` that includes `**/test/**`, so Renovate never saw `test/e2e/go.mod` either. Nothing updated it and nothing checked it, and the drift surfaced only when `go vet` inside that directory began refusing to run. [`renovate.json5`](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/blob/main/.github/renovate.json5) now restates `ignorePaths` without that entry, keeping `**/vendor/**` - a vendored module's own `go.mod` is upstream's, not ours to bump.

## When it fails on a bot's pull request

A dependency update re-vendors as part of the change, so the first step passes. Where it does not - a module that moved import paths, say - the fix is a human running `mage dev:vendor` on the branch, not a rerun.
