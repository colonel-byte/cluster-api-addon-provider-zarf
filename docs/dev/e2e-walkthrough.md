# E2E walkthrough: what the targets do, and how to drive them by hand

[`e2e-testing.md`](e2e-testing.md) is the overview - what the suite is for, the topology, the host prerequisites, and the rule about what a test may destroy. This page is the other half: exactly what each target does step by step, what it leaves on disk, and how to work with it at a terminal rather than watching a CI log.

Read this before changing `magefiles/pkg/e2e/`, and before trusting a failure you did not cause.

## The three targets

```sh
go run ./magefiles/core e2e:up     # build the topology (slow, idempotent, resumable)
go run ./magefiles/core e2e:run    # build and install the controller, then run the suite
go run ./magefiles/core e2e:down   # remove everything e2e:up built
```

Plus the pivot, which is separate because it roughly doubles the topology:

```sh
go run ./magefiles/core e2e:move      # second management cluster, then migrate onto it
go run ./magefiles/core e2e:moveDown  # remove only that second cluster
```

`e2e:up` and `e2e:run` are deliberately separate. Building the topology is minutes; running the suite is a couple of minutes. You build once and run the suite as many times as it takes.

## What `e2e:up` does

Thirteen steps, each printed as `==> <name>` so a failure names its own step. Every one is idempotent, so re-running after a failure continues rather than starting over.

| #  | Step                                  | What it does                                                                                                                                                                                                                                                    |
| -- | ------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1  | `clusterctl binary`                   | Downloads the pinned `clusterctl` to `dist/e2e/clusterctl` if absent                                                                                                                                                                                            |
| 2  | `registry container`                  | Starts `distribution:3` as `caapz-e2e-registry`, published on `127.0.0.1:5001`                                                                                                                                                                                  |
| 3  | `kind management cluster`             | Creates kind cluster `caapz-e2e` with the docker socket mounted (CAPD needs it to create sibling containers) and a containerd `config_path`                                                                                                                     |
| 4  | `registry wiring on management nodes` | Writes a containerd `hosts.toml` so the node resolves `caapz-e2e-registry:5000` over plain HTTP                                                                                                                                                                 |
| 5  | `CAPI core + CAPD`                    | `clusterctl init --infrastructure docker` with `CLUSTER_TOPOLOGY=true`                                                                                                                                                                                          |
| 6  | `provider deployments available`      | Waits for every provider deployment. Sized for a **cold** image pull - see the constant's comment                                                                                                                                                               |
| 7  | `workload cluster`                    | Generates a `--flavor development` cluster manifest, caches it keyed on the Kubernetes version, applies it with retries, waits for `ControlPlaneInitialized` and the kubeconfig secret                                                                          |
| 8  | `registry wiring on workload nodes`   | Same `hosts.toml`, plus a containerd `config_path` patch and restart, because CAPD machines never saw kind's config patches                                                                                                                                     |
| 9  | `CNI on the workload cluster`         | Writes `dist/e2e/workload.kubeconfig`, applies Calico, waits for node `Ready`, labels the node for the registry's affinity, removes the control-plane taint, installs local-path as the default StorageClass, and marks its namespace Pod-Security `privileged` |
| 10 | `spike image in registry`             | Stages the busybox the Phase 0 assertions use                                                                                                                                                                                                                   |
| 11 | `fixture signing keypairs`            | Generates two Cosign keypairs into `dist/e2e/keys` - the signing pair, and an unrelated one for the bad-signature case                                                                                                                                          |
| 12 | `fixture packages in registry`        | Builds and pushes three signed fixture packages through the vendored Zarf SDK                                                                                                                                                                                   |
| 13 | `init package mirror`                 | Copies the v1beta1 init package into the local registry, registry to registry. The one large transfer; skipped when already present                                                                                                                             |

Step 9 is where four CAPD facts live, each of which is invisible until something needs it. A CAPD cluster has no CNI, no StorageClass, a control-plane taint that disqualifies every node from Zarf's injector, and a `baseline` Pod Security standard that forbids the storage provisioner's `hostPath` helper pod. All four are documented at the code.

## What `e2e:run` does

| # | Step                         | What it does                                                                                                      |
| - | ---------------------------- | ----------------------------------------------------------------------------------------------------------------- |
| 1 | `controller image`           | Builds the manager with the release `Dockerfile` against a goreleaser-shaped context, so it is the release recipe |
| 2 | `controller image into kind` | `kind load docker-image` into the management cluster                                                              |
| 3 | `controller manifests`       | Renders a workdir kustomize overlay over `config/default`, applies it, restarts the rollout, waits for it         |

Then it runs `go test` in `test/e2e` with the environment the suite reads:

| Variable                  | Points at                                                                      |
| ------------------------- | ------------------------------------------------------------------------------ |
| `E2E_KUBECONFIG`          | The management cluster                                                         |
| `E2E_WORKLOAD_KUBECONFIG` | The workload cluster, host-reachable variant                                   |
| `E2E_KEY_DIR`             | The fixture keypairs, so a test can name a public key a proxy verifies against |

`e2e:move` exports two more: `E2E_TARGET_KUBECONFIG` (the second management cluster, which is also what makes the pivot test run rather than skip) and `E2E_CLUSTERCTL`.

The rollout restart in step 3 is not redundant. A reinstall with an unchanged manifest changes nothing, so the rollout wait would pass against the pod still running the *old* image - which is how a code change appears to have no effect.

## What `e2e:up` leaves on disk

Everything lives in `dist/e2e/`, which is gitignored build output. Deleting the directory is part of starting clean.

| Path                       | What it is                                                                                 |
| -------------------------- | ------------------------------------------------------------------------------------------ |
| `mgmt.kubeconfig`          | The management cluster. This is what `kubectl --kubeconfig` wants for proxies and releases |
| `workload.kubeconfig`      | The workload cluster, rewritten for host access by `clusterctl get kubeconfig`             |
| `target.kubeconfig`        | The pivot's second management cluster, only after `e2e:move`                               |
| `clusterctl`               | The pinned binary                                                                          |
| `kind.yaml`                | The management cluster's kind config                                                       |
| `workload-v<version>.yaml` | The generated workload cluster manifest, keyed on the Kubernetes version                   |
| `controller.yaml`          | The rendered controller manifest `e2e:run` applied                                         |
| `controller/`              | The kustomize overlay it was rendered from                                                 |
| `image/`                   | The goreleaser-shaped build context for the controller image                               |
| `keys/`                    | `cosign.key`/`cosign.pub` and `wrong.key`/`wrong.pub`                                      |
| `fixtures/`                | The generated fixture package definitions                                                  |
| `zarf-cache/`              | The SDK's cache from building fixtures                                                     |

## Driving it by hand

### First run

```sh
go run ./magefiles/core e2e:up
go run ./magefiles/core e2e:run
```

Expect roughly ten to fifteen minutes for `e2e:up` on a cold cache - most of it pulling the CAPI provider images, the kind node image, and the init package - and a little over three minutes for `e2e:run`. A warm `e2e:up` that only has to re-check things is well under two minutes.

If `e2e:up` fails, read the `==>` line above the error: that is the step, and every step is safe to re-run. Just run `e2e:up` again.

### Iterating on controller code

`e2e:run` rebuilds and reinstalls the controller every time, so the loop is just:

```sh
# edit internal/controller/...
go run ./magefiles/core e2e:run
```

No teardown, no reinstall by hand. The topology outlives the controller.

### Running one test

The suite is a normal Go test binary, so `-run` works - but it has to be given the environment, and a bare `go test` in `test/e2e` silently skips everything rather than failing. Export what the target would:

```sh
cd test/e2e
export E2E_KUBECONFIG="$PWD/../../dist/e2e/mgmt.kubeconfig"
export E2E_WORKLOAD_KUBECONFIG="$PWD/../../dist/e2e/workload.kubeconfig"
export E2E_KEY_DIR="$PWD/../../dist/e2e/keys"
go test -count=1 -v -run TestAutoInitAndDeploy ./...
```

This skips `e2e:run`'s controller rebuild, so it runs whatever is already installed. After changing controller code, run `e2e:run` once to reinstall, then iterate with `-run`.

`-count=1` matters: without it a passing result can come from the test cache rather than from the cluster.

### Watching it work

The suite logs progress while it waits, which `go test` only shows with `-v` or on failure. Watching the cluster directly is usually faster:

```sh
watch 'kubectl --kubeconfig dist/e2e/mgmt.kubeconfig get zarfpackageproxy,zarfpackagerelease,jobs'
```

```sh
watch 'kubectl --kubeconfig dist/e2e/workload.kubeconfig get pods -n zarf'
```

### When a test fails

Work outward from the release, because that is where the controller records what it decided.

```sh
kubectl --kubeconfig dist/e2e/mgmt.kubeconfig get zarfpackagerelease -o custom-columns='NAME:.metadata.name,PHASE:.status.phase,READY:.status.conditions[?(@.type=="Ready")].reason,MSG:.status.conditions[?(@.type=="Ready")].message'
```

| Where to look        | Command                                                                                              |
| -------------------- | ---------------------------------------------------------------------------------------------------- |
| Controller decisions | `kubectl --kubeconfig dist/e2e/mgmt.kubeconfig -n caapz-system logs deploy/caapz-controller-manager` |
| A deploy's own error | `kubectl --kubeconfig dist/e2e/mgmt.kubeconfig logs job/<release>-deploy`                            |
| An init's own error  | `kubectl --kubeconfig dist/e2e/mgmt.kubeconfig logs job/<release>-init`                              |
| What Zarf believes   | `kubectl --kubeconfig dist/e2e/workload.kubeconfig get secret -n zarf`                               |
| What actually landed | `kubectl --kubeconfig dist/e2e/workload.kubeconfig get configmap e2e-app -o yaml`                    |

A failed Job's pod is kept rather than cleaned up, so its logs and its termination message survive for exactly this.

### Failures that are the environment, not the code

Worth recognising, because each one has cost someone an hour:

| Symptom                                                                                  | Cause                                                                                                      |
| ---------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------- |
| Management-cluster pods crashloop with `too many open files`                             | inotify limits. See the host prerequisites in [`e2e-testing.md`](e2e-testing.md)                           |
| `timed out waiting for the condition` on a provider deployment, pods `ContainerCreating` | Still pulling images on a cold cache. Check `crictl images` on the node before assuming anything broke     |
| A workload machine never boots and nothing says why                                      | The Kubernetes version has no published `kindest/node` image. The pin tracks kind's images for this reason |
| `no suitable injector image or node exists`                                              | The node carries a `NoSchedule` taint. Step 9 removes it; a hand-made cluster will not have                |
| A registry pod `Pending` with an unbound claim                                           | No StorageClass, or the provisioner's helper pod is blocked by Pod Security                                |

### Starting genuinely clean

A clean run is the only thing that proves `e2e:up` encodes every step rather than depending on something applied by hand earlier. Three of the bugs in that table above were found exactly this way, and could not have been found any other way.

```sh
go run ./magefiles/core e2e:moveDown
go run ./magefiles/core e2e:down
rm -rf dist/e2e
go run ./magefiles/core e2e:up
go run ./magefiles/core e2e:run
```

Removing `dist/e2e` is the part that matters. `e2e:down` removes clusters and containers; the workdir is what holds cached manifests, generated fixtures, and keys, and a cached manifest is how a stale pin stays invisible.

Do this before trusting the harness after any change to `magefiles/pkg/e2e/`.

### The pivot

```sh
go run ./magefiles/core e2e:run    # the pivot carries what this deployed
go run ./magefiles/core e2e:move
go run ./magefiles/core e2e:moveDown
```

`e2e:move` reuses the controller image and rendered manifest `e2e:run` produced rather than rebuilding, so the two management clusters are running the same binary - which is the situation a real migration is in.

It leaves the topology pivoted onto the target: the workload cluster is managed by `caapz-e2e-target`, and the proxies are still on `caapz-e2e`. That is the behaviour under test, not a mess to clean up - but it does mean a subsequent `e2e:run` against the source has no cluster to match. Tear down and rebuild after a pivot.

## Things that will catch you out

- **A bare `go test` in `test/e2e` skips everything and exits 0.** It is a separate module precisely so the root `go test ./...` cannot reach it, and every test skips without `E2E_KUBECONFIG`. A fully-skipped suite reads as green; `e2e:run` is the invocation that means anything.
- **The suite shares one topology across tests.** Tests clean up after themselves and establish their own preconditions - the auto-init test de-initializes the cluster first, for instance - but an interrupted run leaves state behind. If results stop making sense, start clean.
- **`test/e2e` is its own module and is deliberately unvendored.** Running `go mod tidy` there is part of changing it; CI checks it separately from the root module.
- **Only one deploy or removal Job runs per cluster at a time.** A row of releases sitting in `Pending`/`WaitingForActiveDeploy` while one is `Deploying` is the design working, not a stall.
