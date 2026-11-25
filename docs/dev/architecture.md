# Architecture Proposal: Cluster API Add-on Provider for Zarf (CAAPZ)

Inspired by the design of [cluster-api-addon-provider-helm (CAAPH)](https://github.com/kubernetes-sigs/cluster-api-addon-provider-helm), this document proposes a declarative Cluster API Add-on Provider for [Zarf](https://github.com/zarf-dev/zarf) packages: `cluster-api-addon-provider-zarf` (CAAPZ).

The goal of CAAPZ is to automate the deployment, day-2 lifecycle, and one-time initialization ("seed") of self-contained Zarf packages onto CAPI workload clusters across air-gapped enterprise environments.

---

## 1. High-Level Architecture

CAAPZ operates in the management cluster as an add-on controller. It watches CAPI `Cluster` resources, resolves and verifies packages in an internal OCI registry by digest, and stamps one Kubernetes deploy Job per cluster - the Job mounts that cluster's kubeconfig and drives Zarf deployment via Zarf's Go SDK against the workload cluster API (§6 step 6). The controller itself never deploys.

```
┌────────────────────────────────────────────────────────────────────────┐
│                        Management Cluster                              │
│                                                                        │
│   [ CAPI Cluster: example-cluster ] ─ (provides remote kubeconfig)     │
│             │                                                          │
│             ▼                                                          │
│   [ ZarfPackageProxy ] ◄── (defined by platform team / GitOps)         │
│             │                                                          │
│     ┌───────▼────────────────────────────────────────────────────┐     │
│     │ CAAPZ Controller                                           │     │
│     │                                                            │     │
│     │ 1. Watches Clusters & ZarfPackageProxy                     │     │
│     │ 2. Reads the internal Zarf OCI mirror                      │     │
│     │ 3. Resolves digest, verifies signature                     │     │
│     │ 4. Stamps a deploy Job per cluster                         │     │
│     │ 5. Job runs Zarf deploy via kubeconfig                     │     │
│     │ 6. Creates & tracks per-cluster ZarfPackageRelease CR      │     │
│     └──────────────────────────────┬─────────────────────────────┘     │
└────────────────────────────────────┼───────────────────────────────────┘
                                     │ (Remote Kube API)
                                     ▼
                      ┌────────────────────────────┐
                      │ Workload Cluster (example) │
                      │                            │
                      │   [ zarf-init / agent ]    │
                      │   [ seed packages ]        │
                      │   [ platform workloads ]   │
                      └────────────────────────────┘
```

---

## 2. Declarative CRD Design

CAAPZ separates cluster selection and configuration templates from per-cluster release status tracking, following CAAPH's `HelmChartProxy` / `HelmReleaseProxy` pattern.

### A. Cluster-Wide Proxy: `ZarfPackageProxy`
Managed on the management cluster to declare desired package states across matching clusters:

```yaml
apiVersion: addons.colonel-byte.dev/v1alpha1
kind: ZarfPackageProxy
metadata:
  name: security-baseline
  namespace: fleet-default
spec:
  # Cluster matching
  clusterSelector:
    matchLabels:
      environment: production
      cargoship.colonel-byte.dev/profile: mission

  # Package source specification
  package:
    repository: zarf-docker-registry.zarf.svc.cluster.local:5000/packages/security-baseline
    version: "1.4.2"
    # Cosign signature verification before extracting/applying
    signatureKeySecretRef:
      name: cosign-pubkey

  # Deployment strategy: Reconcile (continuous) vs ApplyOnce (seed)
  strategy: Reconcile

  # Explicit dependency ordering across ZarfPackageProxies
  dependsOn:
    - zarf-init
    - csi-ceph

  # Zarf initialization parameters if target cluster is not initialized
  init:
    autoInit: true
    # The init package to deploy - a full package source, resolved, digest-pinned, and
    # signature-verified exactly like spec.package. Required when autoInit is true.
    package:
      repository: zarf-docker-registry.zarf.svc.cluster.local:5000/packages/init
      version: "v1.0.1"
      signatureKeySecretRef:
        name: cosign-pubkey
    # Variables for the init deploy, same semantics as spec.set (v1alpha1 packages only; the
    # upstream init package published at v1.0.1 is v1alpha1). A v1beta1 init package takes
    # init.values instead - e.g. registry.persistence.storageClass.
    set:
      REGISTRY_STORAGE_CLASS: "ceph-bucket"

  # Component filtering
  components:
    - name: agent
    - name: logging
    - name: security-rules
      flavor: enterprise-linux

  # Native zarf.dev/v1beta1 structured values override (supports CAPI templating)
  values:
    logging:
      retentionDays: 30
      storage:
        storageClassName: "{{ .Cluster.metadata.annotations['storage.infra/class'] }}"
```

(`spec.set`, the legacy variable mechanism, applies only to `v1alpha1` packages and requires declaring `spec.package.api: v1alpha1` - see §4C.)

### B. Per-Cluster Release: `ZarfPackageRelease`
Created by the controller in the cluster's namespace and owned by the target `Cluster`:

```yaml
apiVersion: addons.colonel-byte.dev/v1alpha1
kind: ZarfPackageRelease
metadata:
  name: example-security-baseline
  namespace: fleet-default
  ownerReferences:
    - apiVersion: cluster.x-k8s.io/v1beta2
      kind: Cluster
      name: example-cluster
spec:
  clusterRef:
    name: example-cluster
  packageProxyRef:
    name: security-baseline
status:
  phase: Deployed
  deployedVersion: "1.4.2"
  conditions:
    - type: Ready
      status: "True"
    - type: PackageSignatureVerified
      status: "True"
```

### C. Cross-Namespace Selection Is Flag-Gated, Same-Namespace by Default

By default a proxy's `clusterSelector` matches only Clusters **in the proxy's own namespace**, and every object reference resolves in the referrer's namespace. This diverges from CAAPH deliberately: the controller reads and deploys with a cluster-wide role, so a cross-namespace selection is a request the controller performs with *its* permission, not the proxy author's - allowing it by default would let anyone who can create a `ZarfPackageProxy` in a namespace they own deploy arbitrary packages onto clusters in namespaces they do not. (This mirrors cargoship's `--secret-namespaces` design; see its `docs/agent/choice-cross-namespace-secrets.md` for the full argument.)

Two manager flags open it up, each a comma-separated namespace list where `*` permits any, empty (the default) permits none:

- `--cluster-namespaces` - namespaces beyond its own that a proxy may select Clusters in. When a release is created in another cluster's namespace, its `packageProxyRef.namespace` points back at the proxy.
- `--secret-namespaces` - namespaces a proxy may name its `signatureKeySecretRef` in besides its own, matching cargoship's flag of the same name.

A selection or reference into a namespace the flags do not permit is **an error surfaced as a condition with `reason: CrossNamespaceNotEnabled`** - never a quiet fallback to the local namespace, because resolving a different object than the one named is worse than refusing: it succeeds. Opening up later is a flag value; closing down later breaks every fleet that relied on it, which is the asymmetry the default follows from.

Two operational caveats, documented rather than startup-validated (a permitted-but-unused configuration should not refuse to start):

- `clusterctl move` moves what a Cluster owns; a proxy or secret in another namespace is not that, and arrives missing on the new management cluster.
- A manager whose cache is scoped to one namespace reads every cross-namespace referent as `NotFound`, whatever the flags permit.

---

## 3. The "Seed" Package Pattern (ApplyOnce)

An ideal capability for this architecture is supporting **Seed Packages** that run **strictly once per imported or provisioned cluster**:

### Motivation
Certain foundational packages perform initialization tasks that must not be continuously reconciled or reverted on subsequent reconciliation loops, such as:
1. One-time cluster credential seeding or bootstrap tokens.
2. Initial root user/admin setup.
3. Foundational certificate generation or initial storage class bindings.
4. One-time registration or enrollment jobs.

### Implementation via `strategy: ApplyOnce`
Borrowing from core CAPI's `ClusterResourceSet.spec.strategy`, CAAPZ supports two strategies:

1. **`strategy: Reconcile` (Continuous Lifecycle)**:
   - Reacts to changes in `spec.package.version`, values, and variables by redeploying the package to converge the workload cluster to the new state. The package content itself is pinned by digest (see "Package identity" below); following a mutable tag is an explicit per-proxy opt-in, polled rather than watched - there is no OCI watch primitive.
   - "Drift repair" is periodic idempotent redeploy of the pinned digest on the controller's resync interval; the Zarf SDK has no drift detection.

2. **`strategy: ApplyOnce` (Seed / Initialization Package)**:
   - When a new cluster matching the selector is discovered or imported, a deploy Job (§6 step 6) pulls and applies the Zarf package.
   - Once the deployment succeeds, the `ZarfPackageRelease` is marked `phase: Completed` and `applied: true`.
   - The controller will **never re-execute** this package against that cluster, even if the proxy's spec or variables change.
   - Ideal for Day-0/Day-1 cluster bootstrap hooks and registration payloads.

#### Package identity: a tag resolves once to a digest

`spec.package.version` is an OCI tag, and tags are mutable; the deployed fleet must not be. So the tag is an *input to resolution*, not the package's identity:

- On first resolution - proxy creation, or any edit to `spec.package.repository`/`version` - the controller resolves the tag to its digest and records it in `status.resolvedDigest`. Every subsequent pull, Cosign verification, and deploy uses **the digest, not the tag** (Zarf resolves digest references natively), and each release records the digest it actually deployed. A re-pushed tag therefore cannot change what the fleet runs, even in principle.
- The controller still re-checks the tag on reconcile. When the registry's tag no longer matches the pinned digest, the proxy gets a warning condition with `reason: TagContentChanged`, naming both digests. Pinned deploys continue untouched; the new content is **not** adopted. The update path is the one that already exists: bump `spec.package.version`, which re-resolves and re-pins.
- In an air-gapped registry that is periodically re-staged, a re-push of the same tag trips `TagContentChanged` on every re-stage. That is working as intended: the operator's move is a version bump (or the opt-in below), never silent adoption.

**Following a mutable tag is an explicit, per-proxy opt-in via annotations** - an operational/dev toggle for connected test environments, deliberately kept out of the GitOps-owned spec:

| Annotation                                    | Meaning                                                                                                                                       |
| --------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| `addons.colonel-byte.dev/follow-tag: "true"`  | Re-resolve the tag on a timer; a digest change re-pins `status.resolvedDigest` and triggers redeploy                                          |
| `addons.colonel-byte.dev/follow-tag-interval` | Poll interval as a Go duration. Default `15m`; meaningless without `follow-tag`; invalid values fall back to the default with a warning event |

ApplyOnce proxies take no part in re-resolution or tag-following: their one deploy pinned what it pinned.

The `ZarfPackageRelease` is a cache of the guarantee, not the guarantee itself: it is deliberately garbage-collected with its owning `Cluster` (§7), and a `Cluster` object is deleted *without the infrastructure being wiped* during `clusterctl move` and de-import/re-import - exactly the imported-cluster flows seed packages target. The durable record is the one Zarf itself writes on the workload cluster: every deploy stores a `zarf-package-<name>` secret (labeled `package-deploy-info`) in the `zarf` namespace, which survives management-cluster churn because it lives on the physical cluster being guarded.

So the seed check is two-tier:

- `status.applied: true` on an existing release short-circuits the reconcile with no workload-cluster round trip.
- When the release is missing or not yet applied - the only states in which a deploy would fire - the controller first probes the workload cluster for the package's deployed-package secret. If present, the release is marked `Completed`/`applied: true` **without re-executing** (adoption); only a cluster with no record is seeded.

Consequences, all deliberate:

- Re-importing a live cluster re-creates its releases but does not re-run its seed packages.
- Hand-deleting a `ZarfPackageRelease` does not re-run the seed; the recreated release adopts the existing record.
- Intentionally re-seeding takes **two deletions, record first**: remove the package's record on the workload cluster (`zarf package remove`, or deleting the `zarf-package-<name>` secret), *then* delete the `ZarfPackageRelease`. The release alone is not enough - the recreated release adopts the surviving record - and the record alone is not enough either, because `status.applied: true` short-circuits without probing. Deleting the release before the record just re-adopts.
- Wiping and rebuilding infrastructure behind the same `Cluster` object re-seeds, because the rebuilt cluster carries no record. That is the desired reading of "once per physical cluster".

One restriction follows: Zarf only stores the deployed-package record when the package has at least one cluster-touching component, so **an ApplyOnce package must include one**. A pure-actions package would leave no durable record, silently degrading the guarantee to the garbage-collectable release; the controller surfaces a condition on the release when a completed seed deploy left no record behind.

#### Seed-then-manage handoff, and the package name as identity

A supported pattern (used by the sister `ansible-collection-zarf` tooling): an ApplyOnce proxy seeds a package - `zarf-init`, say - and a Reconcile proxy later takes over the *same package name* for day-2 lifecycle. This works by construction: the Reconcile path never consults the seed record, Zarf treats a same-name deploy as an in-place upgrade updating the same record, and on re-import the seed proxy adopts whatever version the successor last deployed. Two properties to hold on to:

- **Adoption is keyed on the Zarf package name on the cluster - nothing else.** Not the proxy's name, not the declared version or digest. Any ApplyOnce proxy whose package carries that name adopts the record, even if its spec declares different content. Package names are therefore an identity namespace the fleet's conventions must own; two unrelated packages sharing a name will silently satisfy each other's seed checks.
- **A successor proxy for a seeded package must be `deletionPolicy: Retain`** (§8). Under the default `Delete`, deleting the successor runs `zarf package remove` on the shared package name - destroying both the package and the deployed-package record the seed's guarantee rests on, while the seed's release still reports `Completed` and its `applied` cache suppresses any re-seed. The cluster loses the package; nothing notices. `Retain` on the successor keeps the handoff safe.

#### Composition guidance: separate packages by default, bundle into init only for storage bootstrap

Heterogeneous fleets (one segment wants vsphere CSI, another rook-ceph) are the reason foundational packages stay *separate* from the init package by default - the layout the CRDs already express with no extra machinery:

- One shared init proxy (or `autoInit`), identical fleet-wide; per-variant proxies (`csi-vsphere`, `csi-rook-ceph`) with disjoint `clusterSelector`s keyed on the cluster's existing labels, each with `dependsOn: [zarf-init]`. The DAG and the init-first eligibility gate (§6) order everything; a cluster gets the variant its labels declare. Where variants share most content, Zarf `flavor`s via `spec.components` let two thin proxies select from one package.
- **Bundling components into a custom init package is the escape hatch for one constraint only**: storage that must exist before init can finish - Zarf's internal registry needing a real `StorageClass` with nothing provisioning one pre-init. Zarf's component `import` mechanism composes this (a custom `ZarfInitConfig` importing, say, rook-ceph's components); the cost is per-variant init packages, paid knowingly by the fleet segment that needs it. The decision rule per segment: *can init complete on this cluster's day-zero storage?* Yes - shared init, selector-driven variants. No - that segment carries a variant init package. The init package's own storage-class knob (`init.set`'s `REGISTRY_STORAGE_CLASS`, or its v1beta1 value equivalent) bridges the two where a StorageClass exists at provision time.
- **Day-2 handoff of bundled components follows the package name**, per the handoff rules above. The clean path is one `Retain` Reconcile proxy managing the custom init package itself - same name, same record, in-place upgrades. Splitting a bundled component out to its own proxy later changes the package name: no adoption, no shared record, and the first deploy collides with Helm releases the init components own. Zarf's `DeployOptions.TakeOwnership` exists for exactly that adoption, which implies a proxy-level `takeOwnership` knob - **not in the API today; decide whether to add it before v1alpha1 ships to real users**, since the name-discontinuous handoff is impossible without it.

---

## 4. Native `zarf.dev/v1beta1` Package Support

CAAPZ natively implements the modern `zarf.dev/v1beta1` Zarf package schema while maintaining backward compatibility with `zarf.dev/v1alpha1`:

### A. Native Deployment, No Conversion
- When pulling a package from an OCI registry, CAAPZ inspects the `apiVersion`. A proxy may assert it up front via `spec.package.api` (`v1alpha1` or `v1beta1`); when the declared version does not match the pulled artifact, the release fails with a condition before any deploy runs.
- Packages deploy **as their native schema version** - CAAPZ performs no in-memory conversion. Converting a `v1alpha1` package to `v1beta1` before deploy would trip Zarf's rejection of deploy-time variables on `v1beta1` packages and silently break `spec.set`; Zarf's deployer handles both versions natively.
- **Deprecation:** `v1alpha1` package support exists for compatibility and will be dropped when upstream Zarf drops `v1alpha1` support. New packages should be authored as `v1beta1` and configured through `spec.values`.

### B. Hierarchical Structured Values (`spec.values`)
In `v1beta1`, Zarf supports structured values maps (analogous to Helm `values.yaml`). CAAPZ exposes `spec.values` directly on `ZarfPackageProxy`:
- Values accept full YAML trees rather than only string scalars (`spec.set`).
- Supports Go template substitution referencing CAPI `Cluster` metadata and annotations (e.g. `{{ .Cluster.metadata.name }}`).
- Merged with the package's internal defaults at deployment time.

**Deferred, additive later: sourcing values from a Secret or ConfigMap** (`valuesFrom`, CAAPH/Flux-style). The seam already exists - the deploy Job reads only the rendered deploy-spec Secret, never the proxy - so a future `valuesFrom` is purely a controller-side resolution step at render time, merged into the same `Values`/`Set` fields; the deploy pod and subcommand need no change, and a changed source changes the rendering, so the `spec-hash` redeploy path picks it up for free. Adding the optional field later is API-compatible (no version bump), and `signatureKeySecretRef`'s `CrossNamespaceNotEnabled` refusal is the reference-shape and namespace-policy precedent to reuse. The future change owes three things: ConfigMap get/list/watch RBAC (Secrets are already covered), an answer for "the referenced source changed but the proxy didn't" (the lazy version is the periodic resync re-render, consistent with the drift-repair item owed from M3; immediate reaction needs a watch plus mapping), and precedence semantics between inline and referenced values (Flux's "later entries win, inline wins last" is the boring default).

### C. Legacy Variables (`spec.set`) Are Gated on a Declared `v1alpha1` Package
Zarf rejects deploy-time variables (`SetVariables`) for `v1beta1` packages, so `spec.set` is only valid against `v1alpha1` packages - and the CRD enforces that at admission:

- `spec.package.api` is an optional enum (`v1alpha1` | `v1beta1`). Unset means "detect from the artifact".
- Using `spec.set` **requires** declaring `spec.package.api: v1alpha1`. A CEL validation rule on the CRD rejects the object at `kubectl apply` otherwise, turning a deploy-time Zarf error buried in a Job log into an immediate admission message. (CEL CRD validation is GA from Kubernetes 1.29, the management-cluster floor this sets.)
- The declaration is a claim: the controller verifies it against the pulled artifact's actual `apiVersion` and fails the release with a condition on mismatch, before stamping a deploy Job.
- `spec.set` values support the same Go templating as `spec.values`, and the field is deprecated on the same schedule as `v1alpha1` support itself (§4A).

### D. Architecture & Flavor Auto-Detection
`v1beta1` formalizes component selectors with `flavor` and `architecture`. CAAPZ automatically extracts the target cluster's architecture from CAPI `Machine` specifications (`amd64`, `arm64`) and selects the appropriate component variants automatically unless explicitly overridden in `spec.components`.

---

## 5. Dependency Management & Execution Ordering (`dependsOn`)

Foundational platform services require strict execution ordering. For example, workloads cannot schedule PersistentVolumeClaims before storage drivers exist, and Zarf packages cannot deploy before the cluster has a running registry or injector:

```
[ zarf-init ] ──► [ csi-ceph (CSI Driver) ] ──► [ logging / monitoring ]
```

### A. Declarative Ordering via `spec.dependsOn`
A `ZarfPackageProxy` can declare dependencies on other `ZarfPackageProxy` resources:
```yaml
apiVersion: addons.colonel-byte.dev/v1alpha1
kind: ZarfPackageProxy
metadata:
  name: logging-stack
  namespace: fleet-default
spec:
  dependsOn:
    - zarf-init
    - csi-ceph
```

### B. Dependency Resolution & Reconcile Gating
For each workload cluster, every entry in `dependsOn` is resolved to a verdict. The rule that prevents deadlock: **a dependency that cannot be satisfied on this cluster is a named error condition, never a silent wait.** Waiting is reserved for the one case where progress is actually coming.

| Dependency state on this cluster                          | Verdict   | `DependenciesSatisfied` reason |
| --------------------------------------------------------- | --------- | ------------------------------ |
| Release exists in phase `Deployed` (or `Completed`)       | satisfied | -                              |
| Proxy exists, selector matches, release pending or absent | wait      | `WaitingForDependency`         |
| Proxy exists but its selector does not match this cluster | error     | `DependencyNotApplicable`      |
| Proxy does not exist in this namespace                    | error     | `DependencyNotFound`           |

- The dependent release holds in `phase: Pending` with `DependenciesSatisfied: False` and the reason above; the message names the offending dependency.
- `DependencyNotApplicable` is deliberately an error rather than auto-satisfied: declaring "X needs Y here" when Y's selector will never match this cluster is a configuration contradiction, and silently skipping the ordering would hide exactly the label mistake the admin needs to see.
- All verdicts are level-triggered: fixing labels, creating the missing proxy, or the dependency deploying re-evaluates the dependent with no operator intervention. Re-evaluation is watch-driven - the proxy reconciler watches `ZarfPackageRelease` phase changes and maps them back to dependents - with the periodic resync as backstop; there is no requeue polling loop.
- A dependency proxy deleted *after* its ApplyOnce package seeded a cluster reports `DependencyNotFound`, even though the package is on the cluster. This is strict on purpose - keep proxies that the dependency graph references. (Softening it by probing the workload cluster's deployed-package record (§3) is a possible later relaxation, not a commitment.)

### C. Circular Dependency Detection
Directed Acyclic Graph (DAG) validation runs **at reconcile time, not admission** - cross-object graph checks cannot be expressed in CEL, and a validating webhook is not worth its cert plumbing in air-gapped installs when admission-time validation can be invalidated by the very next edit to a *different* proxy anyway. Each reconcile builds the dependency graph from the namespace's proxies (informer cache, no API round trips) and runs a cycle check; every proxy in a detected cycle gets `DependenciesValid: False, reason: DependencyCycle` and deploys nothing until the cycle is broken.

---

## 6. Reconcile Workflow

1. **Cluster Discovery**:
   The controller watches for CAPI `Cluster` resources reaching `status.initialization.controlPlaneInitialized == true` (the v1beta2 initialization signal, surfaced by the `ControlPlaneInitialized` condition) and matching `spec.clusterSelector`.
2. **Kubeconfig Resolution**:
   CAAPZ retrieves the cluster's administrative kubeconfig secret (`<cluster-name>-kubeconfig`) using CAPI's cluster client cache.
3. **Seed Check**:
   If `spec.strategy == ApplyOnce`, skip execution when the release is already `applied: true`. Otherwise probe the workload cluster for the package's `zarf-package-<name>` deployed-package secret in the `zarf` namespace; if present, mark the release `Completed` without deploying (see §3, "Where the 'never re-execute' record actually lives").
4. **Digest Resolution & Signature Verification**:
   The controller resolves the tag to its pinned digest (`status.resolvedDigest`, §3) and, if `signatureKeySecretRef` is set, verifies the Cosign signature against that digest **in the controller, at resolution time** - a manifest-and-signature read against the registry, not a package pull, so the controller needs no package-sized disk. The deploy Job later pulls that verified digest: content addressing makes the verified artifact and the deployed artifact the same bytes. Verification binds to the digest, so a re-pushed tag never inherits a prior verification. See `docs/agent/choice-deploy-job.md` for why verification sits here rather than in the Job.
5. **Auto-Init Check**:
   If `spec.init.autoInit == true`, the controller probes the target cluster for Zarf state (`zarf-state` secret in the `zarf` namespace) at deploy-prep, and only an absent state triggers an init deploy. Auto-init is keyed on *observed cluster state*, never on a package or proxy name - so it commutes with a fleet that manages init through its own proxy (the §3 seed-then-manage handoff): whoever initialized first, the probe finds the state and auto-init no-ops. When both are declared, `dependsOn` naming the init proxy is the deterministic ordering; the remaining concurrent-first-deploy race is closed structurally by step 6's one-Job-per-cluster rule. **Init-first is an eligibility gate, not a priority**: on a cluster with no Zarf state, the only releases eligible for a Job slot are init-capable ones - a package that resolves as an init package (`kind: ZarfInitConfig`, read at resolution) or a proxy with `autoInit: true`; everything else holds in `Pending` with condition reason `WaitingForZarfState` rather than failing inside a Job. With step 6's one-Job-per-cluster rule, "the init package deploys first" is therefore an invariant, not a scheduling preference.

   **The auto-init deploy path.** `spec.init.package` names the init package - a full package source (repository, version, optional `api` claim and `signatureKeySecretRef`), required when `autoInit: true`. The controller resolves and verifies it exactly as step 4 does the main package - pinned to `status.initResolvedDigest`/`initResolvedRef`, surfaced on the `InitPackageResolved` condition - and additionally refuses an artifact that does not resolve to `kind: ZarfInitConfig`. On a state-less cluster, an `autoInit` release stamps an *init Job*: the step 6 Job contract verbatim (same image, kubeconfig, cache volume, `deploy` subcommand) with an `-init` name suffix, reading `init-spec.yaml` rendered into the release's deploy-spec Secret beside the main spec. The init Job takes the one-Job-per-cluster slot like any other Job, so two `autoInit` proxies racing on a fresh cluster serialize, and the loser's next probe finds the state and no-ops - the commuting property above, preserved. Success leaves no status record beyond the probe finding state (the `zarf-state` secret *is* the record, and Zarf keeps its own deployed-package record for the init package); the controller deletes the finished Job and the release proceeds to its normal deploy on the next pass. Failure surfaces as `Ready: False, reason: InitFailed` with the pod's error, and retry is edit-to-retry via the init spec hash - the same stance as deploys. The proxy's *own* package never takes the slot while state is absent: `autoInit` makes the init Job eligible, not the main package. A proxy whose main package itself resolves as `ZarfInitConfig` needs no `autoInit` - it is init-capable as-is, and deploying it initializes the cluster. **An init package whose components are optional deploys nothing until they are named.** Component selection is `spec.init.components`, with `spec.components`' semantics - and it is not the edge case it sounds like: an init package built to bring a cluster up in stages marks every component optional, so the default selection (required components only) is empty. `spec.init.values` and `spec.init.set` pass through to the init deploy exactly as the proxy-level fields do to the main one - same `{{ .Cluster... }}` templating, same v1alpha1-only rule for `set` (keyed on `init.package.api`) - so there is no dedicated storage-class field: it is the `REGISTRY_STORAGE_CLASS` variable in the v1alpha1 init package upstream publishes at v1.0.1, or the `registry.persistence.storageClass` value in a v1beta1 one. CAAPZ never removes an init package it auto-deployed - `deletionPolicy` governs `spec.package` only.
6. **Deploy Execution (the deploy Job)**:
   Render values and variables (`{{ .Cluster.metadata.name }}`) and stamp one Kubernetes Job per release - **a deploy never runs in the controller process**. The Job's contract, which §4C, the Signature Trust Model, and §8A reference:
   - It runs the controller's own image (or `spec.deployJob.image`) with a `deploy` subcommand. **The pod acts as a pure operator: its entire input is files on disk** - a rendered deploy spec the controller writes to a per-release Secret and mounts (package repository pinned by digest, rendered values and variables, component selection), the kubeconfig, and the cache. It holds no management-cluster credentials and makes no management-cluster API calls; the subcommand assembles `packager.DeployOptions` from the spec file alone.
   - Results return in the pod's termination message; the controller reads the finished Job and patches `ZarfPackageRelease.status`. The deploy pod's service account grants nothing.
   - The target cluster's `<cluster-name>-kubeconfig` secret is mounted and `KUBECONFIG` points at it - the Zarf SDK resolves its target from the process environment, so the pod's environment is what selects the cluster. No mutex, no serialization: deploys to different clusters are different pods.
   - The package is pulled inside the Job **by the verified digest from step 4** (`repository@digest`, never a tag) into the Job's `zarf-cache` volume (default `emptyDir`, PVC-replaceable via `spec.deployJob.volumes`).
   - The pod runs a restricted security profile by default; `spec.deployJob` exposes the knobs (service account, security contexts, volumes and mounts, node selector, tolerations). A package `action` - arbitrary command execution - is thereby contained to a pod holding one cluster's kubeconfig, not the fleet-wide controller.
   - `backoffLimit: 0`: the controller owns retries, so the release stays the single record of attempts. The Job is controller-owned by its release and garbage-collects with it; a removal Job (§8) reuses this same contract with a `remove` subcommand.
   - **One Job per cluster at a time.** Upstream Zarf recommends a single package operation per cluster with current tooling, so the controller stamps at most one deploy or removal Job per target cluster; further ready releases hold in `phase: Pending` with condition reason `WaitingForActiveDeploy` until the slot frees. This holds even where the dependency DAG (§5) would permit parallelism - the DAG decides *who goes next*, never simultaneity - and it is what closes step 5's double-init race without naming conventions. Parallelism across *clusters* is unaffected. A CAPI `Cluster` annotated `addons.colonel-byte.dev/concurrent-deploys: "true"` opts that cluster out of the serialization, for when tooling or operator confidence allows it; the annotation lives on the Cluster because concurrency tolerance is a property of the target, not of any one proxy. Bounded concurrency - the annotation carrying a slot *count* rather than a boolean - is deliberately deferred to a later phase of the controller; `"true"` is specified to leave room to grow into an integer without breaking existing annotations.
   Why a Job rather than the in-process SDK - and why verification precedes it - is recorded in `docs/agent/choice-deploy-job.md`.
7. **Status Update**:
   Update `ZarfPackageRelease.status` and surface condition `Ready: True` on the release and proxy objects.

### Registry Reachability

Verification is about *what* the artifact is. Reachability is the separate question of whether the controller can get to the registry at all, and in an air gap the answer usually involves a credential and a certificate authority nothing public signed.

Five fields on `spec.package` - and on `spec.init.package`, which is the same type - cover it:

| Field                   | What it supplies                                                                                               |
| ----------------------- | -------------------------------------------------------------------------------------------------------------- |
| `pullSecretRef`         | A `kubernetes.io/dockerconfigjson` Secret. Empty means anonymous, which was the only option before it          |
| `caBundleSecretRef`     | PEM certificates under `ca.crt`, trusted *in addition to* the system roots - the cert-manager shape            |
| `caBundleConfigMapRef`  | The same, from a ConfigMap. A CA is not secret; the Secret form exists because cert-manager writes one         |
| `fromZarfState`         | Both of the above, read from the management cluster's own Zarf deployment instead of from an operator's Secret |
| `insecureSkipTLSVerify` | Accepts the certificate unverified. Per-package, so one unfixable registry cannot lower the fleet's bar        |

Every reference resolves through the same `--secret-namespaces` gate as `signatureKeySecretRef` (§2C), so a proxy author can only name material their own namespace holds unless the operator widened it.

**The material reaches two consumers by two routes, and that is deliberate.** The controller resolves and verifies the package itself, and passes credentials and a root pool per call - so two proxies naming two registries with two credentials never see each other's. The deploy pod pulls by digest through an upstream API that carries neither, so there the credentials and the CA arrive as files mounted from the release's deploy-spec Secret, with the pod's environment pointing at them. The pod holds no management-cluster access and could not look a Secret up; one pod also serves exactly one package, so a file scoped to the pod is scoped to the pull. `docs/agent/choice-registry-credentials.md` records why, and what would have to change upstream for the pod to use the same hook the controller does.

**`fromZarfState` reads the management cluster, not the workload cluster**, which is the opposite of every other Zarf-state read in this controller. The init gate, probe-and-adopt, and removal all probe a *workload* cluster through its CAPI kubeconfig; this one reads the controller's own cluster, because that is where the registry being pulled *from* lives. A workload cluster's internal Zarf registry is not a package source - its recorded address is node-local, and nothing in the management cluster can route to it.

No client certificate is presented, measured rather than assumed: Zarf's registry serves clients that offer none, and the certificate it issues belongs to the per-node registry proxy. A registry that genuinely requires one needs an upstream change before this provider can carry it.

### Signature Trust Model

`signatureKeySecretRef` adopts the sister cargoship repository's semantics verbatim (its `signatureKeySecretRef` field and the reasoning in its doc comment):

- The reference names a Secret whose `cosign.pub` key holds the Cosign public key. It resolves in the proxy's namespace unless `--secret-namespaces` permits another (§2C), which is what bounds "anyone who can edit the proxy picks the key": a proxy author can only name keys their own namespace holds.
- **Empty verifies nothing.** A signature with no key to check it against is not verification, so the controller logs that it could not verify and deploys anyway - for a signed package exactly as for an unsigned one. The `PackageSignatureVerified` condition is absent, not `True`.
- **Naming a key makes verification a precondition.** An unsigned package, a signature the key does not verify, and a signature in a form that cannot be checked are all refused, and the refusal comes out of digest resolution in the controller (§6 step 4) - **before a deploy Job is ever stamped**, so unverified content never reaches a pod holding a cluster's kubeconfig. The Job then pulls the verified digest; content addressing makes what was verified and what is deployed the same bytes. Verification binds to the pinned digest (§3), so a re-pushed tag never inherits a prior verification.
- **Rotation is a Secret edit.** Verification runs on the passes that deploy, against the key as it currently reads, so a rotated key takes effect on the next reconcile and a Secret deleted under a converged fleet is reported as a failed precondition rather than noticed the next time something changes.
- **Fleet-wide enforcement is deliberately deferred.** Per-proxy keys match the sister repository's model; a controller-level `--require-signature` flag (refuse any proxy with no key named) is the natural later addition if an environment needs "no unverified package, ever" as policy rather than convention.

## 7. Non-Blocking Cluster Deletion & Lifecycle Safety

A critical architectural pitfall in add-on controllers (including earlier versions of CAAPH) is hanging or blocking cluster deletion when a workload cluster is destroyed. CAAPZ explicitly avoids blocking cluster deletion:

### A. Non-Blocking Finalizer Policy
- **No Blocking Finalizer on the CAPI `Cluster`**: CAAPZ does **not** install a finalizer on the CAPI `Cluster` resource itself. When an operator or automation deletes a CAPI `Cluster`, CAPI begins tearing down the infrastructure and VMs immediately.
- **Immediate Reconcile Termination on Deletion**: If `Cluster.DeletionTimestamp.IsZero() == false`, CAAPZ immediately halts any pending deployments, releases any local mutexes or downloads, and exits without attempting to connect to the dying cluster.
- **Cascade via Kubernetes `ownerReferences`**: The per-cluster `ZarfPackageRelease` CR carries a standard `ownerReferences` pointing to the CAPI `Cluster`. When the `Cluster` is deleted, Kubernetes garbage-collects the `ZarfPackageRelease` automatically via background cascading deletion.

### B. Graceful Degradation on Unreachable Clusters
- When an infrastructure provider tears down VMs (or when connectivity to the workload API drops), attempts to uninstall or communicate will fail (`connection refused` or `context deadline exceeded`).
- CAAPZ treats cluster deletion as an infrastructure-level wipe for *teardown* purposes: attempting an orderly Zarf uninstall over the wire is neither necessary nor desirable, and the controller discards its state without blocking CAPI or Machine deletion. But Cluster-object deletion is **not always** an infrastructure wipe - `clusterctl move` and de-import/re-import delete the object while the cluster lives on - which is why the ApplyOnce guarantee is anchored on the workload cluster itself rather than on anything garbage-collected in this cascade (§3).

---

## 8. Uninstall Semantics & Deletion Policy

§7 covers the cluster going away; this section covers the *declaration* going away while the cluster lives on: a `ZarfPackageProxy` is deleted, or a cluster's labels change so the selector no longer matches it.

### A. `spec.deletionPolicy: Delete | Retain`

The proxy declares what happens to its deployed packages when their release is deleted:

- **`Delete` (the default, matching CAAPH)**: the package is uninstalled from the workload cluster - via `packager.Remove`, run in a removal Job on the same plumbing as deploys (§6's deploy Job: same image, kubeconfig mount, cache volume, and `deployJob` knobs) - before the release object is allowed to go away.
- **`Retain`**: the release object is deleted immediately and the deployed package is orphaned on the workload cluster, left running and operator-owned from then on. The honest choice for large or foundational packages, where an in-place uninstall is unreliable and an orphaned-but-working package beats a half-removed one.
- **`ApplyOnce` proxies ignore the policy and always behave as `Retain`**: a seed cannot be un-run, and its durable record lives on the workload cluster (§3) regardless of what happens to management-cluster objects.
- **A Reconcile proxy managing a package an ApplyOnce proxy seeded must also be `Retain`**: `Delete` on the successor removes the shared package name's deployed-package record along with the package, silently voiding the seed guarantee while the seed's release still reports `Completed` - see §3's seed-then-manage handoff.

### B. Mechanics: a finalizer only where a removal is owed

- Releases under `Delete` carry a CAAPZ finalizer; the controller runs the removal Job and drops the finalizer on success. Releases under `Retain` (and all ApplyOnce releases) carry **no finalizer at all**, so their deletion can never block on anything.
- The finalizer's presence tracks the proxy's *current* policy: flipping a proxy between `Delete` and `Retain` adds or strips the finalizer on its releases on the next reconcile, so the policy is live-editable and acts as the declarative break-glass for a stuck removal.
- **Proxy deletion**: a finalizer on the proxy deletes its releases (found via `spec.packageProxyRef`) and waits for them to finish their own finalization before the proxy goes away. Releases are deliberately *not* owner-referenced to the proxy: Kubernetes garbage collection only deletes a dependent once **all** of its owners are gone, so a second ownerRef would stop Cluster deletion from collecting releases and quietly break §7's cascade. The Cluster stays the sole owner.
- **Selector un-match**: the controller deletes the no-longer-matching release, and the same finalizer path decides whether that is an uninstall (`Delete`) or an orphan (`Retain`).

### C. The §7 override: cluster teardown always wins

If the release's owning `Cluster` has a deletion timestamp, no longer exists, or is unreachable, the finalizer is dropped **without** attempting removal. Uninstall semantics never block or delay cluster deletion; §7 outranks this section wherever they conflict.

When removal fails while the Cluster is alive and reachable, the controller requeues with backoff and surfaces a `RemovalFailed` condition on the release. The operator's escape hatches, in preference order: flip the proxy to `Retain` (declarative, affects all its releases), or remove the finalizer by hand (surgical, one release).

### D. Known limitation: shrinking `spec.components`

Removing an entry from `spec.components` does not remove that component from the workload cluster - a redeploy with fewer components leaves the extras running. Removed components are orphaned until the package is removed and redeployed. Per-component removal choreography is deliberately out of scope until a real need arrives.

---

## 9. Clusterctl Provider Contract

CAAPZ ships as a clusterctl-manageable provider, the same way the sister cargoship repository does:

- **`metadata.yaml`** at the repository root maps each release series to its Cluster API contract (`v1beta2`). Release-please owns the current series entry via `x-release-please-major`/`-minor` markers; past series are appended as releases are cut so `clusterctl upgrade` can map an installed version. Without this file, `clusterctl init` refuses to install the provider.
- **Provider labels** - `clusterctl.cluster.x-k8s.io: ""` and `cluster.x-k8s.io/provider: addon-zarf` - go on every manifest in the release bundle, so clusterctl can find and manage what it installed. They land with the manager deployment manifests.
- **`clusterctl move`**: `ZarfPackageProxy` and `ZarfPackageRelease` carry the `clusterctl.cluster.x-k8s.io/move` label so a pivot carries the declarations to the new management cluster. Releases are re-derivable (the controller re-stamps them from proxies and Clusters), but moving them preserves status history and avoids a re-deploy window; proxies are *not* re-derivable and must move. The ApplyOnce guarantee survives either way - it lives on the workload cluster (§3), not in anything being moved. The §2C caveat stands: a cross-namespace secret is outside what move carries.

---

## 10. Enterprise & Air-Gap Advantages

- **Air-gap Compliant**: Zarf packages bundle container images, Helm charts, and manifests together in an OCI artifact, eliminating out-of-band image syncing.
- **Supply Chain Security**: Cosign signature verification occurs on the management plane before any package payload touches the remote workload cluster.
- **Native CAPI Integration**: Works across all CAPI infrastructure providers (vSphere, AWS, Azure, bare-metal) and bootstrap providers (Cargoship, RKE2, Kubeadm).
