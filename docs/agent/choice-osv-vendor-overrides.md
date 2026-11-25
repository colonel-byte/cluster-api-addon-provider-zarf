# Why `vendor/` carries two generated `osv-scanner.toml` files

`go mod vendor` copies two files into `vendor/` that have nothing to do with Go: a `requirements-test.txt` under `github.com/theupdateframework/go-tuf`, and a `requirements.txt` under `go.opentelemetry.io/otel`. Next to each of them now sits a generated `osv-scanner.toml` that tells a scanner to skip that ecosystem. This document records why those files exist, why they are generated rather than committed by hand, and what they deliberately do not cover.

## What forced it

The OpenSSF Scorecard `Vulnerabilities` check runs osv-scanner recursively over the whole checkout and scores it in `checks/evaluation/vulnerabilities.go` as `MaxResultScore - numVulnsFound`, floored at zero. There is no severity weighting and no reachability analysis: every finding is one point, so three findings and forty score the same. The check read 2 against eight findings, and `vendor/` supplied five of them.

| Manifest                                                            | Ecosystem | Findings | What it pins                                                   |
| ------------------------------------------------------------------- | --------- | -------- | -------------------------------------------------------------- |
| `vendor/github.com/theupdateframework/go-tuf/requirements-test.txt` | PyPI      | 5        | The Python harness go-tuf runs its own conformance tests under |
| `vendor/go.opentelemetry.io/otel/requirements.txt`                  | PyPI      | 0        | A `codespell` pin for otel's own spelling check                |

None of it is a CAAPZ dependency. Nothing in those files is compiled, executed, installed or shipped; they are in the tree because they happen to sit in the directory of a module CAAPZ imports Go packages from, and `go mod vendor` copies directories. The five findings are three advisories against `requests==2.31.0` and two against `tuf==3.1.0` - true statements about those packages, and false statements about this repository.

The otel row is zero findings today and has an override anyway. A `requirements.txt` with one package in it is one CVE away from costing a point, and the whole purpose of the arrangement is that a new advisory upstream is not an event here.

## Deleting the files does not work, and neither does un-vendoring

`rm -rf vendor && go mod vendor` puts both back; they are part of what the vendoring step produces. Removing them by hand means removing them again after every dependency bump, and a run of `go mod vendor` between the removal and the Scorecard run silently undoes it.

Dropping `vendor/` entirely would have removed the problem at the root and was never a candidate. The tree is vendored so the repository builds in an air-gapped management cluster with no route to a module proxy; that constraint is the reason `vendor/` exists at all, and it is not negotiable for a scorecard point.

## The config has to sit beside the manifest

osv-scanner resolves configuration relative to the file it is scanning, not to the root of the scan, and it only consults a single root config when the caller passes `ConfigPath`. Scorecard's `clients/osv.go` builds `osvscanner.ScannerActions` without one. A single `osv-scanner.toml` in the repository root therefore changes nothing at all - which is what anyone would try first, and the reason the files are scattered through `vendor/` instead of sitting in one obvious place at the top of the repository.

## Renovate cannot do this, which is the other thing people try

Renovate is the obvious place to look, because it already owns every dependency bump here. It cannot help, and the reason is worth writing down so nobody re-derives it.

Renovate's vulnerability surface - `vulnerabilityAlerts`, `osvVulnerabilityAlerts`, and the `osvVulnerabilitySummary` on the dependency dashboard - decides only *whether Renovate proposes a bump*. Suppressing something in Renovate means a `packageRules` entry with `enabled: false`, or `ignoreDeps`: no justification field, and visible to nothing but Renovate. The Scorecard alert is raised by osv-scanner, which reads `osv-scanner.toml` and nothing else. Different consumer, so there is no Renovate setting that reaches it.

It is worse than merely unhelpful: [`.github/renovate.json5`](../../.github/renovate.json5) has `**/vendor/**` in `ignorePaths`, so Renovate never sees these two files in the first place. A vendored module's own manifests are upstream's, not ours to bump.

## Ecosystem-wide overrides, not lists of advisory IDs

`[[IgnoredVulns]]` suppresses named advisories and would work. It was rejected because the claim being made here is not "these particular advisories are harmless". It is "this ecosystem is not part of the build", which is a property of the manifest and stays true as the advisory list changes. An ID list would need a commit every time a new CVE landed against a requirements file nobody here reads, and each of those commits would look like a security decision while being pure bookkeeping.

`[[PackageOverrides]]` with only `ecosystem` and `ignore = true` says the intended thing once. Each generated file carries a `reason` and a comment explaining itself to whoever finds it while reading vendored third-party source.

## Generated by mage, not committed by hand

[`magefiles/pkg/osv/osv.go`](../../magefiles/pkg/osv/osv.go) holds the two entries and the text they render to. `dev:writeOSVOverrides` writes them; `dev:vendor` ends with it, so the normal way of updating dependencies leaves them in place without anybody remembering. Committing them as ordinary files was the alternative, and it fails the first time somebody runs `dev:vendor` and commits the result.

`dev:verifyVendor` is the guard, and it is wired into both the `verify-vendor` pre-commit hook and [`.github/workflows/check-go-mod.yaml`](../../.github/workflows/check-go-mod.yaml). It fails if a declared override is missing or has drifted from the generator, and - the half that matters more - it fails if `vendor/` contains a non-Go dependency manifest that no entry covers. Without that second check, a dependency bump that happens to pull in a module carrying a `Gemfile.lock` would drop the score on some later Scorecard run with nothing in the diff to point at. It fails on the commit that caused it instead. It also fails when a manifest an entry names has gone away, which is what turns a stale entry into a build error rather than a file quietly doing nothing.

`check-go-mod.yaml` needed a second change for the same reason. Its first step re-runs `go mod tidy` and `go mod vendor` and then diffs the result against the tree, which deletes both overrides and reports them as drift. `dev:writeOSVOverrides` runs between the vendor and the diff; because it is the same target `dev:vendor` ends with, a properly vendored tree comes out byte-identical and the diff still reports only real changes.

## Renovate puts them back with a post-upgrade task, not a workflow

Generating the overrides from `dev:vendor` assumes the normal way of updating a dependency goes through mage. It does not: Renovate re-runs `go mod vendor` itself for every gomod update, the recreated tree arrives without the two files, and `dev:verifyVendor` fails the pull request. With 170-odd indirect updates queued behind `prHourlyLimit`, that is a hand-written `ci: readd the osv scanner artifacts` commit on every one of them.

Renovate here is self-hosted, out of [`.github/workflows/renovate.yaml`](../../.github/workflows/renovate.yaml), and that is what makes the fix a config entry rather than machinery. `postUpgradeTasks` runs a command in the branch workspace after the updates land, and it is a self-hosted-only feature precisely because it executes arbitrary commands - the hosted app cannot offer it. `.github/renovate.json5` names the command and the paths it is allowed to write; the workflow sets `RENOVATE_ALLOWED_COMMANDS`, because `allowedCommands` is global-only config that Renovate refuses to read from a repository file. Both halves are required, and a command that does not match the allowlist is skipped with a warning in the log rather than an error.

The sister project [cargoship](https://github.com/colonel-byte/cargoship) solves the same problem with a `pull_request_target` workflow that builds a commit through the git data API and pushes it onto the branch, because its bumps come from Dependabot and Dependabot offers no hook of this kind. That is roughly a hundred lines of workflow, a `pull_request_target` trigger, and a second GitHub App token with write access to pull request branches - all of it avoided here by Renovate being self-hosted. If this repository ever moves back to the hosted Renovate app or to Dependabot, that workflow is the thing to port, because `postUpgradeTasks` stops working the moment Renovate is not ours.

Two cases are deliberately left to fail. A bump that drops a module entirely leaves no directory to copy into, and a bump that pulls in a module carrying an uncovered lockfile trips the second half of `dev:verifyVendor`. Both need `Overrides` edited by a person, so the task cannot fix either and the check reports instead.

## What this does not do

It does not raise the check to 10. What remains is Go, and Go is where an override would be wrong: an advisory against a module that is actually in the binary is the thing this check exists to report. Re-derive these rows from `go.mod` and the advisory ranges before quoting them - they are claims about advisory metadata rather than about this repository's code, which is exactly the kind of claim that goes stale without anything here changing.

| Advisory       | Module                                               | Status                                                                                                                                                                                                                   |
| -------------- | ---------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `GO-2026-6778` | `github.com/docker/distribution v2.8.3+incompatible` | Not linked into the manager - `go mod why` reaches it only through `docker/cli`'s `internal/test`, and it is absent from `go list -deps ./cmd/...`. Fixed in an untagged pseudo-version, so Renovate will not propose it |
| `GO-2026-6905` | `github.com/blakesmith/ar`                           | Linked, by way of syft's Debian package cataloger. The advisory files one range with no fixed version, so there is nothing to bump to                                                                                    |
| `GO-2026-6225` | `github.com/chrismellard/docker-credential-acr-env`  | Linked, by way of zarf's registry credential chain. No fixed version either. Harmless in an air-gapped deployment with no Azure registry, but that is a deployment property and not a reason to override the advisory    |

More broadly, none of this makes anything safer. `govulncheck ./cmd/...` reported zero reachable vulnerabilities before the overrides existed and reports zero after; what changed is that a check which was describing go-tuf's test harness is now describing CAAPZ. That is worth the two files and the mage target, and it is worth being clear that it is a measurement fix and not a security fix. The maintenance cost is real and lands on whoever next adds a dependency that carries a lockfile - `dev:verifyVendor` exists so that person gets a clear failure rather than a mystery.
