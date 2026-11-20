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

// Package v1alpha1 contains the CAAPZ API types: ZarfPackageProxy, the fleet-wide declaration of a
// Zarf package, and ZarfPackageRelease, the controller-owned record of one cluster's deployment of
// it. The group is addons.colonel-byte.dev, a colonel-byte-owned group - addons.cluster.x-k8s.io is community-protected and CAAPH-owned.
//
// Nothing here imports controller-runtime, which is deliberate - the scheme is built from
// apimachinery's own SchemeBuilder so that anything reading these types (a test, another
// provider, a CLI) does not pay for a controller runtime it never starts. This mirrors the
// cluster-api-provider-cargoship API packages.
// +kubebuilder:object:generate=true
// +groupName=addons.colonel-byte.dev
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is the group and version the types in this package belong to.
	GroupVersion = schema.GroupVersion{Group: "addons.colonel-byte.dev", Version: "v1alpha1"}

	// schemeBuilder is used to add go types to the GroupVersionKind scheme.
	schemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme registers this package's types into a runtime.Scheme.
	AddToScheme = schemeBuilder.AddToScheme

	// objectTypes is appended to by each types file's init, so a new kind registers itself
	// beside its own declaration rather than in a list here that is easy to forget.
	objectTypes = []runtime.Object{}
)

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion, objectTypes...)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
