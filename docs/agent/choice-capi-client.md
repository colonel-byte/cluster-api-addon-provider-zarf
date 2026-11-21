# The controller reads Clusters through CAPI's typed API module

The controller imports `sigs.k8s.io/cluster-api/api` - the types-only module CAPI split out at v1.11 - and reads `Cluster` objects as `clusterv1.Cluster` from `api/core/v1beta2`. It does not read Clusters as `unstructured.Unstructured` or `PartialObjectMetadata`, and it does not import the main `sigs.k8s.io/cluster-api` module. Decided 2026-11-21, before the first functional reconcile milestone, which this gates. The pin is v1.14.4, matching cluster-api-provider-cargoship.

## Why typed rather than unstructured

- **The v1beta2 contract is a set of field paths, and hand-encoding them is the failure mode the contract pass just fixed.** The architecture doc moved from `status.controlPlaneReady` (v1beta1) to `status.initialization.controlPlaneInitialized` (v1beta2) precisely because a string path written from memory had drifted across contract versions. Unstructured access re-encodes every such path as a string literal (`NestedBool(obj.Object, "status", "initialization", "controlPlaneInitialized")`) that compiles against any contract, wrong or right. The typed struct makes the next contract migration a compile error instead of a silent false.
- **The sister repo has already validated the exact pin combination.** cluster-api-provider-cargoship pins `sigs.k8s.io/cluster-api/api v1.14.4` against the same `controller-runtime v0.25.3` and `apimachinery v0.37.2` this module pins, so the compatibility question the vendored build makes expensive to discover late is already answered in production next door. Mirroring its pin also keeps the two providers' Renovate upgrades in lockstep.
- **The surface only grows from here.** The reconcile core starts with labels, `controlPlaneInitialized`, and ownerReferences, but §6 already commits to reading conditions (`ControlPlaneInitialized`), `status.phase` for deletion handling, and the `concurrent-deploys` annotation - and envtest needs the real types to build fixture Clusters. An unstructured shim that is tolerable for three fields is not tolerable for the full §6/§7 read set, and converting mid-implementation would churn every reconciler layer above it.

What it costs: a vendored dependency carried by every clone. The types-only module bounds that - its dependency graph is apimachinery-sized, not the main module's (which links the full CAPI controller machinery, webhooks, and feature gates). `go list -deps` the import before widening it.

## The boundary: types module yes, main module not yet

Only `sigs.k8s.io/cluster-api/api` is imported. The main `sigs.k8s.io/cluster-api` module's `util/*` helpers (`patch`, `conditions`, `predicates`) are genuinely useful and cargoship imports them - but each is a separate, later decision with its own vendor delta, taken when a reconciler actually wants the helper, not pre-emptively. Nothing here forecloses adding it; this file records that its absence today is deliberate, not an oversight.
