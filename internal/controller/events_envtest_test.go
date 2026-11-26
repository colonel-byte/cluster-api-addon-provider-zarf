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
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/envtest"
)

// TestConditionEventLands drives a failing resolution through a real recorder and asserts the
// Event reaches the API server, then that a second identical reconcile adds none - the
// transition rule, checked where the Events actually are.
func TestConditionEventLands(t *testing.T) {
	c := envtest.Client(t)
	ns := envtest.Namespace(t, c)
	newTestProxy(t, c, ns, "events", nil)

	r := &ZarfPackageProxyReconciler{
		Client:   c,
		Resolver: &stubResolver{err: errors.New("the registry refused the read")},
		Recorder: testRecorder(t),
	}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: "events"}}
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	events := &corev1.EventList{}
	err := wait.PollUntilContextTimeout(t.Context(), 100*time.Millisecond, 30*time.Second, true,
		func(ctx context.Context) (bool, error) {
			if err := c.List(ctx, events, client.InNamespace(ns)); err != nil {
				return false, err
			}
			return len(events.Items) > 0, nil
		})
	if err != nil {
		t.Fatalf("no Event landed for the failed resolution: %v", err)
	}
	if len(events.Items) != 1 {
		t.Fatalf("the failed resolution landed %d Events, want 1", len(events.Items))
	}
	event := events.Items[0]
	if event.Type != corev1.EventTypeWarning || event.Reason != addonsv1alpha1.ReasonResolutionFailed {
		t.Errorf("landed %s/%s, want Warning/%s", event.Type, event.Reason, addonsv1alpha1.ReasonResolutionFailed)
	}
	if event.InvolvedObject.Name != "events" || event.InvolvedObject.Kind != "ZarfPackageProxy" {
		t.Errorf("the Event names %s/%s, want ZarfPackageProxy/events", event.InvolvedObject.Kind, event.InvolvedObject.Name)
	}

	// A second pass computes the same condition, so it is not a transition and emits nothing.
	// Event aggregation would fold a repeat into a count on this same object, so the count is
	// what proves the emission never happened.
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	after := &corev1.EventList{}
	if err := c.List(t.Context(), after, client.InNamespace(ns)); err != nil {
		t.Fatalf("unable to list Events: %v", err)
	}
	if len(after.Items) != 1 || after.Items[0].Count != 1 {
		t.Errorf("the second reconcile emitted again: %d Events, first with count %d",
			len(after.Items), after.Items[0].Count)
	}
}

// testRecorder is a real client-go recorder writing to the envtest API server - the only way an
// Event is observable as an object rather than as a call on a fake.
func testRecorder(t *testing.T) record.EventRecorder {
	t.Helper()

	cfg, err := clientcmd.RESTConfigFromKubeConfig(envtest.Kubeconfig(t))
	if err != nil {
		t.Fatalf("unable to build a rest config: %v", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("unable to build a clientset: %v", err)
	}
	scheme, err := envtest.Scheme()
	if err != nil {
		t.Fatalf("unable to build the scheme: %v", err)
	}

	broadcaster := record.NewBroadcaster()
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: clientset.CoreV1().Events("")})
	t.Cleanup(broadcaster.Shutdown)
	return broadcaster.NewRecorder(scheme, corev1.EventSource{Component: "events-envtest"})
}
