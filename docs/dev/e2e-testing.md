# E2E testing: the kind + CAPD topology

The end-to-end suite runs the things [`test:envtest`](../../magefiles/test.go) structurally cannot: real CAPI lifecycle machinery (a control-plane provider flipping `ControlPlaneInitialized`, the `<cluster-name>-kubeconfig` secrets CAPI writes, garbage-collection cascades), pods actually scheduling, and an OCI registry reachable from every side of the topology. It stands up a kind management cluster with CAPI core + CAPD initialized into it, one CAPD workload cluster with a CNI, and a `ghcr.io/distribution/distribution:3` registry on the kind docker network, then runs the suite in the separate `test/e2e` module against all of it.

### The workload cluster needs a CNI, and CAPD ships none

Worth knowing because it is invisible until something tries to run: a cluster CAPD creates has no CNI at all. Its node stays `NotReady` reporting `cni plugin not initialized`, coredns sits `Pending` forever, and nothing can be scheduled - while `ControlPlaneInitialized` is true, the kubeconfig secret exists, and the API server answers perfectly well. Phase 0's spike assertions needed none of that, so the gap only surfaced when a deploy had somewhere to land.

`e2e:up` therefore applies Calico and waits for the node to go `Ready`. Calico because the Cluster API quick start pairs it with CAPD: its default pod CIDR is the `192.168.0.0/16` the development flavor hands out, so the manifest applies with nothing to reconfigure. The same step writes the workload cluster's host-reachable kubeconfig to `dist/e2e/workload.kubeconfig` - the localhost/port-map variant `clusterctl get kubeconfig` rewrites, not the pod-reachable one in the secret - and `e2e:run` passes it to the suite as `E2E_WORKLOAD_KUBECONFIG` so assertions can read what a deploy actually left on the target cluster.

A second thing that cluster does not have is a `StorageClass`, so anything wanting a PVC there has to say otherwise - for the Zarf init package, `init.set`'s `REGISTRY_PVC_ENABLED: "false"`.

```sh
go run ./magefiles/core e2e:up     # kind + clusterctl init + CAPD workload cluster + registry
go run ./magefiles/core e2e:run    # the suite, against the running topology
go run ./magefiles/core e2e:down   # removes everything, including the CAPD machine containers
```

[`e2e-walkthrough.md`](e2e-walkthrough.md) is the companion page: what each of those targets does step by step, what it leaves in `dist/e2e/`, and how to drive the suite from a terminal - running a single test, reading a failure, and starting genuinely clean.

`e2e:up` is idempotent and resumable - a failed run is re-runnable and picks up where it stopped. The suite does not tear the topology down; iterate on tests against a long-lived topology and run `e2e:down` when finished.

**The forbidden invocation**: a bare `go test` inside `test/e2e/`. The suite talks to a live topology through `E2E_KUBECONFIG`, which only `e2e:run` sets - without it every test skips, and a fully-skipped suite reads as green while asserting nothing. `test/e2e` is its own Go module precisely so the root `go test ./...` can never reach it; it is also deliberately unvendored, see the exception note in [`magefiles/AGENTS.md`](../../magefiles/AGENTS.md).

## A test may only destroy what it created

One assertion in the suite has to delete a workload cluster - §8C, that destroying a cluster is never blocked by a package deployed to it - and the shared topology is sitting right next to it. The mistakes available here are all the same shape: a test aimed at one object reaches another, nothing complains, and the damage is done before any assertion reports it. This one has already happened once, by building the throwaway cluster from a deep copy of the standing cluster's whole `Spec`: a `Cluster` spec carries `controlPlaneRef` and `infrastructureRef`, the topology controller fills them in with the objects it created, and deleting a `Cluster` deletes whatever those refs name - so the copy pointed the throwaway cluster at the standing cluster's control plane, and its cleanup would have destroyed it.

So the rule, enforced rather than remembered:

- **Infrastructure the suite creates carries `e2e.addons.colonel-byte.dev/owned-by: caapz-e2e-suite`,** and `requireSuiteOwned` refuses to delete anything without it. The standing topology belongs to `e2e:up` and carries no such label, which makes it undeletable from a test.
- **Build a throwaway cluster from the standing one's `topology` and `clusterNetwork` only**, never its whole spec. Carrying the topology over is what avoids hand-maintaining a CAPD topology and the ClusterClass variables it requires; carrying the refs over is what breaks it.
- **A delete of a `Cluster` checks its refs first**, and bails if either names something outside that cluster.

The same reasoning applies to anything else expensive the suite might reach for - the registry container, the management cluster, the initialized Zarf state on the standing cluster. If a test needs to destroy one, it creates its own first.

## Host prerequisites

`docker`, `kind`, and `kubectl` on `PATH`, network access (kind node images, the clusterctl binary, provider manifests), and one pair of kernel settings that is easy to miss:

### Raise the inotify limits before the first `e2e:up`

kind's own [known-issues page](https://kind.sigs.k8s.io/docs/user/known-issues/#pod-errors-due-to-too-many-open-files) documents this, and this repository has hit it: on a stock Linux host, `fs.inotify.max_user_instances` (often 128) is exhausted by a kind cluster running beside anything else that watches files - another local cluster, an editor, a file sync. The failure does not name inotify: pods on the fresh management cluster crashloop, and `kubectl logs` on kube-proxy (or cert-manager, or local-path-provisioner) shows

```
"command failed" err="failed complete: too many open files"
```

The fix, once per boot:

```sh
sudo sysctl -w fs.inotify.max_user_watches=524288 fs.inotify.max_user_instances=512
```

To make it survive reboots, persist it as a sysctl drop-in:

```sh
printf 'fs.inotify.max_user_watches = 524288\nfs.inotify.max_user_instances = 512\n' | sudo tee /etc/sysctl.d/99-kind-inotify.conf
sudo sysctl --system
```

The crashlooped pods recover on their own next restart once the limits are raised - no need to recreate the cluster.

## What the suite covers today, and does not

Phases 0 and 1 of the plan. Phase 0 is the two spike assertions the deploy-Job model rests on in this topology - the server address written in the CAPD-generated kubeconfig secret is reachable from a management-cluster pod, and one registry name (`caapz-e2e-registry:5000`) resolves and pulls from management pods and workload nodes alike. Phase 1 is the controller running in the cluster: `e2e:run` builds the manager with the release Dockerfile, kind-loads it, and installs it through a workdir kustomize overlay over `config/default` (adding `--registry-plain-http` for the HTTP test registry), then asserts pure matching behavior on real CAPI objects - a release stamped Cluster-owned on selector match and deleted on un-match, a foreign-namespace proxy reconciling to zero matches without `--cluster-namespaces`, and the three `dependsOn` hold verdicts as live conditions. It does **not** yet deploy any Zarf package or exercise uninstall - those arrive with their phases (fixtures and the Cosign keypair first).

Every version the topology pins - clusterctl/CAPD, the workload Kubernetes version, the node/registry/test image digests - lives as annotated constants in `magefiles/pkg/e2e/e2e.go`, each with its Renovate annotation or written bump procedure per the pin-ownership rule in [`magefiles/AGENTS.md`](../../magefiles/AGENTS.md#every-pin-needs-an-owner).
