// Copyright 2026 colonel-byte
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package v1alpha1

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// DeploymentStrategy says how a package tracks its declared state after the first deploy.
// +kubebuilder:validation:Enum=Reconcile;ApplyOnce
type DeploymentStrategy string

const (
	// StrategyReconcile continuously converges matching clusters on the declared package version,
	// redeploying on version bumps, value edits, and detected drift.
	StrategyReconcile DeploymentStrategy = "Reconcile"

	// StrategyApplyOnce deploys the package exactly once per matching cluster and never
	// re-executes it, even if the proxy's spec changes later. For seed and bootstrap payloads.
	StrategyApplyOnce DeploymentStrategy = "ApplyOnce"
)

// DeletionPolicy says what happens to a deployed package when its release is deleted - the
// declaration going away while the cluster lives on (docs/dev/architecture.md §8A).
// +kubebuilder:validation:Enum=Delete;Retain
type DeletionPolicy string

const (
	// PolicyDelete uninstalls the package from the workload cluster - a removal Job running
	// packager.Remove on the deploy Job's plumbing - before the release object may go away.
	PolicyDelete DeletionPolicy = "Delete"

	// PolicyRetain deletes the release immediately and orphans the deployed package on the
	// workload cluster, left running and operator-owned from then on. The honest choice for
	// large or foundational packages, where an orphaned-but-working package beats a
	// half-removed one.
	PolicyRetain DeletionPolicy = "Retain"
)

// FinalizerRemoval is the release finalizer carried only where a removal is owed: releases
// under policy Delete. Retain and ApplyOnce releases never carry it, so their deletion can
// never block on anything (§8B). It tracks the proxy's current policy - flipping to Retain
// strips it, the declarative break-glass for a stuck removal.
const FinalizerRemoval = "addons.colonel-byte.dev/removal"

// FinalizerReleaseCleanup is the proxy finalizer: proxy deletion deletes the proxy's releases
// and waits for their own finalization before the proxy goes away (§8B). Releases are
// deliberately not owner-referenced to the proxy - the Cluster stays the sole owner - so this
// finalizer is what makes proxy deletion reach them at all.
const FinalizerReleaseCleanup = "addons.colonel-byte.dev/release-cleanup"

// ReasonCrossNamespaceNotEnabled is the condition reason set when a clusterSelector match or an
// object reference crosses into a namespace the controller's --cluster-namespaces or
// --secret-namespaces flags do not permit.
const ReasonCrossNamespaceNotEnabled = "CrossNamespaceNotEnabled"

// ConditionDependenciesValid is the proxy condition for the reconcile-time cycle check over the
// namespace's dependsOn graph (docs/dev/architecture.md §5C). False means the proxy is part of a
// cycle and deploys nothing until the cycle is broken.
const ConditionDependenciesValid = "DependenciesValid"

const (
	// ReasonDependenciesValid is DependenciesValid: True - the proxy is in no dependsOn cycle.
	ReasonDependenciesValid = "Valid"
	// ReasonDependencyCycle is DependenciesValid: False - the proxy is a member of a dependsOn
	// cycle; the message names it.
	ReasonDependencyCycle = "DependencyCycle"
)

// Ready on a proxy is the fleet-level roll-up: True when every matching cluster's release is
// Deployed or Completed. A proxy-level refusal - an unresolved package, a dependency cycle -
// surfaces its own reason here instead, because that is the blocker worth naming when nothing
// can deploy anywhere. The condition type itself is declared with ZarfPackageRelease, where the
// same name means the single cluster that release is for.
const (
	// ReasonConverged is Ready: True - every matching cluster is deployed.
	ReasonConverged = "Converged"

	// ReasonNoMatchingClusters is Ready: Unknown - the selector matches nothing, so the proxy
	// has not converged and has not been asked to do anything either. Unknown rather than True
	// so a label typo does not read as success.
	ReasonNoMatchingClusters = "NoMatchingClusters"

	// ReasonAwaitingReleases is Ready: False with no release yet stamped to borrow a reason
	// from - the window between matching a cluster and stamping its release.
	ReasonAwaitingReleases = "AwaitingReleases"

	// ReasonSuperseded is Ready: True on an ApplyOnce proxy whose clusters are all deployed but
	// some were seeded with a version the spec no longer declares. True because every cluster
	// has the package and a seed is never re-run; a reason of its own because "converged" would
	// otherwise be the only thing an operator sees on a fleet that is a version behind.
	ReasonSuperseded = "Superseded"
)

// ConditionPackageResolved is the proxy condition for §6 step 4's digest resolution and
// signature verification: True once the tag is pinned to status.resolvedDigest (and verified,
// when a key is named), False when resolution or verification refused.
const ConditionPackageResolved = "PackageResolved"

const (
	// ReasonResolved is PackageResolved: True.
	ReasonResolved = "Resolved"
	// ReasonResolutionFailed is PackageResolved: False - the registry read failed; the message
	// carries the error.
	ReasonResolutionFailed = "ResolutionFailed"
	// ReasonSignatureVerificationFailed is PackageResolved: False - the package is unsigned,
	// the signature does not verify against the named key, or it is in a form that cannot be
	// checked. Refusal happens before any deploy Job exists (the Signature Trust Model).
	ReasonSignatureVerificationFailed = "SignatureVerificationFailed"
	// ReasonPackageAPIMismatch is PackageResolved: False - spec.package.api declares a schema
	// version the pulled artifact does not carry (§4C: the declaration is a claim the
	// controller verifies).
	ReasonPackageAPIMismatch = "PackageAPIMismatch"
)

// ConditionInitPackageResolved is the proxy condition for resolving spec.init.package - §6 step
// 5's auto-init deploy path. It shares PackageResolved's reasons, plus NotAnInitPackage.
const ConditionInitPackageResolved = "InitPackageResolved"

// ReasonNotAnInitPackage is InitPackageResolved: False - spec.init.package resolved to an
// artifact that is not a ZarfInitConfig, so deploying it could never initialize the cluster.
const ReasonNotAnInitPackage = "NotAnInitPackage"

// ConditionTagDrift is the abnormal-true warning condition for §3's tag re-check: True with
// reason TagContentChanged when the registry's tag no longer matches the pinned digest. Pinned
// deploys continue untouched; the update path is a spec.package.version bump.
const ConditionTagDrift = "TagDrift"

// ReasonTagContentChanged is TagDrift: True; the message names both digests.
const ReasonTagContentChanged = "TagContentChanged"

// ConditionPackageSignatureVerified is True once the Cosign signature verified against the
// pinned digest. Absent - not True - when no key is named, because a signature with no key to
// check it against is not verification (the Signature Trust Model).
const ConditionPackageSignatureVerified = "PackageSignatureVerified"

// ReasonSignatureVerified is PackageSignatureVerified: True.
const ReasonSignatureVerified = "SignatureVerified"

const (
	// AnnotationFollowTag opts a proxy into re-resolving its tag on a timer ("true"), for
	// connected test environments - §3's tag-following. Deliberately an annotation, not spec,
	// to keep dev toggles out of GitOps-owned state.
	AnnotationFollowTag = "addons.colonel-byte.dev/follow-tag"
	// AnnotationFollowTagInterval is the poll interval as a Go duration. Default 15m;
	// meaningless without follow-tag; invalid values fall back with a warning event.
	AnnotationFollowTagInterval = "addons.colonel-byte.dev/follow-tag-interval"
)

// NamespacedReference names an object, in the referring object's namespace by default.
type NamespacedReference struct {
	// name of the referenced object.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name,omitempty"`

	// namespace of the referenced object. Empty means the namespace of the object holding this
	// reference. A namespace other than that one is refused unless the controller was started
	// with a flag naming it (--secret-namespaces for secret references, --cluster-namespaces
	// for the proxy references cross-namespace selection creates), surfaced as a condition with
	// reason CrossNamespaceNotEnabled. See docs/dev/architecture.md §2C.
	// +optional
	// +kubebuilder:validation:MaxLength=63
	Namespace string `json:"namespace,omitempty"`
}

// Key is the object this reference names, as seen from an object in ownerNamespace.
//
// allowed is the gating flag's namespace list; "*" in it permits any namespace. flag names the
// gating flag for the error text. A cross-namespace reference to a namespace not in the list is
// an error rather than a fallback to ownerNamespace, because silently resolving a different
// object than the one named is worse than refusing; callers surface it as a condition with
// reason CrossNamespaceNotEnabled.
func (r NamespacedReference) Key(ownerNamespace string, allowed []string, flag string) (types.NamespacedName, error) {
	if r.Namespace == "" || r.Namespace == ownerNamespace {
		return types.NamespacedName{Namespace: ownerNamespace, Name: r.Name}, nil
	}
	for _, ns := range allowed {
		if ns == "*" || ns == r.Namespace {
			return types.NamespacedName{Namespace: r.Namespace, Name: r.Name}, nil
		}
	}
	return types.NamespacedName{}, fmt.Errorf(
		"%s/%s is in another namespace than %s, which this controller was not started with %s for",
		r.Namespace, r.Name, ownerNamespace, flag)
}

// ZarfPackageAPI is a Zarf package schema version.
// +kubebuilder:validation:Enum=v1alpha1;v1beta1
type ZarfPackageAPI string

const (
	// PackageAPIV1Alpha1 is the legacy Zarf package schema. Support is dropped when upstream
	// Zarf drops v1alpha1 support.
	PackageAPIV1Alpha1 ZarfPackageAPI = "v1alpha1"
	// PackageAPIV1Beta1 is the current Zarf package schema.
	PackageAPIV1Beta1 ZarfPackageAPI = "v1beta1"
)

// ZarfPackageSource says where a package comes from, how it is reached, and how it is verified.
//
// Each rule below guards an optional boolean with has() before reading it. A boolean that was
// never set is absent from the stored object rather than false, and reading an absent key is an
// evaluation error that rejects the whole resource - so `!self.fromZarfState || ...` would refuse
// every proxy that never mentioned the field. The sister repository learned this from an envtest
// suite and nothing else would have caught it.
// +kubebuilder:validation:XValidation:rule="!has(self.caBundleSecretRef) || !has(self.caBundleConfigMapRef)",message="set caBundleSecretRef or caBundleConfigMapRef, not both"
// +kubebuilder:validation:XValidation:rule="!has(self.fromZarfState) || !self.fromZarfState || (!has(self.pullSecretRef) && !has(self.caBundleSecretRef) && !has(self.caBundleConfigMapRef))",message="fromZarfState supplies the credentials and the CA, so pullSecretRef, caBundleSecretRef and caBundleConfigMapRef must be unset"
type ZarfPackageSource struct {
	// repository is the OCI repository holding the package, without a tag.
	// +required
	Repository string `json:"repository"`

	// api asserts the package's Zarf schema version. When unset, the controller detects it
	// from the pulled artifact; when set, a mismatch with the artifact fails the release
	// before any deploy runs. Declaring v1alpha1 is required to use spec.set.
	// +optional
	API ZarfPackageAPI `json:"api,omitempty"`

	// version is the package version (OCI tag) to deploy.
	// +required
	Version string `json:"version"`

	// signatureKeySecretRef names a Secret holding the Cosign public key ("cosign.pub" key)
	// the package signature is verified against before extraction. When unset, signature
	// verification is skipped. In the proxy's namespace unless its namespace field names one
	// the controller's --secret-namespaces flag permits.
	//
	// Unlike the authentication fields below, this stands alongside fromZarfState rather than
	// instead of it: Zarf's own state carries no signing key, so a package pulled from a Zarf
	// registry is verified through this field like any other.
	// +optional
	SignatureKeySecretRef *NamespacedReference `json:"signatureKeySecretRef,omitempty"`

	// pullSecretRef names a kubernetes.io/dockerconfigjson Secret holding credentials for the
	// registry this package is pulled from - what `kubectl create secret docker-registry`
	// produces. Empty means anonymous access, which is what every registry had to allow before
	// this field existed.
	//
	// The credentials are needed in two places and reach them differently. The controller
	// resolves and verifies the package itself, and passes them per-call, so two proxies
	// pulling from two registries with two different credentials never see each other's. The
	// deploy pod pulls the package by digest, and there they arrive as a mounted Docker config
	// file the pod's own environment points at - the pod holds no management-cluster access and
	// cannot look a Secret up, and one pod serves exactly one package, so a file scoped to the
	// pod is scoped to the pull. See docs/agent/choice-registry-credentials.md.
	// +optional
	PullSecretRef *NamespacedReference `json:"pullSecretRef,omitempty"`

	// caBundleSecretRef names a Secret holding PEM-encoded certificates to trust when reaching
	// this registry, in its "ca.crt" key. They are trusted *in addition to* the system roots,
	// so naming a private CA does not stop the controller reaching anything else.
	//
	// This is the field for a registry serving a certificate nothing public signed, and a
	// Secret is the shape cert-manager already writes one into. Prefer it to
	// insecureSkipTLSVerify, which verifies nothing at all.
	// +optional
	CABundleSecretRef *NamespacedReference `json:"caBundleSecretRef,omitempty"`

	// caBundleConfigMapRef is caBundleSecretRef from a ConfigMap instead, same "ca.crt" key.
	// A certificate authority is not secret, so a ConfigMap is the honest place for one; the
	// Secret form exists because that is where cert-manager puts the CA it generates. Set one
	// or the other, never both.
	// +optional
	CABundleConfigMapRef *NamespacedReference `json:"caBundleConfigMapRef,omitempty"`

	// fromZarfState reads this registry's credentials and certificate authority out of the Zarf
	// deployment in the *management* cluster, instead of from pullSecretRef and caBundleSecretRef.
	//
	// This is the air-gapped case with nothing to wire up: where the registry the fleet pulls
	// packages from is a Zarf registry in the management cluster, Zarf generated its credentials
	// and signed its certificate itself, and both are already present as the `zarf-state` and
	// `zarf-registry-server-tls` secrets. The pull-only credentials are preferred over the push
	// pair. It replaces the manual step of running `zarf tools get-creds`, creating a
	// docker-registry Secret whose server had to match the repository host exactly, and
	// extracting the CA by hand.
	//
	// The repository is still named in this spec rather than read from that state, and that is
	// the part worth knowing: Zarf records the address a *node* reaches the registry at - a
	// NodePort, or an external host - and a controller inside the cluster wants the service,
	// which the state does not carry.
	//
	// No client certificate is presented, and that is measured rather than assumed: Zarf's
	// registry chart sets `verify-client-cert-if-given`, so a client offering none is still
	// served, and the certificate Zarf issues belongs to the per-node registry proxy rather than
	// to anything a management-cluster controller runs.
	// +optional
	FromZarfState bool `json:"fromZarfState,omitempty"`

	// insecureSkipTLSVerify accepts this registry's certificate without verifying it, rather
	// than trusting it through caBundleSecretRef. It covers reaching the package registry only,
	// and neither the management cluster's API nor any workload cluster's.
	//
	// Per-package on purpose rather than a manager flag: one registry nobody can issue a
	// certificate for must not lower the bar for every other package in the fleet.
	// +optional
	InsecureSkipTLSVerify bool `json:"insecureSkipTLSVerify,omitempty"`
}

// InitRegistryMode is how workload pods reach the registry zarf-state names - upstream's
// state.RegistryMode. Upstream also knows "external", which needs the address and credential
// fields a later change adds, so it is not accepted here yet.
// +kubebuilder:validation:Enum=nodeport;proxy
type InitRegistryMode string

const (
	// RegistryModeNodePort serves the in-cluster registry through a NodePort Service. Zarf's
	// default. Forces the registry's mTLS strategy to none upstream, so the internal registry
	// cannot serve TLS in this mode.
	RegistryModeNodePort InitRegistryMode = "nodeport"

	// RegistryModeProxy serves the in-cluster registry through a per-node DaemonSet proxy on a
	// host port, with zarf-managed mTLS - the mode where the internal registry serves TLS.
	// Requires init.injectorImage: an image the node already holds, because on first init
	// nothing can pull one.
	RegistryModeProxy InitRegistryMode = "proxy"
)

// InitRegistryOptions shapes the registry half of the zarf-state that initialization creates,
// mirroring upstream's RegistryInfo. These settings ride to every ZarfInitConfig deploy this
// proxy performs, and re-deploys re-assert them - upstream only applies a mode that is
// explicitly set, so a cluster can never silently regress to upstream defaults.
type InitRegistryOptions struct {
	// mode is how the registry is served: nodeport (Zarf's default) or proxy. Immutable once
	// set, including unsetting it - switching a live cluster's mode re-addresses the registry
	// under every release that templated the old address, so this provider refuses to drive
	// one. A cluster whose existing zarf-state disagrees holds with condition
	// InitSettingsSuperseded instead of being converged onto the declared mode.
	// +optional
	Mode InitRegistryMode `json:"mode,omitempty"`

	// port is the registry's port: a Kubernetes NodePort in nodeport mode, a host port in
	// proxy mode. Zero takes upstream's per-mode default.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	Port int32 `json:"port,omitempty"`
}

// InitOptions controls Zarf initialization of a target cluster that has no Zarf state yet -
// §6 step 5's auto-init deploy path (docs/dev/architecture.md).
// +kubebuilder:validation:XValidation:rule="!(has(self.autoInit) && self.autoInit) || has(self.package)",message="init.autoInit requires init.package"
// +kubebuilder:validation:XValidation:rule="!has(self.set) || (has(self.package) && has(self.package.api) && self.package.api == 'v1alpha1')",message="init.set requires init.package.api: v1alpha1; v1beta1 init packages take init.values"
// +kubebuilder:validation:XValidation:rule="!has(self.registry) || !has(self.registry.mode) || self.registry.mode != 'proxy' || has(self.injectorImage)",message="init.registry.mode: proxy requires init.injectorImage - first init has no registry to pull an injector from"
type InitOptions struct {
	// autoInit deploys init.package first when the target cluster has no zarf-state secret in
	// the zarf namespace. Keyed on observed cluster state, never a name, so it commutes with a
	// fleet that manages init through its own proxy: whoever initialized first, the probe finds
	// the state and auto-init no-ops.
	// +optional
	AutoInit bool `json:"autoInit,omitempty"`

	// package is the init package to deploy - resolved, digest-pinned, and signature-verified
	// exactly like spec.package, and refused unless the artifact resolves to kind
	// ZarfInitConfig. Required when autoInit is true.
	// +optional
	Package *ZarfPackageSource `json:"package,omitempty"`

	// components filters which of the init package's components deploy, with spec.components'
	// semantics: empty means the package's defaults. Init packages commonly mark every
	// component optional so an operator can bring a cluster up in stages, and a package built
	// that way deploys nothing at all unless its components are named here.
	// +optional
	Components []ComponentSelector `json:"components,omitempty"`

	// values is the init deploy's structured values tree, with spec.values' semantics and
	// templating. The registry storage class lives here on v1beta1 init packages
	// (registry.persistence.storageClass).
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Values *apiextensionsv1.JSON `json:"values,omitempty"`

	// set are the init deploy's Zarf variables, with spec.set's semantics, templating, and
	// v1alpha1-only rule: using it requires init.package.api: v1alpha1. The init package
	// upstream publishes is v1alpha1 and takes its registry storage class as the
	// REGISTRY_STORAGE_CLASS variable.
	// +optional
	Set map[string]string `json:"set,omitempty"`

	// registry shapes the registry half of the zarf-state initialization creates. The settings
	// apply to every ZarfInitConfig this proxy deploys - the auto-init package here, or a
	// spec.package that itself resolves to one - so an init block carrying only settings, with
	// no package or autoInit, is valid for proxies that init through spec.package.
	// +optional
	Registry *InitRegistryOptions `json:"registry,omitempty"`

	// injectorImage is the container image Zarf's injector runs as during initialization.
	// Proxy mode requires it on first init, when no registry exists to pull from, so it must
	// be an image the node already holds - a per-fleet value, typically something the node
	// image ships. A wrong value fails at init with an image-pull symptom, not a config error.
	// +optional
	InjectorImage string `json:"injectorImage,omitempty"`
}

// ComponentSelector names a package component to deploy, optionally pinning its flavor.
type ComponentSelector struct {
	// name is the component's name in the package definition.
	// +required
	Name string `json:"name"`

	// flavor selects a component flavor, overriding auto-detection.
	// +optional
	Flavor string `json:"flavor,omitempty"`
}

// DependencyKind names the kind a dependsOn entry points at. A typed reference rather than a
// bare name because ordered bundles (#379) will be a second kind a package can wait for, and a
// bare string cannot say which of two kinds a name means - taken while the API is v1alpha1 and
// the break is cheap.
type DependencyKind string

const (
	// DependencyKindPackageProxy is a ZarfPackageProxy. The only kind today, and the default,
	// so an entry naming nothing else stays a single line.
	DependencyKindPackageProxy DependencyKind = "ZarfPackageProxy"
)

// DependencyReference names one object this package waits for. Always in the referring object's
// own namespace: unlike the secret references, a dependency carries no namespace field, because
// a dependency graph spanning namespaces is one no single author can read.
type DependencyReference struct {
	// kind of the referenced object. Empty means ZarfPackageProxy.
	// +optional
	// +kubebuilder:default=ZarfPackageProxy
	// +kubebuilder:validation:Enum=ZarfPackageProxy
	Kind DependencyKind `json:"kind,omitempty"`

	// name of the referenced object, in this object's namespace.
	// +required
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// ProxyKind reports whether the entry refers to a ZarfPackageProxy, which an empty kind does.
func (d DependencyReference) ProxyKind() bool {
	return d.Kind == "" || d.Kind == DependencyKindPackageProxy
}

// DeployJobOptions customizes the Kubernetes Job the controller runs each deploy in. Every
// field is optional; the defaults are a restricted pod security profile and an emptyDir cache
// volume. Supply a volume named "zarf-cache" to replace the default cache emptyDir - the quirk
// in AGENTS.md on package sizes is the reason to.
type DeployJobOptions struct {
	// image overrides the deploy pod's image. Defaults to the controller's --deploy-image.
	// +optional
	Image string `json:"image,omitempty"`

	// serviceAccountName is the service account the deploy pod runs as.
	// +optional
	ServiceAccountName string `json:"serviceAccountName,omitempty"`

	// podSecurityContext overrides the pod-level security context (default: runAsNonRoot,
	// RuntimeDefault seccomp).
	// +optional
	PodSecurityContext *corev1.PodSecurityContext `json:"podSecurityContext,omitempty"`

	// securityContext overrides the deploy container's security context (default: no privilege
	// escalation, all capabilities dropped, read-only root filesystem).
	// +optional
	SecurityContext *corev1.SecurityContext `json:"securityContext,omitempty"`

	// volumes are added to the deploy pod. A volume named "zarf-cache" replaces the default
	// emptyDir cache volume and is mounted at the cache path.
	// +optional
	Volumes []corev1.Volume `json:"volumes,omitempty"`

	// volumeMounts are added to the deploy container, alongside the cache and kubeconfig mounts.
	// +optional
	VolumeMounts []corev1.VolumeMount `json:"volumeMounts,omitempty"`

	// nodeSelector constrains which nodes the deploy pod schedules to - for example, nodes with
	// disks sized for the package cache.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// tolerations are added to the deploy pod.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`
}

// ZarfPackageProxySpec declares a package's desired state across every matching cluster.
//
// The registry-mode transition rule lives at spec level rather than on the registry struct so
// it also refuses *clearing*: dropping spec.init (or its registry block) in one edit and
// re-adding it with a different mode in the next would bypass a rule that can only see the
// struct it sits on.
// +kubebuilder:validation:XValidation:rule="!has(self.set) || (has(self.package.api) && self.package.api == 'v1alpha1')",message="spec.set requires spec.package.api: v1alpha1; v1beta1 packages take spec.values"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.init) || !has(oldSelf.init.registry) || !has(oldSelf.init.registry.mode) || (has(self.init) && has(self.init.registry) && has(self.init.registry.mode) && self.init.registry.mode == oldSelf.init.registry.mode)",message="init.registry.mode is immutable once set, including unsetting it - switching a live cluster's registry mode re-addresses the registry under deployed releases"
type ZarfPackageProxySpec struct {
	// clusterSelector selects the CAPI Clusters this package deploys to, by label. Only
	// Clusters in the proxy's own namespace match unless the controller was started with
	// --cluster-namespaces naming (or wildcarding) others.
	// +required
	ClusterSelector metav1.LabelSelector `json:"clusterSelector"`

	// package is the package source and verification settings.
	// +required
	Package ZarfPackageSource `json:"package"`

	// strategy is Reconcile (continuous lifecycle, the default) or ApplyOnce (seed packages).
	// +optional
	// +kubebuilder:default=Reconcile
	Strategy DeploymentStrategy `json:"strategy,omitempty"`

	// dependsOn names other objects in the same namespace whose releases must be Deployed (or
	// Completed, for ApplyOnce) on a cluster before this package deploys there.
	// +optional
	DependsOn []DependencyReference `json:"dependsOn,omitempty"`

	// init controls Zarf auto-initialization of uninitialized target clusters.
	// +optional
	Init *InitOptions `json:"init,omitempty"`

	// components filters which package components deploy. Empty means the package's defaults.
	// +optional
	Components []ComponentSelector `json:"components,omitempty"`

	// values is a structured values tree merged over the package's defaults (zarf.dev/v1beta1
	// structured values). String leaves support Go templates over the target Cluster, for
	// example "{{ .Cluster.metadata.name }}".
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Values *apiextensionsv1.JSON `json:"values,omitempty"`

	// set are Zarf deploy-time variables, supported only by v1alpha1 packages: using it
	// requires spec.package.api: v1alpha1, enforced at admission. Values support the same Go
	// templating as values. Deprecated on the same schedule as v1alpha1 package support.
	// +optional
	Set map[string]string `json:"set,omitempty"`

	// deployJob customizes the Kubernetes Job each deploy runs in.
	// +optional
	DeployJob *DeployJobOptions `json:"deployJob,omitempty"`

	// deletionPolicy says whether deleting a release uninstalls its package from the workload
	// cluster (Delete, the default) or orphans it there (Retain). ApplyOnce proxies always
	// behave as Retain - a seed cannot be un-run. A Reconcile proxy managing a package an
	// ApplyOnce proxy seeded must be Retain, or deleting it removes the deployed-package
	// record the seed guarantee rests on (docs/dev/architecture.md §8A).
	// +optional
	// +kubebuilder:default=Delete
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`
}

// ZarfPackageProxyStatus is the observed state across matching clusters.
type ZarfPackageProxyStatus struct {
	// conditions summarize the proxy's state. Ready is true when every matching cluster's
	// release is Deployed or Completed.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// matchingClusters are the names of the Clusters the selector currently matches.
	// +optional
	MatchingClusters []string `json:"matchingClusters,omitempty"`

	// clusterCount is how many Clusters the selector matches - the length of
	// matchingClusters, carried as a number because a printer column cannot take the length
	// of a list and `kubectl get` is where an operator looks first.
	// +optional
	ClusterCount int32 `json:"clusterCount,omitempty"`

	// readyClusterCount is how many of those Clusters carry a release in Deployed or
	// Completed. Beside Ready rather than instead of it: one stuck cluster out of two hundred
	// and a fleet that deployed nowhere are both Ready: False, and only the counts tell them
	// apart.
	// +optional
	ReadyClusterCount int32 `json:"readyClusterCount,omitempty"`

	// supersededClusterCount is how many Clusters were seeded with a version the spec no longer
	// declares. Only ApplyOnce proxies can hold this state, and holding it is the contract
	// rather than a fault - a seed is never re-run - so it does not make the proxy un-Ready. It
	// is counted because a fleet reporting converged while every cluster is a version behind is
	// otherwise indistinguishable from one that is actually up to date.
	// +optional
	SupersededClusterCount int32 `json:"supersededClusterCount,omitempty"`

	// resolvedDigest is the package manifest digest the declared tag resolved to, pinned on
	// first resolution. Every pull, verification, and deploy uses this digest, never the tag,
	// so a re-pushed tag cannot change what the fleet runs. See docs/dev/architecture.md §3.
	// +optional
	ResolvedDigest string `json:"resolvedDigest,omitempty"`

	// resolvedRef is the "repository:version" whose resolution resolvedDigest pins. The
	// controller re-resolves only when the spec's reference no longer matches it - an edit to
	// values or components must not silently re-pin a moved tag.
	// +optional
	ResolvedRef string `json:"resolvedRef,omitempty"`

	// packageKind is the resolved artifact's kind - ZarfPackageConfig, or ZarfInitConfig,
	// which is what §6 step 5's init-first eligibility gate reads.
	// +optional
	PackageKind string `json:"packageKind,omitempty"`

	// initResolvedDigest is the manifest digest spec.init.package pinned to, with
	// resolvedDigest's pin-once semantics. Every init Job deploys this digest, never the tag.
	// +optional
	InitResolvedDigest string `json:"initResolvedDigest,omitempty"`

	// initResolvedRef is the "repository:version" whose resolution initResolvedDigest pins.
	// +optional
	InitResolvedRef string `json:"initResolvedRef,omitempty"`

	// packageName is the resolved package's declared name - the identity zarf's
	// deployed-package record on each workload cluster is keyed on (§3), which is what the
	// ApplyOnce probe-and-adopt looks up.
	// +optional
	PackageName string `json:"packageName,omitempty"`

	// observedGeneration is the generation the status reflects.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=zarfpackageproxies,scope=Namespaced,categories=cluster-api,shortName=zpp
// +kubebuilder:storageversion
// The default width answers "is this proxy fine, how broadly, and why not" - Ready, the two
// counts that tell one stuck cluster from a dead fleet, and the reason behind Ready. Strategy
// and the PackageResolved pair move to -o wide: they are rarely what an operator is scanning
// for, and the roll-up already surfaces a resolution failure as its own reason.
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].status",description="Every matching cluster is deployed"
// +kubebuilder:printcolumn:name="Deployed",type="integer",JSONPath=".status.readyClusterCount",description="Matching clusters with the package deployed"
// +kubebuilder:printcolumn:name="Clusters",type="integer",JSONPath=".status.clusterCount",description="Clusters the selector matches"
// +kubebuilder:printcolumn:name="Status",type="string",JSONPath=".status.conditions[?(@.type==\"Ready\")].reason",description="Why Ready holds its status"
// +kubebuilder:printcolumn:name="Superseded",type="integer",priority=1,JSONPath=".status.supersededClusterCount",description="Clusters seeded with a version the spec no longer declares"
// +kubebuilder:printcolumn:name="Version",type="string",JSONPath=".spec.package.version",description="Declared package version"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time since creation"
// +kubebuilder:printcolumn:name="Strategy",type="string",priority=1,JSONPath=".spec.strategy",description="Reconcile or ApplyOnce"
// +kubebuilder:printcolumn:name="Resolved",type="string",priority=1,JSONPath=".status.conditions[?(@.type==\"PackageResolved\")].status",description="Package digest resolved and verified"
// +kubebuilder:printcolumn:name="Resolution",type="string",priority=1,JSONPath=".status.conditions[?(@.type==\"PackageResolved\")].reason",description="Why PackageResolved holds its status"

// ZarfPackageProxy declares a Zarf package's desired state across every CAPI Cluster matching its
// selector. The controller stamps one ZarfPackageRelease per matching cluster and deploys through
// it; the proxy itself never touches a workload cluster.
type ZarfPackageProxy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the desired state of ZarfPackageProxy.
	// +optional
	Spec ZarfPackageProxySpec `json:"spec,omitempty,omitzero"`

	// status is the observed state of ZarfPackageProxy.
	// +optional
	Status ZarfPackageProxyStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// ZarfPackageProxyList is a list of ZarfPackageProxy.
type ZarfPackageProxyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ZarfPackageProxy `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &ZarfPackageProxy{}, &ZarfPackageProxyList{})
}
