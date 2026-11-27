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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ReleasePhase is where a release is in its lifecycle.
// +kubebuilder:validation:Enum=Pending;Deploying;Deployed;Completed;Failed;Removing
type ReleasePhase string

const (
	// PhasePending means the release is waiting - on dependencies, on the cluster's control
	// plane, or on its first reconcile.
	PhasePending ReleasePhase = "Pending"
	// PhaseDeploying means a deploy is in flight.
	PhaseDeploying ReleasePhase = "Deploying"
	// PhaseDeployed means the declared version is deployed; Reconcile releases return here
	// after every successful converge.
	PhaseDeployed ReleasePhase = "Deployed"
	// PhaseCompleted means an ApplyOnce release ran once and will never re-execute.
	PhaseCompleted ReleasePhase = "Completed"
	// PhaseFailed means the last deploy attempt failed; see the conditions for why.
	PhaseFailed ReleasePhase = "Failed"
	// PhaseRemoving means the release is deleted and its removal Job is uninstalling the
	// package from the workload cluster before the finalizer lets the object go (§8B).
	PhaseRemoving ReleasePhase = "Removing"
)

// ConditionDependenciesSatisfied is the release condition carrying the per-cluster dependsOn
// verdict (docs/dev/architecture.md §5B). The rule that prevents deadlock: a dependency that
// cannot be satisfied on this cluster is a named error reason, never a silent wait.
const ConditionDependenciesSatisfied = "DependenciesSatisfied"

const (
	// ReasonDependenciesSatisfied is DependenciesSatisfied: True - every dependsOn entry has a
	// release in phase Deployed or Completed on this cluster.
	ReasonDependenciesSatisfied = "Satisfied"
	// ReasonWaitingForDependency means the dependency's proxy exists and matches this cluster,
	// and its release is pending or absent - the one case where progress is actually coming.
	ReasonWaitingForDependency = "WaitingForDependency"
	// ReasonDependencyNotApplicable means the dependency's proxy exists but its selector never
	// matches this cluster - a configuration contradiction, deliberately an error rather than
	// auto-satisfied so the label mistake is visible.
	ReasonDependencyNotApplicable = "DependencyNotApplicable"
	// ReasonDependencyNotFound means the dependency names no proxy in this namespace.
	ReasonDependencyNotFound = "DependencyNotFound"
)

// ConditionDeploySpecReady is the release condition for the rendered deploy spec - the
// per-release Secret the deploy Job mounts (§6 step 6). True once the Secret holds the current
// rendering; False when rendering refused, with the template error in the message.
const ConditionDeploySpecReady = "DeploySpecReady"

const (
	// ReasonDeploySpecWritten is DeploySpecReady: True.
	ReasonDeploySpecWritten = "DeploySpecWritten"
	// ReasonRenderFailed is DeploySpecReady: False - a value template did not parse or
	// referenced a field the target cluster does not carry.
	ReasonRenderFailed = "RenderFailed"
)

// ConditionReady is the release condition summarizing deployability (§6 step 7): True once the
// declared package is on the cluster, False with a reason naming what the release waits on.
const ConditionReady = "Ready"

const (
	// ReasonWaitingForZarfState holds a non-init release while its cluster has no zarf
	// state - §6 step 5's init-first eligibility gate. Only init-capable releases may take
	// the cluster's Job slot until the state exists.
	ReasonWaitingForZarfState = "WaitingForZarfState"
	// ReasonWaitingForActiveDeploy holds a ready release while another deploy or removal Job
	// runs on its cluster - §6 step 6's one-Job-per-cluster rule.
	ReasonWaitingForActiveDeploy = "WaitingForActiveDeploy"
	// ReasonDeploying means this release's deploy Job is running.
	ReasonDeploying = "Deploying"
	// ReasonInitializing means this release's init Job is deploying spec.init.package to a
	// cluster that had no zarf state - §6 step 5's auto-init deploy path.
	ReasonInitializing = "Initializing"
	// ReasonInitFailed is Ready: False - the init Job failed; the message carries the error the
	// pod reported. Retry is edit-to-retry via the init spec, the same stance as deploys.
	ReasonInitFailed = "InitFailed"
	// ReasonAdoptedExistingRecord means an ApplyOnce release found the package's
	// deployed-package record already on the cluster and completed without deploying (§3).
	ReasonAdoptedExistingRecord = "AdoptedExistingRecord"

	// ReasonAlreadySeeded is Ready: True on an ApplyOnce release that has already run here and
	// will not run again. It exists so that a proxy bumped after seeding says so: the release
	// reports Completed either way, and without this the only visible difference between "the
	// declared version is deployed" and "the declared version will never deploy here" is
	// comparing two fields by eye.
	ReasonAlreadySeeded = "AlreadySeeded"
	// ReasonStageListDiverged is Ready: False - the declared stage list structurally diverges
	// from this cluster's applied ApplyOnce history (a rename, a removal, or an insertion
	// before an applied seed), and running it could re-apply superseded seed values. The
	// message names the first divergence; Reconcile stages are free to change.
	ReasonStageListDiverged = "StageListDiverged"
	// ReasonWaitingForStageSpec holds a staged release while a stage's document is not yet
	// rendered - its package has not resolved; the proxy's PackageResolved condition says why.
	ReasonWaitingForStageSpec = "WaitingForStageSpec"
	// ReasonDeployed is Ready: True - the declared digest deployed successfully.
	ReasonDeployed = "Deployed"
	// ReasonCompleted is Ready: True for an ApplyOnce release whose one deploy ran.
	ReasonCompleted = "Completed"
	// ReasonDeployFailed is Ready: False - the deploy Job failed; the message carries the
	// error the pod reported.
	ReasonDeployFailed = "DeployFailed"
)

// ReasonRemoving is Ready: False while a deleted release's removal Job uninstalls its package.
const ReasonRemoving = "Removing"

// ConditionRemovalFailed is the abnormal-true condition set when a removal Job failed while the
// cluster was alive and reachable (§8C); the message carries the error the pod reported. The
// controller retries with backoff. The escape hatches, in preference order: flip the proxy to
// Retain (declarative, all its releases), or remove the finalizer by hand (surgical, one
// release). Absent otherwise.
const ConditionRemovalFailed = "RemovalFailed"

// ReasonRemovalJobFailed is RemovalFailed: True.
const ReasonRemovalJobFailed = "RemovalJobFailed"

// ConditionSeedRecorded is set False on an ApplyOnce release whose completed deploy left no
// deployed-package record on the cluster - a pure-actions package silently degrades the §3
// guarantee to this garbage-collectable release, and that deserves a name. Absent otherwise.
const ConditionSeedRecorded = "SeedRecorded"

// ReasonNoDeployedPackageRecord is SeedRecorded: False - zarf stores the record only for
// packages with at least one cluster-touching component.
const ReasonNoDeployedPackageRecord = "NoDeployedPackageRecord"

// AnnotationConcurrentDeploys opts a CAPI Cluster out of the one-Job-per-cluster serialization
// when set to "true" - on the Cluster, because concurrency tolerance is a property of the
// target, not of any one proxy. Specified as "true" so it can grow into a slot count later.
const AnnotationConcurrentDeploys = "addons.colonel-byte.dev/concurrent-deploys"

// ZarfPackageReleaseSpec binds one cluster to one proxy.
type ZarfPackageReleaseSpec struct {
	// clusterRef names the CAPI Cluster, in this namespace, the package deploys to.
	// +required
	ClusterRef corev1.LocalObjectReference `json:"clusterRef"`

	// packageProxyRef names the ZarfPackageProxy this release realizes. In this namespace
	// unless cross-namespace selection (--cluster-namespaces) created this release from a
	// proxy in another one.
	// +required
	PackageProxyRef NamespacedReference `json:"packageProxyRef"`
}

// ZarfPackageReleaseStatus is the observed state of one cluster's deployment.
type ZarfPackageReleaseStatus struct {
	// phase is where the release is in its lifecycle.
	// +optional
	Phase ReleasePhase `json:"phase,omitempty"`

	// deployedVersion is the package version last deployed successfully.
	// +optional
	DeployedVersion string `json:"deployedVersion,omitempty"`

	// deployedDigest is the package manifest digest the last successful deploy ran from -
	// recorded per release so a fleet mid-rollout is legible digest by digest (§3).
	// +optional
	DeployedDigest string `json:"deployedDigest,omitempty"`

	// applied is true once an ApplyOnce release has executed; it is never reset.
	// +optional
	Applied bool `json:"applied,omitempty"`

	// stageCount is how many stages the proxy declared, stamped onto the release so it stays
	// self-describing after a clusterctl move separates it from its proxy. Zero on the
	// single-package form.
	// +optional
	StageCount int32 `json:"stageCount,omitempty"`

	// appliedStageCount is how many declared stages have settled on this cluster - carried as
	// a number because a printer column cannot take the length of a list.
	// +optional
	AppliedStageCount int32 `json:"appliedStageCount,omitempty"`

	// stages are the per-stage applied records of a staged proxy's release, one entry per
	// stage that has run on this cluster, in applied order. The identity each record carries
	// is the controller's own hash of the stage's rendered spec - never the pod-reported
	// digest, which a lost termination message leaves empty (docs/dev/design-package-stages.md,
	// "Per-stage state, not a cursor").
	// +optional
	// +listType=map
	// +listMapKey=name
	Stages []StageState `json:"stages,omitempty"`

	// conditions detail the release's state: Ready, PackageSignatureVerified,
	// DependenciesSatisfied.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// StageState is one stage's applied record on one cluster: what ran, identified by the spec
// hash it ran from, and when.
type StageState struct {
	// name is the declared stage name this record belongs to.
	// +required
	Name string `json:"name"`

	// specHash is the controller's hash of the rendered stage document the applied run was
	// stamped from - the identity a changed stage is detected by.
	// +optional
	SpecHash string `json:"specHash,omitempty"`

	// strategy is the stage's strategy as declared when it applied, so history stays
	// self-describing after the stage leaves the spec - the structural-divergence check needs
	// to know whether an undeclared record was ApplyOnce without the list that said so.
	// +optional
	Strategy DeploymentStrategy `json:"strategy,omitempty"`

	// appliedAt is when the stage's deploy Job completed.
	// +optional
	AppliedAt metav1.Time `json:"appliedAt,omitempty"`

	// deployedDigest is the digest the stage's pod reported, for display; possibly empty when
	// the termination message was lost, which is why it is never the applied identity.
	// +optional
	DeployedDigest string `json:"deployedDigest,omitempty"`

	// deployedVersion is the package version the stage's pod reported.
	// +optional
	DeployedVersion string `json:"deployedVersion,omitempty"`

	// undeclared is true when the spec no longer declares a stage of this name. The record is
	// kept rather than pruned - it is the evidence an ApplyOnce decision rests on.
	// +optional
	Undeclared bool `json:"undeclared,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=zarfpackagereleases,scope=Namespaced,categories=cluster-api,shortName=zpr
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".spec.clusterRef.name",description="Target cluster"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase",description="Release phase"
// +kubebuilder:printcolumn:name="Version",type="string",JSONPath=".status.deployedVersion",description="Deployed package version"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time since creation"
// +kubebuilder:printcolumn:name="Reason",type="string",priority=1,JSONPath=".status.conditions[?(@.type==\"Ready\")].reason",description="Why Ready holds its status"
// +kubebuilder:printcolumn:name="Applied",type="integer",priority=1,JSONPath=".status.appliedStageCount",description="Declared stages settled on this cluster"
// +kubebuilder:printcolumn:name="Stages",type="integer",priority=1,JSONPath=".status.stageCount",description="Stages the proxy declares"

// ZarfPackageRelease records one cluster's deployment of one ZarfPackageProxy. The controller
// creates it with an ownerReference to the CAPI Cluster, so deleting the Cluster garbage-collects
// the release - deliberately, so nothing CAAPZ owns can block cluster deletion.
type ZarfPackageRelease struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// spec is the desired state of ZarfPackageRelease.
	// +optional
	Spec ZarfPackageReleaseSpec `json:"spec,omitempty,omitzero"`

	// status is the observed state of ZarfPackageRelease.
	// +optional
	Status ZarfPackageReleaseStatus `json:"status,omitempty,omitzero"`
}

// +kubebuilder:object:root=true

// ZarfPackageReleaseList is a list of ZarfPackageRelease.
type ZarfPackageReleaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ZarfPackageRelease `json:"items"`
}

func init() {
	objectTypes = append(objectTypes, &ZarfPackageRelease{}, &ZarfPackageReleaseList{})
}
