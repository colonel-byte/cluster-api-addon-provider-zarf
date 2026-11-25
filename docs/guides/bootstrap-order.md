# Bootstrap order

The order a first install has to follow, and why. If you deploy one package to one already-initialized cluster, you do not need this page. If you are standing up clusters from nothing - which is the case this provider exists for - the ordering is the part that fails, and it fails by waiting rather than by erroring.

This page is the declarative counterpart to [`zarf-bundle-order.yml`](https://github.com/colonel-byte/ansible-collection-zarf/blob/main/ansible/examples/playbooks/zarf-bundle-order.yml) in the Ansible collection, which performs the same ordering imperatively against a single cluster. The constraint is identical; what differs is that you describe the end state and the controller sequences it, across every matching cluster, instead of you sequencing it once per cluster.

## The constraint

Zarf's own internal registry wants persistent storage. Your storage provider is a package, and a package's images are pulled from a registry. On a cluster with neither, that is a cycle:

```
zarf init ──needs──> a StorageClass ──provided by──> a storage package ──needs──> a registry ──provided by──> zarf init
```

Nothing reports this as an error. `zarf init` stands up its injector and a temporary seed registry, then the permanent registry's `PersistentVolumeClaim` goes `Pending` and stays there, because nothing in the cluster provisions volumes yet. Meanwhile every other package you declared sits in `Pending` with reason `WaitingForZarfState`, because the init-first gate holds non-init packages until the cluster has Zarf state. The cluster looks busy and is stuck.

You break the cycle in one of four ways. They are listed cheapest-first, and the first one is the right answer far more often than it looks.

## Option 1: provide storage outside the package system

If a `StorageClass` already exists when `zarf init` runs, there is no cycle and no ordering problem. Nothing to sequence, nothing to stage, one proxy.

This is the right answer whenever the storage is something your cluster images or your infrastructure provider can carry: `local-path` on a single-node or lab cluster, a CSI driver your node image already ships, a cloud provider's default class, or a Cluster API `ClusterResourceSet` that applies the provisioner as the cluster comes up.

`local-path` as a `ClusterResourceSet`, applied by Cluster API itself at cluster creation:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-path-storage
  namespace: default
data:
  # The provisioner's own manifest. Stage it from
  # https://github.com/rancher/local-path-provisioner/blob/v0.0.37/deploy/local-path-storage.yaml
  local-path-storage.yaml: |
    # ... contents of local-path-storage.yaml ...
---
apiVersion: addons.cluster.x-k8s.io/v1beta1
kind: ClusterResourceSet
metadata:
  name: local-path-storage
  namespace: default
spec:
  clusterSelector:
    matchLabels:
      storage: local-path
  resources:
    - name: local-path-storage
      kind: ConfigMap
  strategy: ApplyOnce
```

Then the whole Zarf side is one proxy, because by the time it runs there is storage:

```yaml
apiVersion: addons.colonel-byte.dev/v1alpha1
kind: ZarfPackageProxy
metadata:
  name: baseline
  namespace: default
spec:
  clusterSelector:
    matchLabels:
      zarf: enabled
  package:
    repository: registry.internal/packages/baseline
    version: "1.0.0"
    api: v1beta1
  init:
    autoInit: true
    package:
      repository: registry.internal/packages/init-slim
      version: "v1.0.1"
      api: v1beta1
    components:
      - name: zarf-injector
      - name: zarf-seed-registry
      - name: zarf-registry
      - name: zarf-agent
```

Two details in that `init` block that are easy to get wrong:

- **`init.components` is not optional in practice.** An init package built to bring a cluster up in stages marks every component optional, so the default selection - required components only - is empty, and the init deploys nothing while reporting success. Name the components you want.
- **`autoInit` is keyed on observed cluster state, never on a name.** The controller probes for Zarf state and only initializes a cluster that has none, so this commutes with a fleet that manages init through its own proxy. Whoever gets there first wins, and the other no-ops.

**Two things `local-path` will want on a real cluster**, both learned by running it rather than reading about it. Its volume-provisioning helper pod mounts a `hostPath`, which the `baseline` Pod Security standard forbids - so its namespace needs `pod-security.kubernetes.io/enforce: privileged`. And a single-node cluster whose only node carries a control-plane `NoSchedule` taint has nowhere to put the registry: Zarf's injector treats any `NoSchedule` or `NoExecute` taint as disqualifying and reports `no suitable injector image or node exists`.

## Option 2: put the storage inside a custom init package

Zarf's component `import` mechanism composes a `ZarfInitConfig` that carries your storage provider's components alongside the registry's. One package, one proxy, no ordering - the cycle is resolved inside the package instead of between packages.

This is the documented escape hatch for exactly this constraint, and the cost is that each fleet segment needing different storage carries its own init package. Worth it when storage genuinely cannot exist before init; not worth it otherwise, because you have taken on building and maintaining an init package.

## Option 3: let the whole thing wait on a seeded registry

If your storage provider can run from an ephemeral registry, you can order the pieces with `dependsOn` and let the controller sequence them. The seed registry that `zarf init` stands up before the permanent one is enough to pull from:

```yaml
apiVersion: addons.colonel-byte.dev/v1alpha1
kind: ZarfPackageProxy
metadata:
  name: zarf-init
  namespace: default
spec:
  clusterSelector:
    matchLabels:
      zarf: enabled
  strategy: Reconcile
  deletionPolicy: Retain
  package:
    repository: registry.internal/packages/init-slim
    version: "v1.0.1"
    api: v1beta1
  components:
    - name: zarf-injector
    - name: zarf-seed-registry
    - name: zarf-registry
    - name: zarf-agent
---
apiVersion: addons.colonel-byte.dev/v1alpha1
kind: ZarfPackageProxy
metadata:
  name: storage
  namespace: default
spec:
  clusterSelector:
    matchLabels:
      zarf: enabled
  dependsOn:
    - zarf-init
  package:
    repository: registry.internal/packages/local-path
    version: "0.0.37"
    api: v1beta1
```

The init package is declared as a package in its own right here, not through `init.autoInit`. It resolves as `ZarfInitConfig`, which makes it init-capable, so the gate lets it take the cluster's deploy slot while everything else waits. `dependsOn` then holds `storage` until that release reports deployed, and `Retain` keeps a later deletion of the proxy from uninstalling the thing the cluster is built on.

Whether this works depends on your storage provider tolerating a registry that is about to be replaced. If it does not, you are in option 1 or 2.

## Option 4: stage it in two passes, like the Ansible playbook

The Ansible flow deploys the init package **twice**: once with seed components only, then the storage package, then init again with the full component set - because the permanent registry cannot come up until storage exists, and storage cannot be pulled until the seed registry does.

You can do the same here, with one proxy and two commits. Declare it with the seed components:

```yaml
spec:
  components:
    - name: zarf-injector
    - name: zarf-seed-registry
```

let the storage proxy land, then edit the same proxy to the full set:

```yaml
spec:
  components:
    - name: zarf-injector
    - name: zarf-seed-registry
    - name: zarf-registry
    - name: zarf-agent
```

Editing the component list re-renders the deploy and the package redeploys with the wider selection, which is Zarf doing an in-place upgrade of a package it already has a record for.

**What you cannot do today is split those two passes across two proxies.** Both would manage the same package name, so both would own the same deployed-package record on the cluster, and this provider has no `takeOwnership` knob to hand one to the other. So the staging is a sequence of edits - a GitOps commit each - rather than something the DAG performs for you. If your bootstrap genuinely needs two passes without a human between them, option 2 is the one that gets you there.

## Day two

Once a cluster has Zarf state, none of the above applies to it again. The gate is open, `autoInit` no-ops because it probes and finds state, and packages deploy in whatever order `dependsOn` describes.

The Ansible playbook distinguishes bootstrap from convergence by inspecting what is installed and branching. Here that distinction does not exist, because the controller re-derives it on every pass: a release that is already on the declared digest is left alone, a release whose proxy moved to a new version redeploys, and `ApplyOnce` packages adopt zarf's existing record rather than re-running. You declare the end state once and the same declaration is correct on day one and day two.

What still needs a person:

- **A new version** - bump `spec.package.version`. Nothing moves on its own, including a re-pushed tag, which raises `TagDrift` and waits.
- **A failed deploy** - the controller does not yet retry on a timer. Editing the spec is what re-runs it.
- **A failed init** - the release holds with `InitFailed` and the failing Job is kept as evidence. Nothing deploys onto a half-initialized cluster, which is deliberate: a cluster with Zarf state and no registry is indistinguishable from a properly initialized one from the outside, so the provider refuses to guess.

## Migrating a management cluster

`clusterctl move` carries a `Cluster` to a new management cluster. It does not carry your `ZarfPackageProxy` objects, and that is not a defect: a proxy is owned by nothing on purpose - it selects clusters by label and spans a fleet - so it is in no cluster's object graph and clusterctl has no reason to associate it with the cluster being moved. The `ZarfPackageRelease` objects do not survive either; they are owner-referenced to the `Cluster`, so Kubernetes garbage-collects them when the move deletes the source `Cluster` at the end.

What is left untouched is the part that matters. **Zarf's record of what is deployed lives on the workload cluster, not on the management cluster**, so a migration loses bookkeeping, not state. Verified by moving one: the deployed content and `zarf-package-<name>` secrets are exactly as they were.

So the runbook is:

1. `clusterctl move --to-kubeconfig=...` as normal. The clusters arrive.
2. **Re-apply your proxies to the new management cluster.** They are declarations and should already be in Git, which is the whole reason this step is cheap.
3. The controller re-stamps a release per matching cluster and converges.

Step 3 behaves differently by strategy, and it is worth knowing which you have:

| `spec.strategy` | What happens after the proxies are re-applied                                                                                               |
| --------------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `ApplyOnce`     | The rebuilt release probes the cluster, finds Zarf's existing record, and completes with reason `AdoptedExistingRecord`. Nothing re-runs    |
| `Reconcile`     | The rebuilt release has no deploy history to compare against, so it deploys the declared digest again - a redeploy per cluster, not a no-op |

That `Reconcile` row is the one to plan around. The redeploy is of the same digest, so it converges on the state the cluster is already in, but on a large fleet it is a burst of deploy Jobs and registry pulls immediately after a migration. One deploy Job runs per cluster at a time, so it is paced rather than simultaneous - but pace it further if that matters: move in batches, or re-apply the proxies in batches rather than all at once.

## Checking where a cluster is

```sh
kubectl get zarfpackagerelease -o custom-columns='NAME:.metadata.name,CLUSTER:.spec.clusterRef.name,PHASE:.status.phase,READY:.status.conditions[?(@.type=="Ready")].reason'
```

A fleet mid-bootstrap reads as a column of `Pending`/`WaitingForZarfState` with one `Deploying` - the init package holding its cluster's slot. That is the system working; one deploy or removal Job runs per cluster at a time, so the ordering you declared is an invariant rather than a race.

To see whether a cluster is initialized at all, ask the cluster:

```sh
kubectl --kubeconfig ./workload.kubeconfig get secret -n zarf zarf-state
```

And what Zarf believes is deployed there:

```sh
kubectl --kubeconfig ./workload.kubeconfig get secret -n zarf -o name | grep zarf-package-
```

Those secrets, not the objects on the management cluster, are what `ApplyOnce` adoption and uninstall both key on. Deleting a release does not delete them; that is why a recreated release adopts instead of re-running a seed.
