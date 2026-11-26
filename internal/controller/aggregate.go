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

package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
)

// aggregateReady sets the proxy's Ready condition and the two cluster counts the printer
// columns read. It is the fleet-level answer to "is this proxy fine", which no single existing
// condition gives: PackageResolved is about the artifact, DependenciesValid about the graph, and
// neither knows whether anything actually deployed.
//
// Ready means the fleet has converged - every matching cluster carries a release in Deployed or
// Completed - which is what the status field's own doc comment has promised since the type was
// written. The counts exist beside it because one broken cluster out of two hundred and a dead
// fleet are both Ready: False, and only the numbers tell them apart.
//
// A proxy-level failure wins over the roll-up. A proxy that cannot resolve its package has no
// deployed releases either, so the roll-up would report something true but useless - "nothing is
// deployed" - where the artifact's own refusal is the blocker worth naming on that line.
func (r *ZarfPackageProxyReconciler) aggregateReady(ctx context.Context, proxy *addonsv1alpha1.ZarfPackageProxy, matched []clusterv1.Cluster) error {
	releases := make([]*addonsv1alpha1.ZarfPackageRelease, 0, len(matched))
	for i := range matched {
		release, err := r.findRelease(ctx, types.NamespacedName{Namespace: proxy.Namespace, Name: proxy.Name}, &matched[i])
		if err != nil {
			return err
		}
		// A nil entry is a matched cluster with no release yet, which is not the same as a
		// cluster that is not matched - the verdict counts it against the fleet.
		releases = append(releases, release)
	}

	condition, ready := readyVerdict(proxy, releases)
	proxy.Status.ClusterCount = int32(len(matched)) //nolint:gosec // a fleet is not 2^31 clusters
	proxy.Status.ReadyClusterCount = int32(ready)   //nolint:gosec // bounded by len(matched)
	meta.SetStatusCondition(&proxy.Status.Conditions, condition)
	return nil
}

// readyVerdict decides the Ready condition and the deployed count from the proxy's own
// conditions and one release per matched cluster, nil where a cluster has none yet. Pure, so
// the decision is testable without an API server - the fetching is the part that needs one.
func readyVerdict(proxy *addonsv1alpha1.ZarfPackageProxy, releases []*addonsv1alpha1.ZarfPackageRelease) (metav1.Condition, int) {
	condition := metav1.Condition{
		Type:               addonsv1alpha1.ConditionReady,
		ObservedGeneration: proxy.Generation,
	}
	set := func(status metav1.ConditionStatus, reason, message string) (metav1.Condition, int) {
		condition.Status, condition.Reason, condition.Message = status, reason, message
		return condition, 0
	}

	// Proxy-level refusals first, in the order they stop work: a cycle deploys nothing at all,
	// and an unresolved package deploys nothing either.
	for _, conditionType := range []string{
		addonsv1alpha1.ConditionDependenciesValid,
		addonsv1alpha1.ConditionPackageResolved,
	} {
		existing := meta.FindStatusCondition(proxy.Status.Conditions, conditionType)
		if existing != nil && existing.Status == metav1.ConditionFalse {
			return set(metav1.ConditionFalse, existing.Reason, existing.Message)
		}
	}

	// Unknown rather than True: a selector matching nothing has not converged, it has not been
	// asked to do anything, and reporting that as success hides a label typo behind a tick.
	if len(releases) == 0 {
		return set(metav1.ConditionUnknown, addonsv1alpha1.ReasonNoMatchingClusters,
			"the cluster selector matches no Clusters")
	}

	ready := 0
	var holding *addonsv1alpha1.ZarfPackageRelease
	for _, release := range releases {
		if release != nil &&
			(release.Status.Phase == addonsv1alpha1.PhaseDeployed || release.Status.Phase == addonsv1alpha1.PhaseCompleted) {
			ready++
			continue
		}
		// The first cluster that is not done decides the reason, in selector order. Not the
		// "worst" by severity: every hold here is equally a reason the fleet has not converged,
		// and a stable choice beats one that moves as phases churn.
		if holding == nil && release != nil {
			holding = release
		}
	}

	if ready == len(releases) {
		condition.Status = metav1.ConditionTrue
		condition.Reason = addonsv1alpha1.ReasonConverged
		condition.Message = fmt.Sprintf("%d of %d clusters deployed", ready, len(releases))
		return condition, ready
	}

	reason, detail := addonsv1alpha1.ReasonAwaitingReleases, "no release has been stamped yet"
	if holding != nil {
		if readyCondition := meta.FindStatusCondition(holding.Status.Conditions, addonsv1alpha1.ConditionReady); readyCondition != nil {
			reason, detail = readyCondition.Reason, readyCondition.Message
		}
	}
	condition.Status = metav1.ConditionFalse
	condition.Reason = reason
	condition.Message = fmt.Sprintf("%d of %d clusters deployed; %s", ready, len(releases), detail)
	return condition, ready
}
