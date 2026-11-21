# Architecture Proposal: Cluster API Add-on Provider for Zarf (CAAPZ)

Inspired by the design of [cluster-api-addon-provider-helm (CAAPH)](https://github.com/kubernetes-sigs/cluster-api-addon-provider-helm), this document proposes a declarative Cluster API Add-on Provider for [Zarf](https://github.com/zarf-dev/zarf) packages: `cluster-api-addon-provider-zarf` (CAAPZ).

The goal of CAAPZ is to automate the deployment, day-2 lifecycle, and one-time initialization ("seed") of self-contained Zarf packages onto CAPI workload clusters across air-gapped enterprise environments.

---

## 1. High-Level Architecture

CAAPZ operates in the management cluster as an add-on controller. It watches CAPI `Cluster` resources, retrieves target cluster credentials via CAPI's remote cluster cache, pulls packages from an internal or OCI registry, and drives Zarf deployment directly via Zarf's Go SDK against the workload cluster API.

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
│     │ 2. Pulls package from internal Zarf OCI mirror             │     │
│     │ 3. Verifies Cosign signature before extraction             │     │
│     │ 4. Obtains target cluster REST config                      │     │
│     │ 5. Executes Zarf Deploy via Go API / SDK                   │     │
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
    storageClass: "ceph-bucket"

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
   - Continuously watches the package in the OCI registry or changes in `spec.package.version`.
   - On version bumps or variable edits, redeploys the package to converge the workload cluster to the new state.
   - Detects and repairs cluster drift.

2. **`strategy: ApplyOnce` (Seed / Initialization Package)**:
   - When a new cluster matching the selector is discovered or imported, the controller pulls and applies the Zarf package.
   - Once the deployment succeeds, the `ZarfPackageRelease` is marked `phase: Completed` and `applied: true`.
   - The controller will **never re-execute** this package against that cluster, even if the proxy's spec or variables change.
   - Ideal for Day-0/Day-1 cluster bootstrap hooks and registration payloads.

#### Where the "never re-execute" record actually lives

The `ZarfPackageRelease` is a cache of the guarantee, not the guarantee itself: it is deliberately garbage-collected with its owning `Cluster` (§7), and a `Cluster` object is deleted *without the infrastructure being wiped* during `clusterctl move` and de-import/re-import - exactly the imported-cluster flows seed packages target. The durable record is the one Zarf itself writes on the workload cluster: every deploy stores a `zarf-package-<name>` secret (labeled `package-deploy-info`) in the `zarf` namespace, which survives management-cluster churn because it lives on the physical cluster being guarded.

So the seed check is two-tier:

- `status.applied: true` on an existing release short-circuits the reconcile with no workload-cluster round trip.
- When the release is missing or not yet applied - the only states in which a deploy would fire - the controller first probes the workload cluster for the package's deployed-package secret. If present, the release is marked `Completed`/`applied: true` **without re-executing** (adoption); only a cluster with no record is seeded.

Consequences, all deliberate:

- Re-importing a live cluster re-creates its releases but does not re-run its seed packages.
- Hand-deleting a `ZarfPackageRelease` does not re-run the seed; the recreated release adopts the existing record.
- The escape hatch for intentionally re-seeding is removing the package's record on the workload cluster (`zarf package remove`, or deleting the `zarf-package-<name>` secret) - an explicit operator action against the cluster being re-seeded.
- Wiping and rebuilding infrastructure behind the same `Cluster` object re-seeds, because the rebuilt cluster carries no record. That is the desired reading of "once per physical cluster".

One restriction follows: Zarf only stores the deployed-package record when the package has at least one cluster-touching component, so **an ApplyOnce package must include one**. A pure-actions package would leave no durable record, silently degrading the guarantee to the garbage-collectable release; the controller surfaces a condition on the release when a completed seed deploy left no record behind.

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
For each workload cluster:
1. When reconciling `logging-stack` for cluster `example-cluster`, the controller inspects the status of the prerequisite `ZarfPackageRelease` objects for `example-cluster`:
   - `example-zarf-init`
   - `example-csi-ceph`
2. **Readiness Gate**:
   - The dependent package is held in `phase: Pending` with condition:
     ```yaml
     type: DependenciesSatisfied
     status: "False"
     reason: WaitingForDependency
     message: "dependency 'csi-ceph' is not yet in phase 'Deployed'"
     ```
   - The controller requeues until all dependencies report `status.phase: Deployed` (or `Completed` for seed packages).
3. **Circular Dependency Detection**:
   Directed Acyclic Graph (DAG) validation runs during admission and reconcile to reject dependency loops before any deploy begins.

---

## 6. Reconcile Workflow

1. **Cluster Discovery**:
   The controller watches for CAPI `Cluster` resources reaching `status.initialization.controlPlaneInitialized == true` (the v1beta2 initialization signal, surfaced by the `ControlPlaneInitialized` condition) and matching `spec.clusterSelector`.
2. **Kubeconfig Resolution**:
   CAAPZ retrieves the cluster's administrative kubeconfig secret (`<cluster-name>-kubeconfig`) using CAPI's cluster client cache.
3. **Seed Check**:
   If `spec.strategy == ApplyOnce`, skip execution when the release is already `applied: true`. Otherwise probe the workload cluster for the package's `zarf-package-<name>` deployed-package secret in the `zarf` namespace; if present, mark the release `Completed` without deploying (see §3, "Where the 'never re-execute' record actually lives").
4. **OCI Pull & Signature Verification**:
   The package is fetched from the OCI registry to local cache storage on the controller pod. If `signatureKeySecretRef` is set, verify the Cosign signature against the public key before extracting layers.
5. **Auto-Init Check**:
   If `spec.init.autoInit == true`, probe the target cluster for Zarf state (`zarf-state` secret in `zarf` namespace). If absent, deploy the Zarf init package first.
6. **Deploy Execution**:
   Render variables (`{{ .Cluster.metadata.name }}`), assemble `types.ZarfDeployOptions`, and invoke the Zarf packager engine using the remote target cluster's REST configuration.
7. **Status Update**:
   Update `ZarfPackageRelease.status` and surface condition `Ready: True` on the release and proxy objects.

---

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

## 9. Enterprise & Air-Gap Advantages

- **Air-gap Compliant**: Zarf packages bundle container images, Helm charts, and manifests together in an OCI artifact, eliminating out-of-band image syncing.
- **Supply Chain Security**: Cosign signature verification occurs on the management plane before any package payload touches the remote workload cluster.
- **Native CAPI Integration**: Works across all CAPI infrastructure providers (vSphere, AWS, Azure, bare-metal) and bootstrap providers (Cargoship, RKE2, Kubeadm).
