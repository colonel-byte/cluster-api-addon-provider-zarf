# Design: ordered package bundles

**Status: proposed.** Nothing here is implemented. Tracked in [#379](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/issues/379). This records the problem, the shape that solves it, and the questions worth settling before a reconciler exists.

## The problem

A `ZarfPackageProxy` declares one package and one component selection for every cluster it matches. That is the right unit for almost everything, and it cannot express a bootstrap, because a bootstrap is a *sequence in which the same package is deployed more than once with a different component selection each time*.

The reference case is a cluster with no storage and no registry, which is the case this provider exists for. [`bootstrap-order.md`](../guides/bootstrap-order.md) describes the cycle and the four ways out of it; the one an operator actually runs, when no reachable registry exists and storage is a Zarf package, is a staged sequence. Here is the real one, from `uds-bundles/minimus/init-ceph/uds-bundle.yaml` in the sister repository:

| Stage | Package         | Components                                                             | Why                                                               |
| ----- | --------------- | ---------------------------------------------------------------------- | ----------------------------------------------------------------- |
| 1     | `zarf-init`     | `zarf-injector`, `zarf-seed-registry`                                  | Zarf state plus an ephemeral registry, which needs no storage     |
| 2     | `csi-rook-ceph` | images, `operator-seed-chart`, `cluster-seed-chart`, `registry-bucket` | Storage, pulled from the seed registry, exposing an object bucket |
| 3     | `zarf-init`     | `zarf-registry`, `zarf-agent`, `git-server`                            | The permanent registry, now that it has somewhere to live         |
| 4     | `csi-rook-ceph` | images, `registry-bucket`                                              | Re-push the storage images into the permanent registry            |

Both packages appear twice. Stage 4 carries no charts at all - it exists so the cluster stops depending on content that only the ephemeral seed registry held.

## Why the existing API cannot express it

Four separate reasons, each sufficient on its own:

- **`dependsOn` orders proxies, not deploys.** It can hold package B until package A's release reports deployed. It has no way to say "run A again, differently, after B".
- **A proxy's spec is fleet-wide; stage progress is per-cluster.** Cluster A may be at stage 3 while cluster B is still at stage 1. So this cannot be a controller that edits one proxy's component list, because one list serves every matched cluster.
- **The init package cannot be split into per-stage packages.** Measured, not argued: the seed and permanent registries are one Helm release lineage, so two packages carrying one revision each fight over it. See [Why the init package cannot be split](../guides/bootstrap-order.md#why-the-init-package-cannot-be-split).
- **One package name is one deployed-package record.** Two proxies managing one package would both own that record, and there is no `takeOwnership` knob to hand it over. Staging therefore has to be one object's business.

What remains is a human editing a component list between passes, which is what the Ansible collection does imperatively and what this provider asks for in a GitOps commit. That is acceptable for one cluster and wrong for a fleet, because the edit has to land when *every* cluster is ready for it.

## Proposed shape

Two types, mirroring the `ZarfPackageProxy`/`ZarfPackageRelease` split exactly, for the same reason that split exists: the declaration spans a fleet, the state is per cluster.

**`ZarfPackageBundle`** - fleet-scoped. A cluster selector and an ordered list of stages. A stage is everything a proxy's `spec.package` already carries, plus its own lifecycle:

```yaml
apiVersion: addons.colonel-byte.dev/v1alpha1
kind: ZarfPackageBundle
metadata:
  name: rook-ceph-bootstrap
  namespace: default
spec:
  clusterSelector:
    matchLabels:
      storage: rook-ceph
  stages:
    - name: init-seed
      strategy: ApplyOnce
      package:
        repository: registry.internal/zarf/init-slim
        version: v1.0.1
        api: v1beta1
        signatureKeySecretRef:
          name: cosign-pubkey
      components:
        - name: zarf-injector
        - name: zarf-seed-registry
    - name: storage-seed
      strategy: ApplyOnce
      package:
        repository: registry.internal/zarf/csi-rook-ceph
        version: v1.21.2
      components:
        - name: operator-images
        - name: cluster-images
        - name: operator-seed-chart
        - name: cluster-seed-chart
        - name: registry-bucket
    - name: registry
      strategy: Reconcile
      package:
        repository: registry.internal/zarf/init-slim
        version: v1.0.1
      components:
        - name: zarf-registry
        - name: zarf-agent
    - name: storage-final
      strategy: Reconcile
      package:
        repository: registry.internal/zarf/csi-rook-ceph
        version: v1.21.2
      components:
        - name: operator-images
        - name: cluster-images
        # The plain charts, not the seed ones: by now the agent is running and rewrites
        # image references itself, so the templated addresses are no longer needed - and
        # keeping them would hard-code the registry address into the release's values.
        - name: operator-chart
        - name: cluster-chart
        - name: registry-bucket
```

**`ZarfBundleRelease`** - one per matched cluster, owner-referenced to the `Cluster` so it garbage-collects with it, exactly as a `ZarfPackageRelease` does. Its status carries **per-stage** state rather than a single cursor: the stage name, the digest that was applied, and when.

## Reuse is a constraint, not an aspiration

A bundle should read as an extension of the existing API, not a second dialect beside it. Concretely: **a stage is a `ZarfPackageProxy` spec with the fleet-level fields lifted out**, and every type below is shared verbatim rather than redeclared.

| Existing type                                | Where it appears in a bundle                                                                                        |
| -------------------------------------------- | ------------------------------------------------------------------------------------------------------------------- |
| `metav1.LabelSelector`                       | `spec.clusterSelector`, the same fleet selection a proxy performs                                                   |
| `ZarfPackageSource`                          | `spec.stages[].package`, unchanged - repository, version, api, signature key, and every registry reachability field |
| `ComponentSelector`                          | `spec.stages[].components`                                                                                          |
| `DeploymentStrategy`                         | `spec.stages[].strategy`, with `ApplyOnce` and `Reconcile` keeping their meanings                                   |
| `DeletionPolicy`                             | `spec.deletionPolicy`, bundle-wide, defaulting to `Retain` rather than `Delete`                                     |
| `DeployJobOptions`                           | `spec.deployJob`, bundle-wide - the cache volume and node placement are a property of the cluster, not of a stage   |
| `apiextensionsv1.JSON` / `map[string]string` | `spec.stages[].values` and `spec.stages[].set`, rendered through the same `{{ .Cluster... }}` templating            |
| `NamespacedReference`                        | every secret and configmap reference inside a stage, resolving through the same `--secret-namespaces` gate          |
| `ReleasePhase`, `metav1.Condition`           | `ZarfBundleRelease.status`, with the same phases and condition vocabulary                                           |
| `corev1.LocalObjectReference`                | `ZarfBundleRelease.spec.clusterRef`, as a `ZarfPackageRelease` already does                                         |

What that leaves genuinely new is small, and worth stating because it is the whole design:

- `spec.stages[]` - an ordered list, where `name` identifies a stage independently of its position
- `ZarfBundleRelease.status.stages[]` - per-stage applied digest and timestamp, the per-cluster state a cursor cannot carry
- a `bundleRef` on the release, mirroring `packageProxyRef`

Three fields are deliberately **not** carried onto a stage, and the reason is the same in each case - they are fleet-level, and a bundle already is:

- `dependsOn` - a bundle's order *is* the dependency, so a per-stage `dependsOn` would be two mechanisms describing one thing
- `init.autoInit` - a bundle expresses the init explicitly as stage 1; probing for state to decide whether to init is exactly what an `ApplyOnce` stage does
- the follow-tag annotations - a bundle that re-pins itself mid-bootstrap is not a behaviour anybody asked for

## Why per-stage state rather than a cursor

A cursor integer ("this cluster has reached stage 3") cannot answer the question an operator actually has, which is *what happens when I bump one thing*. Recording the applied digest per stage answers it:

| Change                                    | Re-runs                                                                                      |
| ----------------------------------------- | -------------------------------------------------------------------------------------------- |
| `csi-rook-ceph` version bumped            | `storage-final`, which carries its plain charts; `storage-seed` is `ApplyOnce` and stays put |
| `init-slim` version bumped                | `registry`; `init-seed` is `ApplyOnce` and stays put                                         |
| A stage's `components` or `values` edited | that stage                                                                                   |
| Nothing                                   | nothing - a converged bundle is quiet                                                        |

This is what recovers the property that splitting packages was supposed to buy: **the CSI and the init package keep independent lifecycles even though one object sequences them.** They are independent because their stages are, not because their packages are.

## Why a stage needs its own `strategy`, and why `ApplyOnce` cannot reuse the existing probe

Some stages are true of a cluster exactly once. `init-seed` stands up an ephemeral registry that stage 3 replaces; re-running it after stage 3 would re-apply seed values over the permanent registry - the revert measured in the spike, which with upstream's `seedRegistry.persistence.enabled: false` also strips the registry's storage configuration. `storage-seed` is the same: its seed charts are superseded, and re-applying them later points image references back at a registry that no longer holds them.

So a stage carries `strategy: ApplyOnce | Reconcile`, the same vocabulary a proxy uses, with the same meaning: deploy exactly once per cluster and never re-execute, even when the spec moves.

**The trap is how "once" is decided.** A proxy's `ApplyOnce` probes the workload cluster for Zarf's deployed-package record - `hasDeployedPackage(ctx, workload, proxy.Status.PackageName)` - and adopts when it finds one. That probe is keyed on the **package name**, and in a bundle two stages routinely name the same package. The moment `init-seed` lands, "zarf has a record for `init-slim`" is true, so a name-keyed probe would make the `registry` stage adopt-and-skip and the cluster would never get a permanent registry.

A stage's `ApplyOnce` must therefore be keyed on the bundle release's own per-stage record, which is the only thing that knows a *stage* ran. Zarf's record cannot tell stages of one package apart.

## What it reuses

Nothing about deployment changes, which is most of the value of this shape:

- resolution, digest pinning, signature verification, and the `spec.package.api` check, per stage
- the registry reachability fields - credentials, CA bundles, `fromZarfState` - per stage, since a bundle may cross registries
- `{{ .Cluster... }}` rendering of values and set entries against the target cluster
- the deploy-spec Secret, the deploy Job, and the pod's termination-message result
- the per-cluster deploy slot, which already serialises one deploy at a time per cluster - so stages are sequential per cluster for free, while different clusters progress independently
- Zarf's own deployed-package records on the workload cluster, which remain the source of truth for what is installed

## Interactions worth getting right

- **A package should be managed by a bundle or a proxy, never both.** Both would own one deployed-package record, which is the collision the spike measured one layer up. Whether the controller detects and refuses this, or documentation alone, is an open question below.
- **`clusterctl move`.** A bundle is fleet-scoped and in no cluster's object graph, so it does not move, exactly like a proxy; the `ZarfBundleRelease` is owner-referenced to the `Cluster` and is garbage-collected with it. After a move the bundle is re-applied and per-stage state is gone - which makes the adoption rule for `ApplyOnce` stages load-bearing rather than a nicety.
- **Deletion.** Bootstrap content is the last thing an operator wants uninstalled because an object was deleted. A bundle should default to retaining, which inverts the proxy's default.
- **Failure.** A failed stage holds the bundle for that cluster and later stages never start, with the failing Job kept as evidence. This matches how a failed init already behaves and it is deliberate: a half-bootstrapped cluster must not be deployed onto.

## Readiness is the package's job

A stage completes when its deploy Job completes, and the controller makes no attempt to decide whether the workload the stage installed is *working*. This is a decision rather than an omission.

The alternative - a per-stage readiness probe in the bundle spec, naming objects and conditions to wait for - would put a second health model in this provider beside the one Zarf already has, and it would have to be expressed generically enough to cover every driver anybody packages. Zarf can already wait, inside the package, where the author knows what "ready" means for that chart. So the controller observes the Job and nothing else, and a package that reports success before its workload can serve is a bug in the package.

What that obliges of a package author is written up for operators in [`bootstrap-order.md`](../guides/bootstrap-order.md#if-you-package-a-storage-driver-make-it-prove-itself-ready), because it is the difference between a bundle that sequences correctly and one that races.

Two consequences for this design:

- **A stage's timeout is the Job's timeout.** A driver whose own wait exceeds it fails the stage, so the two have to be set with each other in mind.
- **`maxTotalSeconds` inside the package is the real bound** on how long a stage can take, and it is invisible from the bundle. A stage that appears hung is usually a package waiting on something that will never happen.

## A bundle is one node in the DAG

A bundle is a **linear, isolated** run. Any `dependsOn` edge between whole objects is allowed - a proxy may depend on a bundle, a bundle on a proxy, a bundle on another bundle - and **no edge may point at a stage**. Stage 3 of a bundle cannot depend on anything; the stage list is its own and only order.

This reuses the dependency machinery unchanged rather than extending it. `dependsOn` is already evaluated per cluster, not per fleet - the condition reads `every dependency is deployed on this cluster` - so the semantics fall out:

| Edge            | Satisfied when                                                                                              |
| --------------- | ----------------------------------------------------------------------------------------------------------- |
| proxy → bundle  | every stage of that bundle has been applied on *this* cluster                                               |
| bundle → proxy  | that proxy's release is `Deployed` (or `Completed`, for `ApplyOnce`) on this cluster, before stage 1 starts |
| bundle → bundle | the same rule, applied to the whole of the other bundle                                                     |

**A bundle is all-or-nothing as a dependency target.** Partially complete is not a satisfying state, which is what makes it a single node rather than a sub-graph somebody can reach into.

Three reasons the stage-level edge is excluded, beyond keeping the first revision small:

- **It would be a second ordering mechanism over one object.** The stage list already says what runs when; a per-stage `dependsOn` would let the two disagree, and nothing good follows from that.
- **Cycle detection would have to span `(object, stage)` pairs** rather than objects. Today a cycle is found over proxy names; a stage-level graph makes every bundle an N-node sub-graph, and the existing `DependencyCycle` detection would have to be rewritten rather than reused.
- **A half-run bundle as a dependency target has no good answer** for status or for the deploy slot. "Stage 3 is done, stage 4 is not" is not a state another object should be able to build on, because the bundle is not finished converging and may yet re-run a stage.

**The restriction costs nothing, which is why it is the right first revision.** If something genuinely has to happen between two stages, that is two bundles with a proxy between them - expressible entirely with object-level edges:

```
bundle: seed-and-storage  ──▶  proxy: something-else  ──▶  bundle: registry-and-images
```

So the linear-bundle rule never blocks a sequence; it only decides which object expresses it. A bundle that wants an external dependency in the middle is a bundle that should have been two.

One thing this leaves open, and it is small: **how a `dependsOn` entry resolves now that two kinds answer to a name.** The field is a bare string list resolved in the proxy's namespace. The backwards-compatible reading is that a name matches a proxy or a bundle in that namespace, and a name matching *both* is refused with a condition rather than guessed at - which keeps the existing field shape and makes the ambiguity detectable. A typed `{kind, name}` reference would be cleaner and would change an existing field.

## Two managers of one package is a hard stop

A package managed by both a bundle and a proxy on one cluster is the shared-record collision one layer up from the Helm one: both own `zarf-package-<name>`, so whichever ran last owns it, removing either uninstalls it, and re-running the other reverts it. The controller refuses to deploy either side rather than picking one, which is the stance it already takes wherever intent is ambiguous - the init gate will not guess at a half-initialized cluster, and `DependencyNotApplicable` is an error rather than a silent auto-satisfy.

**Both sides hold, not just the newcomer.** There is no ordering between two declarations that makes one of them the intruder, and holding only the second would let whichever reconciled first keep deploying over the other's work. Symmetric refusal is also deterministic under a race: two objects created together both observe the same conflict and both hold.

**It is a hold, not a failure.** Level-triggered like every other verdict here: narrow a selector, drop a stage, delete one object, and the next reconcile clears it with no operator action and no retry timer.

**The DAG stop falls out for free.** A held package never reaches `Deployed`, so anything with `dependsOn` pointing at it is already held by `WaitingForDependency`. Nothing new is needed to stop dependents, and the blast radius stays the conflicting objects plus their dependents rather than the fleet - unrelated proxies on that cluster carry on.

### What counts as a conflict

Three things have to be true at once, and each of them narrows it:

- **The same *resolved package name*,** not the same repository and not the same object name. The record is keyed on the name the artifact declares, which the controller only learns at resolution and holds in `status.packageName`. Two objects naming different repositories that both build a package called `init-slim` do conflict; two objects on one repository that resolve to different names do not.
- **On the same cluster.** Two managers whose selectors never overlap are not in conflict, and saying otherwise would make a fleet-wide refusal out of a per-cluster fact.
- **Between distinct managing objects.** One bundle naming one package in several stages is the entire point of bundles and must never trip this. The check is over managers, never within one.

Two consequences of keying on the resolved name. **This cannot be an admission check** - no CEL rule or webhook can see a name that only exists after a registry read, so the refusal lands as a condition after resolution and before any Job is stamped. And **the scan is not single-namespace**: with `--cluster-namespaces`, proxies in other namespaces may select the same cluster, so the search covers every namespace the controller is permitted to select from. Both kinds are read from the informer cache, as §5C's cycle walk already does.

### Loudly means every object an operator might look at

The point of "loudly" is that whichever object somebody opens tells them the whole story, including the name of the other one:

| Where                                                                          | What it says                                                                       |
| ------------------------------------------------------------------------------ | ---------------------------------------------------------------------------------- |
| the bundle, and the proxy, fleet-level                                         | a `False` condition naming the other manager, the package, and the clusters        |
| the `ZarfBundleRelease` and the `ZarfPackageRelease` for each affected cluster | `Pending` with the same reason, naming the other manager                           |
| a Kubernetes Event on both objects                                             | `Warning`, because that is what `kubectl describe` shows and what alerting scrapes |
| the controller log                                                             | once per transition, not once per reconcile                                        |

Two of those four do not exist yet, and both are worth having for their own sake rather than only for conflicts.

**There is no `EventRecorder` in this controller at all.** Nothing emits Events today, so `kubectl describe` on a proxy shows an empty event list and anything scraping Events sees nothing from this provider. Wiring a recorder into the manager is small, and a conflict is a good first thing to emit: a condition is only loud to somebody already looking at the right object, while an Event reaches somebody who is not.

**`kubectl get` shows no status.** The proxy's printer columns are `Strategy`, `Version` and `Age` - nothing from `status` - so a conflict, a failed resolution and a dependency cycle all look identical at a terminal, and an operator has to know to reach for `-o yaml`. The release at least carries `Phase`.

That gap is worth closing with a `Ready`-style column and the reason beside it, which is the `kubectl get helmrelease` idiom; printer columns do support the condition filter:

```
NAME          STRATEGY    VERSION   READY   STATUS                AGE
baseline      Reconcile   1.0.0     True    Deployed              4d
rook-storage  Reconcile   1.21.2    False   PackageManagedTwice   4d
```

**And that is why the status-shape question has to be answered first.** A one-line status needs one aggregate condition, and neither kind has one - the proxy's conditions are `DependenciesValid`, `PackageResolved`, `PackageSignatureVerified` and `TagDrift`, none of which means "this object is fine". So the printer column is the forcing function for the aggregate rather than a task beside it: decide what `Ready` means for a fleet-scoped object whose clusters may disagree, and the column follows for both kinds at once. A conflict is a useful case to design against, being unambiguously not-Ready on every cluster it touches.

Printer columns are additive and render into `config/crd/bases`, so they break nothing and can land ahead of bundles.

### The migration path this forces, which is the point

Moving a package from a proxy to a bundle (or back) cannot be done by declaring both and letting one win. It is deliberate and two-step: set `deletionPolicy: Retain` so nothing uninstalls, delete the old manager, then let the new one adopt the cluster's existing record through the `ApplyOnce` probe. The hard stop is what makes that the only path, instead of a race whose outcome depends on which object reconciled first.

This is also where a `takeOwnership` knob would eventually fit - §3's name-discontinuous handoff - and the hard stop is a better default than that knob, because it fails closed while the knob fails open.

## Re-running a stage

A stage re-runs when its **applied identity** no longer matches what the bundle declares. That identity is the resolved digest plus the stage's own `components`, `values` and `set` - content changes move the digest, selection changes move the spec, and either is a reason to deploy again. A stage whose identity is unchanged is not re-run, which is what makes a bundle's packages independently updatable.

Two rules, and they do not conflict the way the open question feared:

- **Only changed stages re-run.** Bumping one package does not re-deploy the others.
- **Re-runs happen in stage order.** If two stages changed, the earlier one goes first. There is no case where a later stage is applied before an earlier one that also needs applying.

The reason those can both hold is the per-stage `strategy`. The feared case - a changed early stage reverting a later one - is a stage that should have been `ApplyOnce`, and that is a property of the package, not of the ordering algorithm.

### Which stages need `ApplyOnce`, precisely

> A stage must be `ApplyOnce` exactly when re-running it would re-apply values that a later stage supersedes **on the same Helm release**.

Worked through on the reference bundle, where the storage driver ships its charts in two flavours - `-seed`, whose image repositories are templated to `{{ .State.Registry.Address }}`, and plain, whose references stay upstream:

| Stage           | Carries                          | Strategy    | Why                                                                                   |
| --------------- | -------------------------------- | ----------- | ------------------------------------------------------------------------------------- |
| `init-seed`     | injector, seed registry          | `ApplyOnce` | Same release as `registry`, with values that stage supersedes - re-running reverts it |
| `storage-seed`  | images, **seed** charts, bucket  | `ApplyOnce` | Same releases as `storage-final`, whose plain charts supersede these                  |
| `registry`      | permanent registry, **agent**    | `Reconcile` | The convergent half of the init package                                               |
| `storage-final` | images, **plain** charts, bucket | `Reconcile` | The end state, and where storage upgrades flow                                        |

**The last stage must apply the plain charts, not merely re-push images.** This is the part that is easy to get wrong, and the reference bundle as originally written gets it wrong - its final stage selects images and the bucket but no charts, so the driver runs on seed values forever.

Both forms resolve, which is why the mistake is quiet. Zarf pushes every image twice (`src/pkg/images/push.go`): once as `<registry>/<path>:<tag>-<crc32>`, which is what the agent's rewrite produces, and once as `<registry>/<path>:<tag>`, which is what a hand-templated address asks for. So a cluster left on seed values pulls successfully and looks healthy.

What it has lost is that **the agent re-evaluates image references on every pod admission, while templated values were evaluated once.** Seed values hard-code the registry address into the Helm release, so they are correct only while nothing about the registry ever changes - a mode switch between nodeport and proxy, a port change, a move to an external registry, or any re-init that re-addresses it leaves every pod in that release pointing somewhere that no longer serves. The plain charts delegate to the agent, which is the component whose job this is, and which reads the address from state each time.

That is also why the seed flavour exists at all: during stage 2 the agent is not running yet - `registry` installs it - so there is nothing to do the rewriting and the addresses must be templated by hand. The seed charts are scaffolding for exactly one stage, and leaving them in place is leaving the scaffolding up.

Two consequences follow, and both are improvements:

- **Storage upgrades flow through the final stage**, which is `Reconcile` and carries the charts. Bumping the storage package upgrades the driver.
- **The seed stage is genuinely `ApplyOnce`**, by the criterion above rather than by instinct: its values are superseded on the same Helm releases.

### The hazard this leaves

A stage can be correct only because of an *earlier* stage's side effect, and the controller cannot see that. `storage-final` needs its images to live in the registry `registry` stood up; if a re-run of `registry` ever recreated that registry's storage rather than upgrading it in place, the images would be gone and `storage-final` would need to re-run even though its own identity did not change.

Nothing in the stage list expresses "re-run me whenever an earlier stage does", and a first version should not invent it. The operator's lever is to bump both, and the design's obligation is to say so rather than to imply changed-stage-only is always sufficient. A per-stage `rerunAfterEarlierStages` flag is the obvious later addition if real bundles need it; a flag nobody has needed yet is not.

## Adopting a cluster the bundle has not seen

A bundle will meet clusters it did not bootstrap: ones brought up by hand or by the Ansible collection, ones whose `ZarfBundleRelease` was garbage-collected when `clusterctl move` deleted the source `Cluster`, and ones adopted into GitOps after the fact. It has to decide whether each `ApplyOnce` stage already happened there.

**The two ways of being wrong are not symmetric, and that decides the bias.** Deciding "not satisfied" when a stage did run re-applies seed values over a converged cluster - measured to revert the permanent registry, and with upstream's `seedRegistry.persistence.enabled: false` to strip its storage configuration. Deciding "satisfied" when a stage did not run means a later stage deploys onto a cluster that is missing something and fails, loudly, with the failing Job kept as evidence. The first is destruction; the second is a recoverable error. So where the evidence is ambiguous, lean towards satisfied.

| Option                                               | Cost                                          | Why it is or is not enough                                                                                                                                                                                                           |
| ---------------------------------------------------- | --------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| **A. Zarf state exists on the cluster**              | nothing - `hasZarfState` already exists       | Too coarse: state is evidence about the *init* package, yet it would satisfy every `ApplyOnce` stage, including a storage seed stage that never ran                                                                                  |
| **B. Zarf's record for that stage's own package**    | nothing - `hasDeployedPackage` already exists | Per-stage evidence from the cluster itself. Cannot tell two stages of one package apart, which does not matter here: if the init package was deployed at all, re-running its seed stage is the dangerous act                         |
| **C. An explicit operator declaration**              | a field or a Cluster annotation               | Unambiguous and auditable, but it is per-cluster toil, and forgetting it is the destructive direction                                                                                                                                |
| **D. Refuse and hold until told**                    | a condition and a documented runbook          | Never destructive and never silently wrong, which is this controller's house style - but every migrated cluster then needs a human touch, where a proxy needs none                                                                   |
| **E. Keep the stage record on the workload cluster** | a secret the deploy pod writes                | Solves move, re-creation and adoption at once, and matches the architecture's own principle that *Zarf's record of what is deployed lives on the workload cluster, not on the management cluster*. The most robust and the most work |

**B is the one to start with, and it is also the cheapest thing that is not wrong.** It needs no API, no operator action and no new write path - `hasDeployedPackage(ctx, workload, <stage's package name>)` is the probe a proxy's `ApplyOnce` already uses. An `ApplyOnce` stage whose package Zarf has no record of has not run here; one it does have a record of is treated as done. That is correct for the converged cluster, correct for the mid-bootstrap cluster, and correct after a `clusterctl move`.

Two holes B leaves, neither of which argues for a different starting point:

- **A driver installed by something that is not Zarf** - a plain Helm install, or a `ClusterResourceSet` - leaves no Zarf record, so the seed stage runs and collides with the existing Helm release. That is the `takeOwnership` case, and it is already a known gap for proxies rather than something bundles introduce.
- **A cluster whose init genuinely half-finished** - Zarf writes state early, so a record can exist for an init that never stood up a registry. B skips the seed stage and the next stage fails, which is the recoverable direction and the same thing the init gate already refuses to guess about.

E is where this goes if B's holes start being hit in practice. It is worth preferring over C and D when that day comes, because it removes the question rather than delegating it: a bundle that remembers its own stages on the cluster they ran against does not need to infer anything, and `clusterctl move` stops being a special case for bundles the way it already is not one for Zarf's own records.

## One aggregate condition, and the column that needs it

A fleet-scoped object has no single truth to report: its clusters may disagree, and `kubectl get` has one line to say so in. This is the question the printer columns ran into, and it is worth answering for the existing `ZarfPackageProxy` before a bundle exists - a bundle then reuses the answer rather than inventing a second one.

**The event and column machinery is already in place, which makes this cheap.** Condition events are emitted from the single status-write choke point, so a new aggregate condition emits Events on transition with no extra wiring, and the printer columns are already reading conditions by type. What is missing is the condition itself.

Three shapes, and they are not equally honest:

| Shape                                                                             | What `kubectl get` says                      | Problem                                                                                       |
| --------------------------------------------------------------------------------- | -------------------------------------------- | --------------------------------------------------------------------------------------------- |
| **Proxy-local only** - resolution, dependencies, spec sanity                      | `Ready=True` while nothing deployed anywhere | Says the proxy is fine when the fleet is not, which is the blindness the columns exist to fix |
| **Worst-of-releases** - True when every matched cluster is `Deployed`/`Completed` | `Ready=False`, reason from the worst cluster | Honest, but one broken cluster out of two hundred reads the same as total failure             |
| **Counts beside the condition**                                                   | `READY 11/12` and the worst reason           | Needs integer status fields, because printer columns cannot take the length of a list         |

**Worst-of-releases for the condition, counts for the columns.** The condition is what Events and `dependsOn`-style gating can key on, and it should mean "the fleet has converged", because that is the question an operator is actually asking. The counts are what stop a single bad cluster reading as a dead fleet. CAPI's own objects do exactly this - `MachineDeployment` carries `readyReplicas` and friends as integers precisely because a column cannot count a list.

The roll-up is nearly free to compute: the reconciler already resolves `status.matchingClusters` and already looks up each cluster's release through `findRelease`, so counting the ones in `Deployed` or `Completed` is a pass over work it has done. The reason to surface on failure is the worst release's `Ready` reason - which is why `WaitingForDependency` or `PackageManagedTwice` ends up on the proxy's own line, naming the actual blocker rather than a generic failure.

Two sub-questions this leaves, both small and both worth settling with it:

- **A proxy matching zero clusters.** `Ready=True` reads as converged when nothing has happened; `Unknown` with a reason naming it is more honest, and is what CAPI uses for "not yet determined".
- **Which columns earn the default width.** `READY`, a count, and the worst reason are three, on top of `Strategy`, `Version` and `Age`. Something has to move to `-o wide`, and `Strategy` is the least urgent of them.

None of this is blocked on bundles, and bundles are not blocked on it - a `ZarfBundleRelease` reports per-cluster state either way. It is sequenced first only because the answer should be one answer.

## Editing a stage list that clusters have already run

A bundle's stages are an ordered list, and a per-cluster record says which of them ran. Those two can disagree structurally rather than just by digest, and the question is what the controller does about it.

**A stage's identity is its name**, which is why stages are named rather than positional - and that is also the hazard: **a rename reads as a removal plus an insertion.** The new name has never been applied, so it runs. On a `Reconcile` stage that is an idempotent re-deploy. On an `ApplyOnce` seed stage it is the destructive case this design has spent most of its decisions avoiding: re-running `init-seed` reverts the permanent registry. A design that does not say something about renames has a footgun in it.

The five edits, and what each one means on a cluster that has already finished the list:

| Edit                 | The question it raises                                                                                                                                            |
| -------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Append at the end    | None. A new stage has never run, so it runs. The only unambiguous edit                                                                                            |
| Insert in the middle | Does it run after the stages that were declared after it? If yes the declared order was a lie for this cluster; if no the declaration is a lie about this cluster |
| Remove               | What it deployed stays - nothing uninstalls. Is the record kept as history or pruned?                                                                             |
| Reorder              | The applied order is a historical fact and cannot be changed retroactively                                                                                        |
| Rename               | Indistinguishable from remove-plus-insert unless something says otherwise                                                                                         |

### The options

- **A. Forward-only.** The set of applied stage names is the state; any stage not in it runs, in declared order relative to other unapplied stages. An insert therefore runs late, a reorder affects only what has not run, and a removal leaves its record as history. Simple, never destructive, and the declaration is still exactly right for clusters that join later - the cost is that one document means two things depending on when a cluster arrived.
- **B. Refuse structural divergence.** The applied stage names must remain a prefix of the declared list, in order. Anything else holds the bundle for that cluster with a condition naming the first divergence, and the operator's path is a new bundle rather than an edited one. Never silently wrong, and rigid enough that an innocent append-for-new-clusters can block a converged fleet.
- **C. Immutable stages, versioned bundles.** Reject edits other than appends outright, so changing a sequence means a new object. The strongest guarantee and the worst fit for GitOps, where editing in place is the whole workflow.
- **D. Split the list by strategy.** The `ApplyOnce` stages are history and are immutable once applied - editing or renaming one is refused as in B. The `Reconcile` stages are desired state, where inserting, removing and reordering are free and order matters only among those that need running, as in A.

**D is the one worth building, and it follows from a decision already taken.** The per-stage `strategy` already divides the list into exactly these two halves: `ApplyOnce` marks the stages whose values a later stage supersedes - the bootstrap prefix, which is a record of what happened - and `Reconcile` marks the convergent tail, which is a description of what should be true. Those are different kinds of thing and they want different edit rules. It also closes the rename hazard precisely where it bites: renaming an applied `ApplyOnce` stage is refused, so a seed stage cannot be re-run by a typo, while renaming a `Reconcile` stage costs an idempotent redeploy.

Two sub-questions D leaves, both small:

- **Is a removed stage's record pruned or kept?** Keeping it means status grows without bound on a long-lived bundle; pruning loses the only evidence that the stage ever ran, which is what an `ApplyOnce` decision depends on. Keeping it, marked as no longer declared, is the safer default.
- **What refuses an `ApplyOnce` edit - the API server or the reconciler?** CEL cannot see the per-cluster record, so this is a reconciler-time hold rather than admission, with the same consequence as every other verdict here: it is level-triggered, and putting the name back clears it.

## Open questions

These are the things to settle before writing a reconciler, and each one changes the API or the semantics.

1. **How is an `ApplyOnce` stage satisfied on a cluster the bundle has not seen before** - one bootstrapped by hand, by the Ansible collection, or before a `clusterctl move` discarded the per-cluster state? **Punted deliberately, with the option space recorded** in [Adopting a cluster the bundle has not seen](#adopting-a-cluster-the-bundle-has-not-seen). Nobody has asked for adoption yet; option B is what a first implementation should do, costs nothing, and is not the destructive direction, so this does not need settling before a reconciler exists. The options are written down so that the day somebody does ask, the thinking is not redone.
2. ~~**May a `Reconcile` stage re-run out of order?**~~ **Decided: changed stages only, always in stage order.** See [Re-running a stage](#re-running-a-stage).
3. ~~**Does a stage wait for workload health, or only for the deploy Job?**~~ **Decided: only the deploy Job.** See [Readiness is the package's job](#readiness-is-the-packages-job).
4. ~~**Is a bundle allowed to interleave with `dependsOn`?**~~ **Decided: at the object level, yes; at the stage level, never.** See [A bundle is one node in the DAG](#a-bundle-is-one-node-in-the-dag).
5. **What does editing the stage list mean** - inserting a stage in the middle, removing one, reordering, or renaming one? Still open; the options are in [Editing a stage list that clusters have already run](#editing-a-stage-list-that-clusters-have-already-run), along with the one hazard that is sharper than it looks.
6. **Status shape**, and it is the next thing worth building rather than a bundle prerequisite. See [One aggregate condition, and the column that needs it](#one-aggregate-condition-and-the-column-that-needs-it).
7. ~~**Conflict detection with proxies.**~~ **Decided: refuse, on both sides, loudly.** See [Two managers of one package is a hard stop](#two-managers-of-one-package-is-a-hard-stop).

## Out of scope for a first version

Named here so they are not mistaken for oversights: rollback, parallel stages, retry on a timer (a failed stage holds and an edit re-runs it, matching today), per-stage architecture selection, and any attempt to generate a bundle from a `uds-bundle.yaml` automatically.

## One measurement that makes stage 4 make sense

In nodeport mode the registry's address in Zarf state is the same before and after the handover - the spike observed `127.0.0.1:31999` and a `NodePort:31999` service after the seed stage *and* after the permanent one. That is why stage 2's seed charts, whose image references are templated to `{{ .State.Registry.Address }}`, remain correct after stage 3 replaces the registry behind that address, and why stage 4 only has to re-push images rather than re-install charts.
