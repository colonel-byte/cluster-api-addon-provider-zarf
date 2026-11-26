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

//go:build envtest

package controller

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/envtest"
)

// TestJobEventWakesOtherReleasesOnTheCluster pins the deploy slot's liveness. The slot is
// cluster-scoped, so a release held with WaitingForActiveDeploy is waiting on a Job another
// proxy stamped; the gate holds without requeueing, so that Job's event is the only thing that
// can bring the waiter back. Mapping the Job to its own proxy alone left the waiter Pending
// forever against a Job that had already been reaped - found in the e2e topology, where a
// release sat for ten minutes naming a Job that no longer existed.
func TestJobEventWakesOtherReleasesOnTheCluster(t *testing.T) {
	c := envtest.Client(t)
	ns := envtest.Namespace(t, c)
	r := &ZarfPackageProxyReconciler{Client: c}

	release := func(name, proxy, cluster string) {
		t.Helper()
		object := &addonsv1alpha1.ZarfPackageRelease{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: addonsv1alpha1.ZarfPackageReleaseSpec{
				ClusterRef:      corev1.LocalObjectReference{Name: cluster},
				PackageProxyRef: addonsv1alpha1.NamespacedReference{Name: proxy},
			},
		}
		if err := c.Create(t.Context(), object); err != nil {
			t.Fatalf("creating release %s: %v", name, err)
		}
	}

	// Two proxies with a release each on one cluster, and a third release on another cluster
	// that must not be woken by either.
	release("holder-workload", "holder-proxy", "workload")
	release("waiter-workload", "waiter-proxy", "workload")
	release("elsewhere-other", "elsewhere-proxy", "other")

	// The Job the holder stamped, as BuildDeployJob labels it.
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "holder-workload-deploy",
			Namespace: ns,
			Labels: map[string]string{
				releaseLabel: "holder-workload",
				clusterLabel: "workload",
			},
		},
	}

	got := r.jobToProxy(t.Context(), job)
	want := []reconcile.Request{
		{NamespacedName: types.NamespacedName{Namespace: ns, Name: "holder-proxy"}},
		{NamespacedName: types.NamespacedName{Namespace: ns, Name: "waiter-proxy"}},
	}
	if len(got) != len(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
	for _, request := range want {
		if !containsRequest(got, request) {
			t.Fatalf("want %v among %v", request, got)
		}
	}
	// A release on a different cluster shares no slot, so its proxy is not this Job's business.
	if containsRequest(got, reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: "elsewhere-proxy"},
	}) {
		t.Fatalf("want no request for a release on another cluster, got %v", got)
	}
}

// TestJobEventWithoutAClusterLabelStillWakesItsOwnProxy keeps the narrow path working: a Job
// carrying no cluster label holds no slot, and its own proxy still gets the event.
func TestJobEventWithoutAClusterLabelStillWakesItsOwnProxy(t *testing.T) {
	c := envtest.Client(t)
	ns := envtest.Namespace(t, c)
	r := &ZarfPackageProxyReconciler{Client: c}

	object := &addonsv1alpha1.ZarfPackageRelease{
		ObjectMeta: metav1.ObjectMeta{Name: "lonely-workload", Namespace: ns},
		Spec: addonsv1alpha1.ZarfPackageReleaseSpec{
			ClusterRef:      corev1.LocalObjectReference{Name: "workload"},
			PackageProxyRef: addonsv1alpha1.NamespacedReference{Name: "lonely-proxy"},
		},
	}
	if err := c.Create(t.Context(), object); err != nil {
		t.Fatalf("creating the release: %v", err)
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lonely-workload-deploy",
			Namespace: ns,
			Labels:    map[string]string{releaseLabel: "lonely-workload"},
		},
	}
	got := r.jobToProxy(t.Context(), job)
	want := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "lonely-proxy"}}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("want exactly %v, got %v", want, got)
	}
}

func containsRequest(requests []reconcile.Request, request reconcile.Request) bool {
	for _, candidate := range requests {
		if candidate == request {
			return true
		}
	}
	return false
}
