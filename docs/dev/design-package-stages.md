# Design: ordered stages on a package proxy

**Status: proposed.** Nothing here is implemented. Tracked in [#379](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/issues/379).

This replaces an earlier draft that proposed a separate `ZarfPackageBundle` kind with its own per-cluster release. The problem it solves is unchanged; the shape is not, for the reason in [The proxy is already a two-stage pipeline](#the-proxy-is-already-a-two-stage-pipeline). Revised 2026-11-27: the init step now stays outside the stage list - see [Init stays outside the stage list](#init-stays-outside-the-stage-list) - which also gives [#381](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/issues/381)'s registry-mode knobs their home.

## The problem

A `ZarfPackageProxy` declares one package and one component selection for every cluster it matches. A bootstrap is not that shape: it is a sequence in which **the same package is deployed more than once with a different component selection each time**, interleaved with another package doing the same.

The reference case is a cluster with no storage and no registry, which is the case this provider exists for. [`bootstrap-order.md`](../guides/bootstrap-order.md) describes the cycle and the ways out of it; the one an operator actually runs, when no reachable registry exists and storage is a Zarf package, is a staged sequence. Here is the real one, from `uds-bundles/minimus/init-ceph/uds-bundle.yaml` in the sister repository:

| Stage | Package         | Components                                                             | Why                                                               |
| ----- | --------------- | ---------------------------------------------------------------------- | ----------------------------------------------------------------- |
| 1     | `zarf-init`     | `zarf-injector`, `zarf-seed-registry`                                  | Zarf state plus an ephemeral registry, which needs no storage     |
| 2     | `csi-rook-ceph` | images, `operator-seed-chart`, `cluster-seed-chart`, `registry-bucket` | Storage, pulled from the seed registry, exposing an object bucket |
| 3     | `zarf-init`     | `zarf-registry`, `zarf-agent`, `git-server`                            | The permanent registry, now that it has somewhere to live         |
| 4     | `csi-rook-ceph` | images, **plain** charts, `registry-bucket`                            | The end state, pulled from the permanent registry                 |

Both packages appear twice. Today the only way to express that through this provider is an operator editing a component list between passes - a GitOps commit per stage, which has to land when *every* matched cluster is ready for it. That is acceptable for one cluster and wrong for a fleet.

## The proxy is already a two-stage pipeline

The first draft argued a new kind was needed. It is not, because the concept already exists in this codebase - hardcoded at two stages. Everything a sequence needs is there, in pairs:

| Concept                              | Already there, twice                                                                     |
| ------------------------------------ | ---------------------------------------------------------------------------------------- |
| A package source                     | `spec.package` and `spec.init.package`                                                   |
| A resolution result in status        | `status.resolvedDigest` and `status.initResolvedDigest`, with their refs                 |
| A document in the deploy-spec Secret | `deploy-spec.yaml` and `init-spec.yaml`, in one Secret                                   |
| A Job per release                    | `buildJob(..., "-init")`, `"-deploy"`, `"-remove"`                                       |
| Sequencing between them              | `convergeInit` stamps the init Job and `settleInitJob` holds the deploy until it settles |

`spec.init` plus `spec.package` *is* a two-stage sequence: deploy this package with these components first, then that one. So stages are not a new concept to add. **They are an existing concept with the 2 taken out** - with one asymmetry the shape below preserves: the init half is gated on observed cluster state (`hasZarfState`), not on its position in the pair, and that distinction is load-bearing. See [Init stays outside the stage list](#init-stays-outside-the-stage-list).

Two things follow that the first draft got wrong. Per-stage Job names are not a new pattern - a release already has three Jobs - so `<release>-<stage>-deploy` is the same trick with a wider suffix. And ordering is not wholly unimplemented either: init-before-deploy exists, it is simply not a loop.

## The shape

Two additions, both to types that already exist.

**On `ZarfPackageProxy`:** the sources hoisted so a version is declared once, an ordered list of stages over them, and `spec.init` reshaped into the init settings the next section describes.

```yaml
apiVersion: addons.colonel-byte.dev/v1alpha1
kind: ZarfPackageProxy
metadata:
  name: rook-ceph-bootstrap
  namespace: default
spec:
  clusterSelector:
    matchLabels:
      storage: rook-ceph
  packages:
    zarf-init:
      repository: registry.internal/zarf/init-slim
      version: v1.0.1
      api: v1beta1
      signatureKeySecretRef:
        name: cosign-pubkey
    csi:
      repository: registry.internal/zarf/csi-rook-ceph
      version: v1.21.2
  init:
    autoInit: true
    packageRef: zarf-init
    components:
      - name: zarf-injector
      - name: zarf-seed-registry
    registry:
      mode: proxy
      port: 5000
    injectorImage: registry.internal/upstream/kube-proxy:v1.31.0
  stages:
    - name: storage-seed
      packageRef: csi
      strategy: ApplyOnce
      components:
        - name: operator-images
        - name: cluster-images
        - name: operator-seed-chart
        - name: cluster-seed-chart
        - name: registry-bucket
    - name: registry
      packageRef: zarf-init
      strategy: Reconcile
      components:
        - name: zarf-registry
        - name: zarf-agent
    - name: storage-final
      packageRef: csi
      strategy: Reconcile
      components:
        - name: operator-images
        - name: cluster-images
        # The plain charts, not the seed ones - by now the agent is running and rewrites
        # image references itself, so the templated addresses are no longer needed.
        - name: operator-chart
        - name: cluster-chart
        - name: registry-bucket
```

Mapped onto [the reference bundle](#the-problem): its stage 1 is `spec.init` here, and its stages 2-4 are the three declared stages.

**Hoisting the sources is not cosmetic.** A stage list of near-complete sources means a version bump is edited in two places, and a half-finished edit produces two digests of one package name - two revisions of one Helm release lineage with disagreeing values, which is the measured revert in [`bootstrap-order.md`](../guides/bootstrap-order.md#why-the-init-package-cannot-be-split) reintroduced inside a single object. A `packageRef` makes that disagreement structurally impossible.

**On `ZarfPackageRelease`:** `status.stages[]`, one entry per stage that has run on this cluster, carrying the stage name, the applied identity and when. The release is already the per-cluster object, already owner-referenced to the `Cluster`, and already carries the per-cluster phase - so per-cluster stage progress belongs here and nowhere new.

**And `spec.package` becomes sugar for a single stage carrying `spec.strategy`.** `spec.init` is *not* sugar for a stage - the next section is why - but the rest of the claim holds: internally everything is a stage list, every proxy written before this existed is the one-stage case, there is no `oneOf` for a reader to learn, and no second code path to keep in step.

## Init stays outside the stage list

An earlier revision of this document expressed zarf-state creation as the first stage - `init-seed`, components `zarf-injector` plus `zarf-seed-registry`. It is not a stage, for two reasons that are load-bearing in the code. First, the init gate is a *cluster-wide eligibility* rule, not a per-proxy ordering rule: on a cluster with no zarf-state, `convergeDeploy` holds every release whose package is not a `ZarfInitConfig`, whichever proxy declares it - a rule with no representation as "stage 1 of this proxy's list". Second, satisfaction is the `hasZarfState` probe, which is answered by *anyone's* init - another proxy, the Ansible collection, a hand-run `zarf init` - where a positional `ApplyOnce` stage can only ever be satisfied by its own applied record. Together with `settleInitJob`'s failed-after-state handling, that is what lets a staged proxy commute with a fleet that manages init elsewhere, as `InitOptions.AutoInit`'s doc comment already promises.

So the split is: **`spec.init` is the settings for creating zarf-state on a cluster that has none** - which package, which components, and how - and **`spec.stages` is the ordered list of deployments after state exists**. Consequences:

- **`spec.init` gains `packageRef`**, so a staged proxy declares the init package's version once in `spec.packages` even though both `spec.init` and the permanent `registry` stage use it. The inline `package` form stays as the legacy sugar.
- **Init's `ApplyOnce`-ness is the probe itself.** It has no stage record and no identity hash to mismatch: `hasZarfState` true means satisfied, whoever did it. The destructive re-run of seed values over a converged cluster cannot happen through this path. For the init package specifically, the "too coarse" objection to option A in [Adopting a cluster](#adopting-a-cluster-the-stages-have-not-run-on) does not apply - zarf-state is direct evidence about exactly this package.
- **Stages never initialize**, which dissolves a prerequisite the earlier revision carried: "stage 1 has to be init-capable, and nothing says so". A stage that needs state holds with the existing `WaitingForZarfState` reason until init settles, through `settleInitJob`'s existing shape.
- **[#381](https://github.com/colonel-byte/cluster-api-addon-provider-zarf/issues/381)'s registry knobs - `registry.mode`, `registry.port`, `injectorImage` - are declared on `spec.init`, and ride the deploy-spec to *any* `ZarfInitConfig` deploy this proxy performs.** The registry fields group under `init.registry`, mirroring upstream's `RegistryInfo`, so external mode's `address` and `credentialsSecretRef` later join the same block; `injectorImage` stays flat because upstream carries it on `DeployOptions`, not `RegistryInfo`. That covers all three init paths with one rendering path: the `spec.init` auto-init, the seed-then-manage pattern where `spec.package` itself is a `ZarfInitConfig` (so a fleet using [the handoff](architecture.md) can declare proxy mode too - an init block carrying only settings, no `package` or `autoInit`, is valid for exactly this case), and a staged proxy's permanent `registry` stage. They are `DeployOptions`-level values set in `internal/deploy.Options`, the way `plainHTTP` already is, independent of stages, and better landed first, as the issue itself argues.
- **`registry.mode` is declare-once: immutable on the proxy, and held on mismatch with the cluster.** Upstream *does* support swapping modes on re-init - `InitState` in the vendored `src/pkg/cluster/cluster.go` tracks `modeChanged`, resets the injector, and re-derives port, address and mTLS - but a swap re-addresses the registry under every release that templated the old address, which is the hard-coding hazard measured at the end of this document, so this provider refuses to drive one. A CEL transition rule (`oldSelf`) makes the field immutable, and when a cluster's existing zarf-state carries a different mode - an adopted cluster, or two proxies declaring different modes where the deploy slot made one win - the controller sets an `InitSettingsSuperseded` condition naming both modes rather than staying silent. Because `InitState` only applies a mode that is non-empty in its options, an init-package re-deploy carrying the declared mode re-asserts it and never regresses a cluster toward upstream defaults - verified against the vendored source, which settles what a `registry` stage re-run does to a proxy-mode cluster.
- **The `STAGES` column counts declared stages only.** Init is reported through the existing init status fields; folding it into the count would make `3/4` ambiguous between "init pending" and "a stage pending".
- **The CEL moves to the spec root, and the existing `InitOptions` rules move with it.** `init.packageRef` naming a key in `spec.packages` cannot be checked from inside `InitOptions` - CEL's `self` is the struct the rule sits on - so these are root-level rules: `packageRef` must name a key in `packages`; `package` and `packageRef` are mutually exclusive; `autoInit` requires `package` *or* `packageRef` (today's rule knows only `package`); the `init.set`-requires-`v1alpha1` rule learns to index `self.packages[self.init.packageRef].api` - with every optional field behind a `has()` guard, per the warning already on `ZarfPackageSource`, and still covering the inline `package` legacy form; `registry.mode: proxy` requires `injectorImage`, because proxy mode without one fails on first init; and `registry.mode` is immutable per the bullet above - including refusing to *clear* a set mode, since a delete-then-re-add across two edits would otherwise bypass the transition rule, with `InitSettingsSuperseded` as the reconciler backstop either way.

The cost is that the full sequence is written in two places - `init` plus `stages` - so the YAML no longer reads top-to-bottom as one list. That is the price of state-keyed init, and it buys the commute property above. A second consequence to name: two clusters in one fleet can take different init paths - one auto-inits, one arrives with external state - so their effective "stage 0" differs silently. That is inherent to state-keyed init, not to stages, and belongs in the operator guide.

## What this reuses literally

Not "mirrors" - reuses, with no second expression to drift:

- `matchClusters`, `stampReleases`, `findRelease`, the finalizers, the removal path
- `dependsOn`, including the cycle walk, because every edge stays proxy-to-proxy
- the per-cluster deploy slot
- the `Ready` roll-up, the cluster counts, the printer columns, the Events
- all four watches, and the release-to-proxy fan-out
- resolution, digest pinning, signature verification, the `api` claim check, and every registry reachability field, per stage
- the deploy-spec Secret, which already holds more than one document, and the deploy Job, which already has more than one name

A separate kind would have needed each of those a second time, plus a second reconciler, RBAC surface, CRD, status aggregation, docs chapter and e2e harness - and a cross-kind `dependsOn` resolver and cycle walk, which is a silent-hang bug rather than extra typing: `proxyDependencies` filters by kind, so a proxy-to-bundle-to-proxy cycle would be invisible and every member would report `WaitingForDependency` forever.

### The honest objections to a field

- **`kubectl get zarfpackageproxy` mixes two lifecycles.** A four-stage bootstrap and an ordinary one-package proxy appear in one list, and the `Version` column reads `.spec.package.version`, which a staged proxy does not have. Needs a `STAGES` column (`4/6`) and a `Version` that means the last stage's.
- **`spec.strategy` becomes ambiguous.** Per-stage strategy is the point, so the top-level field has to be refused when `stages` is set. More validation, and a reason a reader has to learn.
- **"A sequence is a different noun" is a legitimate taste argument.** `kubectl get zarfbundles` reads better than filtering proxies. It costs a cross-kind cycle walk, a cross-kind conflict detector and a second reconciler, which is what this design judges too expensive for the ergonomics.

## Per-stage state, not a cursor

A cursor integer - "this cluster reached stage 3" - cannot answer the question an operator has, which is *what happens when I bump one thing*. Recording an applied identity per stage answers it:

| Change                                    | Re-runs                                                                                                 |
| ----------------------------------------- | ------------------------------------------------------------------------------------------------------- |
| `csi` version bumped                      | `storage-final`, which carries its plain charts; `storage-seed` is `ApplyOnce` and stays put            |
| `zarf-init` version bumped                | `registry`; the seed half lives in `spec.init`, which the state probe holds put - no record to mismatch |
| A stage's `components` or `values` edited | that stage                                                                                              |
| Nothing                                   | nothing - a converged proxy is quiet                                                                    |

**The identity has to be the controller's own hash of the rendered spec, not a value the pod reported back.** `status.deployedDigest` is parsed from the pod's termination message, and `jobabsorb.go` already tolerates its absence - *"the completed deploy pod left no readable result; deployedDigest stays unset"* - because a pod evicted or deleted by hand leaves nothing to read. Today that is cosmetic, since the re-run trigger is the Job's `spec-hash` annotation. Keying stage identity on the reported digest would make a lost termination message permanent: the recorded identity stays empty, mismatches forever, and the stage hot-loops under `Reconcile` or records nothing under `ApplyOnce`. The existing `specHash` over the rendered deploy spec already covers the digest, components, values and set, so it is the thing to compare; the pod's digest stays a status field for display.

## Which stages need `ApplyOnce`, precisely

> A stage must be `ApplyOnce` exactly when re-running it would re-apply values that a later stage supersedes **on the same Helm release**.

| Step            | Carries                          | Strategy               | Why                                                                                                                 |
| --------------- | -------------------------------- | ---------------------- | ------------------------------------------------------------------------------------------------------------------- |
| `spec.init`     | injector, seed registry          | state-gated, runs once | Same release as `registry`, with values that stage supersedes - but the `hasZarfState` probe means it never re-runs |
| `storage-seed`  | images, **seed** charts, bucket  | `ApplyOnce`            | Same releases as `storage-final`, whose plain charts supersede these                                                |
| `registry`      | permanent registry, **agent**    | `Reconcile`            | The convergent half of the init package                                                                             |
| `storage-final` | images, **plain** charts, bucket | `Reconcile`            | The end state, and where storage upgrades flow                                                                      |

The seed half of the init package satisfies the criterion too - its values are superseded by `registry` on the same release - but it is enforced by the state probe rather than a `strategy` field, per [Init stays outside the stage list](#init-stays-outside-the-stage-list).

**The last stage must apply the plain charts, not merely re-push images.** The reference bundle as originally written gets this wrong - its final stage selects images and the bucket but no charts, so the driver runs on seed values forever.

Both forms resolve, which is why the mistake is quiet: `src/pkg/images/push.go` pushes every image twice, once as `<registry>/<path>:<tag>-<crc32>`, which is what the agent's rewrite produces, and once as `<registry>/<path>:<tag>`, which is what a hand-templated address asks for. What a cluster left on seed values loses is that **the agent re-evaluates image references on every pod admission, while templated values were evaluated once** - so the release hard-codes the registry address, and a mode switch, a port change or a move to an external registry leaves every pod in it pointing somewhere that no longer serves. The seed flavour exists because during the seed stage the agent is not running yet; it is scaffolding for one stage, and leaving it in place is leaving the scaffolding up.

## Re-running a stage

- **Only changed stages re-run.** Bumping one package does not re-deploy the others.
- **Re-runs happen in stage order.** If two stages changed, the earlier goes first.

Those do not conflict, because the feared case - a changed early stage reverting a later one - is a stage that should have been `ApplyOnce`, which is a property of the package rather than of the ordering algorithm.

**Ordering is the reconciler's job, and the deploy slot does not provide it.** `clusterSlotHolder` refuses to stamp a second Job while one runs on that cluster; nothing in it says stage 2 wins the slot before stage 4. It is also opt-out-able per `Cluster` through the `concurrent-deploys` annotation, so a stage list cannot lean on it as an invariant at all. What *does* carry over is `settleInitJob`'s shape: hold until the previous stage's Job has settled, then proceed.

### The hazard this leaves

A stage can be correct only because of an *earlier* stage's side effect, and the controller cannot see that. `storage-final` needs its images in the registry `registry` stood up; if a re-run of `registry` ever recreated that registry's storage rather than upgrading it in place, the images would be gone and `storage-final` would need to re-run even though its own identity did not change. The operator's lever is to bump both. A per-stage `rerunAfterEarlierStages` flag is the obvious later addition if real sequences need it; a flag nobody has needed yet is not.

## Readiness is the package's job

A stage completes when its deploy Job completes, and the controller makes no attempt to decide whether the workload it installed is *working*. A per-stage readiness probe in the spec would put a second health model beside Zarf's own and would have to be generic enough for every driver anybody packages. Zarf can already wait inside the package, where the author knows what ready means, so a package that reports success before its workload can serve is a bug in the package.

What that obliges of a package author is written up for operators in [`bootstrap-order.md`](../guides/bootstrap-order.md#if-you-package-a-storage-driver-make-it-prove-itself-ready). Two consequences here: a stage's timeout is the Job's timeout, and `maxTotalSeconds` inside the package is the real bound on how long a stage can take - invisible from the proxy, and usually the explanation for a stage that appears hung.

## Dependencies stay between objects

Any `dependsOn` edge between whole proxies is allowed, and **no edge may point at a stage**. A stage list is its own and only order.

This is now a reuse rather than an extension: every edge is proxy-to-proxy, so the existing per-cluster evaluation and the existing cycle walk apply unchanged. A proxy depending on a staged proxy is satisfied when **every** stage has been applied on that cluster - all-or-nothing, which is what makes a sequence a single node rather than a sub-graph somebody can reach into.

Three reasons the stage-level edge stays out: it would be a second ordering mechanism over one object; cycle detection would have to span `(object, stage)` pairs rather than objects; and a half-run sequence has no sane answer for status or the deploy slot. The restriction costs nothing - a sequence needing something external in the middle is two proxies with a dependency between them, which object-level edges already express.

## Two managers of one package

A package managed by two objects on one cluster is the shared-record collision: both own `zarf-package-<name>`, so whichever ran last owns it, removing either uninstalls it, and re-running the other reverts it.

**The refusal has to be narrower than "two managers", because one such pair is already a supported pattern.** `architecture.md`'s seed-then-manage handoff is exactly two managers of one package name: an `ApplyOnce` proxy seeds `zarf-init` and a `Reconcile` proxy takes over the same name for day-two lifecycle, which *"works by construction"*. A symmetric refusal keyed on "two managers, one package, one cluster" would hold both sides of every fleet using it, fleet-wide, with no obvious fix.

So the conflict is **two managers that will both re-execute**. A manager whose release is `Completed` with `applied` set will never deploy again and cannot collide with anything; excluding it leaves the handoff alone and still catches what stages introduce, which is a stage and a separate `Reconcile` proxy both converging one package name. The successor must still be `deletionPolicy: Retain`, for the reason §3 gives.

Where it does apply, both sides hold rather than just the newcomer: there is no ordering between two live declarations that makes one the intruder, and holding only the second would let whichever reconciled first deploy over the other's work.

### What counts

- **The same resolved package name**, not the same repository or object name - the record is keyed on the name the artifact declares, which the controller learns at resolution.
- **On the same cluster.** Selectors that never overlap are not in conflict.
- **Between distinct managers that can still deploy.** One proxy naming one package across several stages is the entire point and must never trip this.

Keying on the resolved name means **this cannot be an admission check** - no CEL rule can see a name that only exists after a registry read - so it is a reconciler-time hold, level-triggered like every other verdict here. The scan is also not single-namespace: with `--cluster-namespaces`, proxies elsewhere may select the same cluster.

## Status and columns

The fleet roll-up stages reuse is `internal/controller/aggregate.go`, which has already landed (commit `f059c6bd`), so stages reuse it rather than build it: a `Ready` condition plus `clusterCount`, `readyClusterCount` and `supersededClusterCount`, with the reason taken from **the first cluster that is not done, in selector order** rather than the worst by severity, because every hold is equally a reason the fleet has not converged and a stable choice beats one that churns. Condition Events come from the single status-write choke point, so any new condition emits on transition with no extra wiring.

What stages still owe:

- **A `STAGES` column**, `4/6`, which needs integer status fields for the same reason `readyClusterCount` does - a printer column cannot take the length of a list.
- **A `Version` column that means something for a staged proxy**, since it currently reads `.spec.package.version`.
- **Per-package resolution status on the proxy, keyed by `spec.packages` key: `status.packages[]`.** Resolution is a property of the *source*, not the stage - one package appears in several stages, and per-stage resolution entries would have to agree in pairs, which is the two-digests-of-one-name hazard `packageRef` exists to make unrepresentable, reintroduced in status. So one entry per `spec.packages` key, carrying `resolvedRef`, `resolvedDigest`, `packageName`, `packageKind` and the signature and drift conditions, and a stage reads its resolution through its `packageRef`. Two entries for the reference sequence, not three. Distinct from the release's applied-identity list, which stays per stage because *applying* is per stage even when resolving is not.

## Adopting a cluster the stages have not run on

Clusters will arrive that were bootstrapped by hand, by the Ansible collection, or before a `clusterctl move` discarded per-cluster state. Something has to decide whether each `ApplyOnce` stage already happened there. The init step adopts itself - `hasZarfState` is the existing probe and the existing behaviour - so this question is about `ApplyOnce` *stages*, which in the reference sequence means `storage-seed`.

**The two ways of being wrong are not symmetric, and that decides the bias.** Deciding "not satisfied" when a stage did run re-applies seed values over a converged cluster - measured to revert the permanent registry and, with upstream's `seedRegistry.persistence.enabled: false`, to strip its storage configuration. Deciding "satisfied" when it did not run means a later stage fails loudly with its Job kept as evidence. Destruction versus a recoverable error, so ambiguity leans towards satisfied.

| Option                                               | Cost                                          | Why it is or is not enough                                                                                                                                                                                        |
| ---------------------------------------------------- | --------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **A. Zarf state exists on the cluster**              | nothing - `hasZarfState` already exists       | Too coarse: state is evidence about the *init* package, yet would satisfy every `ApplyOnce` stage, including a storage seed that never ran                                                                        |
| **B. Zarf's record for that stage's own package**    | nothing - `hasDeployedPackage` already exists | Per-stage evidence from the cluster itself. Cannot tell two stages of one package apart, which does not matter here: if the init package was deployed at all, re-running its seed stage is the dangerous act      |
| **C. An explicit operator declaration**              | a field or a Cluster annotation               | Unambiguous and auditable, but per-cluster toil, and forgetting it is the destructive direction                                                                                                                   |
| **D. Refuse and hold until told**                    | a condition and a runbook                     | Never silently wrong, which is the house style - but every migrated cluster then needs a human touch where a single-package proxy needs none                                                                      |
| **E. Keep the stage record on the workload cluster** | a secret the deploy pod writes                | Solves move, re-creation and adoption at once, and matches the principle that *Zarf's record of what is deployed lives on the workload cluster, not on the management cluster*. The most robust and the most work |

**B is where to start, and it is also the cheapest thing that is not wrong.** `hasDeployedPackage(ctx, workload, <stage's package name>)` is the probe a proxy's `ApplyOnce` already uses. Two holes it leaves, neither arguing for a different start: a driver installed by something that is not Zarf leaves no record, so the seed stage collides with the existing Helm release - the `takeOwnership` gap, which already exists for proxies; and a half-finished init can leave state without a registry, where B skips the seed stage and the next one fails, which is the recoverable direction.

One thing to verify before committing to B: whether Zarf writes `zarf-package-<name>` on a partially-failed deploy. Nothing in this tree establishes it either way.

## Editing a stage list that clusters have already run

**A stage's identity is its name**, which is why stages are named rather than positional - and that is the hazard: **a rename reads as a removal plus an insertion.** The new name has never been applied, so it runs. On a `Reconcile` stage that is an idempotent redeploy; on an `ApplyOnce` seed stage it is the destructive case this design spends most of its decisions avoiding.

| Edit                 | The question it raises                                                                                                                              |
| -------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------- |
| Append at the end    | None. A new stage has never run, so it runs. The only unambiguous edit                                                                              |
| Insert in the middle | Does it run after stages declared after it? If yes the declared order was a lie for this cluster; if no the declaration is a lie about this cluster |
| Remove               | What it deployed stays - nothing uninstalls. Is the record kept as history or pruned?                                                               |
| Reorder              | The applied order is a historical fact and cannot be changed retroactively                                                                          |
| Rename               | Indistinguishable from remove-plus-insert unless something says otherwise                                                                           |

- **A. Forward-only.** Applied stage names are the state; anything else runs in declared order relative to other unapplied stages. Never destructive, and still exactly right for clusters that join later - the cost is that one document means two things depending on when a cluster arrived.
- **B. Refuse structural divergence.** Applied names must remain a prefix of the declared list; otherwise hold with a condition naming the first divergence. Never silently wrong, and rigid enough that an append for new clusters can block a converged fleet.
- **C. Immutable stages, versioned objects.** Reject edits other than appends. Strongest guarantee, worst fit for GitOps.
- **D. Split the list by strategy.** `ApplyOnce` stages are history and immutable once applied - edits and renames refused as in B. `Reconcile` stages are desired state, where inserting, removing and reordering are free and order matters only among those needing to run, as in A.

**D is the one worth building**, because the per-stage `strategy` already divides the list into exactly those two halves, and it closes the rename hazard precisely where it bites. Two sub-questions it leaves: whether a removed stage's record is pruned or kept - keeping it, marked undeclared, is safer, since it is the evidence an `ApplyOnce` decision rests on - and that the refusal is reconciler-time rather than admission, because CEL cannot see a per-cluster record.

What CEL *can* enforce, and should, is spec-only: stage names unique, and **a stage's `strategy` immutable once declared**, since flipping `ApplyOnce` to `Reconcile` turns a history stage into a convergent one and re-runs it. A full validating webhook is not worth it for the rest: the precise check needs per-cluster state, this repo has no webhook infrastructure at all, and a webhook adds an outage mode where it being down blocks every edit - to replace a hold that clears itself.

## What a first implementation needs that this does not cover

Found by review rather than by building. Each is a prerequisite rather than a nice-to-have, and none of them is cheaper under a separate kind.

- **A retry that is not a fleet-wide edit.** `BackoffLimit: 0` means one pod attempt and there is no retry timer, so a single node eviction fails a stage. The documented lever - edit the spec - changes the stage's identity on *every* cluster, redeploying hundreds of healthy ones to retry one. At several stages across a large fleet, at least one eviction per rollout is the expected case. Either a bounded per-release retry with backoff, which the removal path already models, or a way to re-run one stage on one cluster.
- **A bound on how much deploys at once.** The deploy slot is per cluster and nothing limits clusters in flight, so every cluster of a fleet created from one template enters stage 1 together - correlated bursts of multi-gigabyte pulls against one registry, which single-package proxies never produce. A `--max-concurrent-deploys` or a per-proxy `maxConcurrentClusters` is worth having on its own.
- **`spec.paused` is not honoured anywhere today.** `clusterctl move` pauses objects before moving them; this controller keeps resolving, stamping and creating Jobs regardless. With a sequence in flight, a move can leave the source cluster's Job deploying one stage while the destination's rebuilt release starts the same stage again against the same workload cluster. A provider-conformance gap with or without stages.
- **A test plan.** The four-stage Ceph case is not reproducible in the CAPD-plus-local-registry e2e topology, so as specified it can never be regression-tested. A synthetic four-stage sequence over two trivial packages that deliberately share one Helm release would exercise every decision here and does fit the existing topology.
- **Per-stage Job and deploy-spec naming.** `<release>-<stage>-deploy` plus a stage label on the Job, making Job creation the commit point with status read back off the labelled Job - otherwise a controller restart between creating a Job and writing status leaves a running Job whose stage nothing can recover, and for an `ApplyOnce` seed stage guessing wrong is the destructive direction. The 63-character name limit goes on the budget alongside a release name that is already proxy-plus-cluster derived.

## Out of scope for a first version

Named so they are not mistaken for oversights: rollback, parallel stages, retry on a timer, per-stage architecture selection, `deletionPolicy: Delete` over a staged proxy - removal of a bootstrap sequence should not be something anybody gets by deleting an object - and generating a stage list from a `uds-bundle.yaml`.

## Open questions

1. **How is an `ApplyOnce` stage satisfied on a cluster the proxy has not seen before?** **Punted deliberately**, with the option space recorded above. Nobody has asked for adoption yet; option B is what a first implementation should do, costs nothing, and is not the destructive direction.
2. **What does editing the stage list mean?** Open, with option D recommended above.

Everything else is decided: readiness is the deploy Job only; dependencies at object level and never at stage level; conflict refusal narrowed to managers that can both re-execute; changed stages only, in stage order; the final stage applies the plain charts; per-stage `ApplyOnce` by the superseded-values criterion; status shape settled and reused rather than redesigned; and the shape itself is a field rather than a kind.

## One measurement that makes the last stage make sense

In nodeport mode the registry's address in Zarf state is the same before and after the handover - the spike observed `127.0.0.1:31999` and a `NodePort:31999` service after the seed step *and* after the permanent one. That is why `storage-seed`'s seed charts, whose image references are templated to `{{ .State.Registry.Address }}`, remain resolvable after the `registry` stage replaces the registry behind that address, and why `storage-final` has to re-install the plain charts for the agent to take over rather than merely re-push images.
