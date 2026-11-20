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
apiVersion: addons.cluster.x-k8s.io/v1alpha1
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

  # Variable templating supporting CAPI cluster metadata
  set:
    CLUSTER_NAME: "{{ .Cluster.metadata.name }}"
    DOMAIN: "{{ .Cluster.metadata.name }}.example.com"
```

### B. Per-Cluster Release: `ZarfPackageRelease`
Created by the controller in the cluster's namespace and owned by the target `Cluster`:

```yaml
apiVersion: addons.cluster.x-k8s.io/v1alpha1
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

---

## 4. Native `zarf.dev/v1beta1` Package Support

CAAPZ natively implements the modern `zarf.dev/v1beta1` Zarf package schema while maintaining backward compatibility with `zarf.dev/v1alpha1`:

### A. Automatic Ingestion & Migration
Using Zarf's Go SDK (`github.com/zarf-dev/zarf/src/api/convert`):
- When pulling a package from an OCI registry, CAAPZ inspects the `apiVersion`.
- Legacy `zarf.dev/v1alpha1` packages are migrated in-memory to `v1beta1.Package` structures without modifying the upstream package artifact.
- Native `zarf.dev/v1beta1` packages are ingested directly.

### B. Hierarchical Structured Values (`spec.values`)
In `v1beta1`, Zarf supports structured values maps (analogous to Helm `values.yaml`). CAAPZ exposes `spec.values` directly on `ZarfPackageProxy`:
- Values accept full YAML trees rather than only string scalars (`spec.set`).
- Supports Go template substitution referencing CAPI `Cluster` metadata and annotations (e.g. `{{ .Cluster.metadata.name }}`).
- Merged with the package's internal defaults at deployment time.

### C. Architecture & Flavor Auto-Detection
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
apiVersion: addons.cluster.x-k8s.io/v1alpha1
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
   The controller watches for CAPI `Cluster` resources reaching `status.controlPlaneReady == true` and matching `spec.clusterSelector`.
2. **Kubeconfig Resolution**:
   CAAPZ retrieves the cluster's administrative kubeconfig secret (`<cluster-name>-kubeconfig`) using CAPI's cluster client cache.
3. **Seed Check**:
   If `spec.strategy == ApplyOnce`, verify if a completed `ZarfPackageRelease` already exists. If yes, skip execution.
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
- CAAPZ treats cluster deletion as an infrastructure-level wipe: since destroying the cluster destroys all underlying compute and storage (disks, nodes, namespaces), attempting an orderly Zarf uninstall over the wire is neither necessary nor desirable. The controller discards state without blocking CAPI or Machine deletion.

---

## 8. Enterprise & Air-Gap Advantages

- **Air-gap Compliant**: Zarf packages bundle container images, Helm charts, and manifests together in an OCI artifact, eliminating out-of-band image syncing.
- **Supply Chain Security**: Cosign signature verification occurs on the management plane before any package payload touches the remote workload cluster.
- **Native CAPI Integration**: Works across all CAPI infrastructure providers (vSphere, AWS, Azure, bare-metal) and bootstrap providers (Cargoship, RKE2, Kubeadm).
