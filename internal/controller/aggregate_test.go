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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
)

// release builds a release in a phase, optionally carrying a Ready reason for the roll-up to
// borrow when it is the one holding the fleet back.
func release(phase addonsv1alpha1.ReleasePhase, reason, message string) *addonsv1alpha1.ZarfPackageRelease {
	object := &addonsv1alpha1.ZarfPackageRelease{}
	object.Status.Phase = phase
	if reason != "" {
		object.Status.Conditions = []metav1.Condition{
			{
				Type:    addonsv1alpha1.ConditionReady,
				Status:  metav1.ConditionFalse,
				Reason:  reason,
				Message: message,
			},
		}
	}
	return object
}

func TestReadyVerdict(t *testing.T) {
	tests := []struct {
		name            string
		proxyConditions []metav1.Condition
		releases        []*addonsv1alpha1.ZarfPackageRelease
		wantStatus      metav1.ConditionStatus
		wantReason      string
		wantReady       int
		wantInMessage   string
	}{
		{
			name: "every cluster deployed",
			releases: []*addonsv1alpha1.ZarfPackageRelease{
				release(addonsv1alpha1.PhaseDeployed, "", ""),
				release(addonsv1alpha1.PhaseDeployed, "", ""),
			},
			wantStatus:    metav1.ConditionTrue,
			wantReason:    addonsv1alpha1.ReasonConverged,
			wantReady:     2,
			wantInMessage: "2 of 2 clusters deployed",
		},
		{
			name: "an ApplyOnce release that completed counts as deployed",
			releases: []*addonsv1alpha1.ZarfPackageRelease{
				release(addonsv1alpha1.PhaseCompleted, "", ""),
			},
			wantStatus: metav1.ConditionTrue,
			wantReason: addonsv1alpha1.ReasonConverged,
			wantReady:  1,
		},
		{
			name: "one stuck cluster out of three borrows its reason, and the counts tell the story",
			releases: []*addonsv1alpha1.ZarfPackageRelease{
				release(addonsv1alpha1.PhaseDeployed, "", ""),
				release(addonsv1alpha1.PhasePending, addonsv1alpha1.ReasonWaitingForDependency, "dependency \"storage\" is not deployed here"),
				release(addonsv1alpha1.PhaseDeployed, "", ""),
			},
			wantStatus:    metav1.ConditionFalse,
			wantReason:    addonsv1alpha1.ReasonWaitingForDependency,
			wantReady:     2,
			wantInMessage: "2 of 3 clusters deployed",
		},
		{
			name: "a matched cluster with no release yet",
			releases: []*addonsv1alpha1.ZarfPackageRelease{
				nil,
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: addonsv1alpha1.ReasonAwaitingReleases,
			wantReady:  0,
		},
		{
			name:       "a selector that matches nothing is Unknown, not converged",
			releases:   nil,
			wantStatus: metav1.ConditionUnknown,
			wantReason: addonsv1alpha1.ReasonNoMatchingClusters,
			wantReady:  0,
		},
		{
			name: "a proxy-level resolution failure wins over the roll-up",
			proxyConditions: []metav1.Condition{
				{
					Type:    addonsv1alpha1.ConditionPackageResolved,
					Status:  metav1.ConditionFalse,
					Reason:  addonsv1alpha1.ReasonSignatureVerificationFailed,
					Message: "the signature does not verify",
				},
			},
			releases: []*addonsv1alpha1.ZarfPackageRelease{
				release(addonsv1alpha1.PhaseDeployed, "", ""),
			},
			// Deployed releases exist from an earlier pin, but the current artifact is refused;
			// naming the refusal is more use than reporting the stale fleet as converged.
			wantStatus: metav1.ConditionFalse,
			wantReason: addonsv1alpha1.ReasonSignatureVerificationFailed,
			wantReady:  0,
		},
		{
			name: "a dependency cycle wins over a resolution failure",
			proxyConditions: []metav1.Condition{
				{
					Type:   addonsv1alpha1.ConditionPackageResolved,
					Status: metav1.ConditionFalse,
					Reason: addonsv1alpha1.ReasonResolutionFailed,
				},
				{
					Type:   addonsv1alpha1.ConditionDependenciesValid,
					Status: metav1.ConditionFalse,
					Reason: addonsv1alpha1.ReasonDependencyCycle,
				},
			},
			releases:   []*addonsv1alpha1.ZarfPackageRelease{nil},
			wantStatus: metav1.ConditionFalse,
			wantReason: addonsv1alpha1.ReasonDependencyCycle,
			wantReady:  0,
		},
		{
			name: "a True proxy condition does not short-circuit the roll-up",
			proxyConditions: []metav1.Condition{
				{
					Type:   addonsv1alpha1.ConditionPackageResolved,
					Status: metav1.ConditionTrue,
					Reason: addonsv1alpha1.ReasonResolved,
				},
			},
			releases: []*addonsv1alpha1.ZarfPackageRelease{
				release(addonsv1alpha1.PhaseDeploying, "", ""),
			},
			wantStatus: metav1.ConditionFalse,
			wantReason: addonsv1alpha1.ReasonAwaitingReleases,
			wantReady:  0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			proxy := &addonsv1alpha1.ZarfPackageProxy{}
			proxy.Status.Conditions = test.proxyConditions

			condition, ready := readyVerdict(proxy, test.releases)
			if condition.Type != addonsv1alpha1.ConditionReady {
				t.Fatalf("want condition type %q, got %q", addonsv1alpha1.ConditionReady, condition.Type)
			}
			if condition.Status != test.wantStatus {
				t.Errorf("want status %s, got %s", test.wantStatus, condition.Status)
			}
			if condition.Reason != test.wantReason {
				t.Errorf("want reason %s, got %s", test.wantReason, condition.Reason)
			}
			if ready != test.wantReady {
				t.Errorf("want %d deployed, got %d", test.wantReady, ready)
			}
			if test.wantInMessage != "" && !strings.Contains(condition.Message, test.wantInMessage) {
				t.Errorf("want %q in the message, got %q", test.wantInMessage, condition.Message)
			}
		})
	}
}
