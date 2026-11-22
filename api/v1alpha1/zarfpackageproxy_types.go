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

// ZarfPackageSource says where a package comes from and how it is verified.
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
	// +optional
	SignatureKeySecretRef *NamespacedReference `json:"signatureKeySecretRef,omitempty"`
}

// InitOptions controls Zarf initialization of a target cluster that has no Zarf state yet.
type InitOptions struct {
	// autoInit deploys the Zarf init package first when the target cluster has no zarf-state
	// secret in the zarf namespace.
	// +optional
	AutoInit bool `json:"autoInit,omitempty"`

	// storageClass is the storage class the init package uses for Zarf's internal registry.
	// +optional
	StorageClass string `json:"storageClass,omitempty"`
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
// +kubebuilder:validation:XValidation:rule="!has(self.set) || (has(self.package.api) && self.package.api == 'v1alpha1')",message="spec.set requires spec.package.api: v1alpha1; v1beta1 packages take spec.values"
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

	// dependsOn names other ZarfPackageProxies in the same namespace whose releases must be
	// Deployed (or Completed, for ApplyOnce) on a cluster before this package deploys there.
	// +optional
	DependsOn []string `json:"dependsOn,omitempty"`

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
// +kubebuilder:printcolumn:name="Strategy",type="string",JSONPath=".spec.strategy",description="Reconcile or ApplyOnce"
// +kubebuilder:printcolumn:name="Version",type="string",JSONPath=".spec.package.version",description="Declared package version"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time since creation"

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
