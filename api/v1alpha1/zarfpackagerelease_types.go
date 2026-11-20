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
// +kubebuilder:validation:Enum=Pending;Deploying;Deployed;Completed;Failed
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
)

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

	// applied is true once an ApplyOnce release has executed; it is never reset.
	// +optional
	Applied bool `json:"applied,omitempty"`

	// conditions detail the release's state: Ready, PackageSignatureVerified,
	// DependenciesSatisfied.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=zarfpackagereleases,scope=Namespaced,categories=cluster-api,shortName=zpr
// +kubebuilder:storageversion
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".spec.clusterRef.name",description="Target cluster"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase",description="Release phase"
// +kubebuilder:printcolumn:name="Version",type="string",JSONPath=".status.deployedVersion",description="Deployed package version"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp",description="Time since creation"

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
