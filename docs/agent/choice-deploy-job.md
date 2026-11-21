# A deploy runs in its own Kubernetes Job, and verification precedes it

Every package deploy (and removal) runs in a Kubernetes Job the controller stamps - one per `ZarfPackageRelease` - never in the controller process. The controller resolves the package's tag to a digest and verifies its Cosign signature at resolution time, before any Job exists; the Job pulls that verified digest. Both halves were decided together, on 2026-11-20/21, and constrain the deploy subcommand, the resolver, and the manager deployment manifests. The user-facing contract is `docs/dev/architecture.md` §6 steps 4 and 6; this file is the why.

## Why a Job rather than the in-process SDK

- **Zarf's deployer selects its target cluster from the process environment.** `packager.Deploy` (zarf v1.0.1, `src/pkg/packager/deploy.go`) takes no cluster handle: the unexported deployer calls `cluster.NewWithWait`, which resolves via `clientcmd.NewDefaultClientConfigLoadingRules()` - the `KUBECONFIG` environment variable. In-process, the only multi-cluster workaround is swapping a process-global variable under a mutex, which serializes every deploy fleet-wide and races anything else reading client-go loading rules. In a Job, "process environment" is a per-pod property: mount the `<cluster-name>-kubeconfig` secret, point `KUBECONFIG` at it, done. Parallelism across clusters is pods, not a scheduler we maintain.
- **Package `actions` are arbitrary command execution on the deploying machine.** In-process, every package author gets code execution inside the pod whose service account can read every cluster's kubeconfig secret. In a Job, the blast radius is one pod holding one cluster's kubeconfig and a minimal service account.
- **Deploy lifetime decouples from controller lifetime.** Packages are routinely tens of gigabytes and deploys run for tens of minutes; actions can legitimately block. A controller restart, leader-election flip, or OOM no longer kills in-flight deploys - the reconciler observes the Job it finds.
- **The package cache becomes a Job-pod concern.** The largest-package disk mount moves off the controller and onto deploy pods, which `spec.deployJob.nodeSelector` can target at big-disk nodes.

What it costs: a `deploy` subcommand in the controller's own image (no separate zarf binary to ship or pin), result plumbing from Job to release status, per-deploy spec handoff, and worse default cache reuse (per-Job `emptyDir`; a `zarf-cache` PVC volume is the escape hatch). The retry owner is the reconciler - Jobs carry `backoffLimit: 0` - so the release stays the single record of attempts.

This forecloses the in-process SDK path. If zarf later grows a cluster-injection API (unchecked past v1.0.1), reasons two through four still stand; do not unwind this for that alone.

## Why verification sits in the controller, not the Job

The two candidate orderings were: the Job pulls and verifies (verification code lives beside the cache it needs), or the controller verifies first and the Job pulls what was verified. The second won, with one refinement that removes its apparent cost:

- **Verifying a Cosign signature needs the manifest and the signature artifact, not the package layers.** The controller already resolves tag → digest (`status.resolvedDigest`); verifying against that digest is a small registry read. The controller therefore needs no package-sized disk, which was the only reason to prefer in-Job verification.
- **Content addressing closes the gap between verify and deploy.** The Job pulls `repository@digest` - never a tag - so the artifact that was verified and the artifact that is deployed are the same bytes by construction. There is no re-push window between the controller's check and the Job's pull.
- **The trust boundary lands where it should.** Unverified content never reaches a pod holding a cluster's admin kubeconfig; refusal happens before a Job is stamped, as a condition on the release, where an operator looks - not inside a Job log. A pull-in-Job-then-verify ordering would rely on the subcommand sequencing pull, verify, extract correctly inside the same process that later executes package actions.

The deploy subcommand may re-verify on load (`LoadOptions.VerifyBlobOptions`) as cheap defense in depth; that is an implementation choice, not part of this contract. What *is* part of it: the Job's `--package` argument carries a digest reference, and a Job must never be stamped from an unresolved or unverified proxy.
