# Installing the provider with Rancher Turtles

[Rancher Turtles](https://turtles.docs.rancher.com) installs Cluster API providers through the Cluster API Operator rather than through `clusterctl`, so on a Turtles management cluster the install path is one `CAPIProvider` object. Everything else about the provider - the CRDs, the controller, the flags - is the same install; this guide mirrors cluster-api-provider-cargoship's Turtles guide, adapted to a single addon provider.

## Declare the provider

The namespace is not created for you, and the operator installs the provider into the namespace of the object that asked for it. Using the one this repository's manifests already name keeps log commands portable:

```sh
kubectl create namespace caapz-system
```

<!-- x-release-please-start-version -->
```yaml
apiVersion: turtles-capi.cattle.io/v1alpha1
kind: CAPIProvider
metadata:
  name: addon-zarf
  namespace: caapz-system
spec:
  name: zarf
  type: addon
  version: v0.1.1
  fetchConfig:
    url: https://github.com/colonel-byte/cluster-api-addon-provider-zarf/releases
```
<!-- x-release-please-end-version -->

Three things in there are easy to get wrong:

- **`spec.name` is the provider name, not `metadata.name`.** Turtles resolves the provider from `spec.name` and falls back to `metadata.name` only when it is unset.
- **`spec.type` is `addon`**, lower-case. The `ClusterctlConfig` spelling below is `AddonProvider` instead.
- **`spec.version` must be semver with a leading `v`**, and it names the release tag. A CEL rule on the CRD rejects the bare form. With `fetchConfig.url` pointing at the releases page, it is also the only thing telling the operator which release to read, including the `metadata.yaml` beside the components that carries the contract version.

## Or register it centrally

Where the provider should be known to the whole cluster - or where the components come from a mirror rather than from GitHub - put the URL in Turtles' `ClusterctlConfig` and drop `fetchConfig` from the object above:

<!-- x-release-please-start-version -->
```yaml
apiVersion: turtles-capi.cattle.io/v1alpha1
kind: ClusterctlConfig
metadata:
  name: clusterctl-config
  namespace: cattle-turtles-system
spec:
  providers:
    - name: zarf
      type: AddonProvider
      url: https://github.com/colonel-byte/cluster-api-addon-provider-zarf/releases/v0.1.1/addon-components.yaml
```
<!-- x-release-please-end-version -->

The name and namespace are not free: Turtles merges this object into the `clusterctl-config` ConfigMap it ships and looks it up at that ConfigMap's own name and namespace - `cattle-turtles-system` by default, `rancher-turtles-system` on older installs. One created anywhere else is read by nothing, and nothing says so. Ask the cluster rather than guessing: `kubectl get configmap clusterctl-config --all-namespaces`.

## Air-gapped, from ConfigMaps

`fetchConfig` takes a `selector` instead of a `url`, and then the operator reads the components and the metadata out of a ConfigMap in the cluster - no route to GitHub, no mirror standing in for it. On the connected side, download `addon-components.yaml` and `metadata.yaml` from the release; inside:

<!-- x-release-please-start-version -->
```sh
kubectl create configmap v0.1.1 --namespace caapz-system \
  --from-file=components=addon-components.yaml \
  --from-file=metadata=metadata.yaml
kubectl label configmap v0.1.1 --namespace caapz-system provider-components=addon-zarf
```
<!-- x-release-please-end-version -->

Then the `CAPIProvider` loses `fetchConfig.url` and gains the selector:

```yaml
  fetchConfig:
    selector:
      matchLabels:
        provider-components: addon-zarf
```

- **The ConfigMap's name is the version the operator offers**, unless a `provider.cluster.x-k8s.io/version` label says otherwise - and it has to match the `CAPIProvider`'s `spec.version`.
- **The label is yours.** `provider-components` is only the convention the operator's examples use; nothing reads the key, the selector just has to match.
- **The components still name a `ghcr.io` image.** Mirror the controller image and override the registry through the `CAPIProvider`'s `configSecret`, whose `images` key the operator reads as a clusterctl image override.

### Or let the Zarf package carry it

The ConfigMap and the `CAPIProvider` are pre-rendered into the air-gapped Zarf package as its `rancher-turtle-artifacts` component - nothing to assemble on the inside:

<!-- x-release-please-start-version -->
```sh
zarf package deploy zarf-package-cluster-api-addon-provider-zarf-amd64-0.1.1.tar.zst \
  --components rancher-turtle-artifacts
```
<!-- x-release-please-end-version -->

It is optional, so it has to be named - `zarf package deploy --confirm` skips every optional component without a word, and a cluster installing with `clusterctl` wants exactly that. And Rancher Turtles has to be on the cluster first: a `CAPIProvider` applied where the Turtles CRDs are missing fails the component, and with it the deploy.

## Air-gapped, from a registry

`fetchConfig.oci` is the third option, for the air gap that already has a registry: the operator pulls `metadata.yaml` and the components out of one OCI artifact. Every release publishes it at **`ghcr.io/colonel-byte/turtle/addon-zarf`**, tagged with the release's own tag and cosign-signed by digest. Mirror it with `oras cp`, or - where the connected side cannot reach the mirror either - build it as a file to carry in:

<!-- x-release-please-start-version -->
```sh
go run ./magefiles/core release:ociArtifact v0.1.1
```
<!-- x-release-please-end-version -->

That writes an OCI image layout to `out/oci` and tars it beside it; nothing is pushed, deliberately. Fill the mirror on the inside with `oras cp --from-oci-layout`, then point the `CAPIProvider` at it:

```yaml
  fetchConfig:
    oci: registry.internal/caapz/capi-provider
```

A reference with no tag takes `spec.version` as the tag. A registry served over plain HTTP needs an `http://` prefix, which is how the operator decides to skip TLS. Registry credentials come from the `configSecret`'s `OCI_USERNAME`/`OCI_PASSWORD` variables, not from a pull secret. The layer names inside the artifact are the operator's *typed* lookups - `addon-components.yaml`, `metadata.yaml` - carried in each layer's title annotation, which is the only thing the operator reads a layer by.

## Check it came up

```sh
kubectl get capiproviders --all-namespaces
```

The row reaches `Ready` in `Phase`, with `zarf` as `ProviderName` and the release tag as `InstalledVersion`. A row stuck in `Provisioning` is the operator failing to fetch or apply the components, and its reason is in the Turtles controller's log rather than on the object.

## What this does not solve

`clusterctl` and Turtles are two ways to install the same component manifest, and installing with one leaves the other unaware. If the provider was installed with the Zarf package's default component instead, Turtles does not manage it and `kubectl get capiproviders` shows nothing - deploy the next package to upgrade, or delete it and declare the `CAPIProvider` to hand it over. On a Turtles cluster, the selector or OCI routes above are the better air-gapped options, because what they install is still a provider Turtles manages.
