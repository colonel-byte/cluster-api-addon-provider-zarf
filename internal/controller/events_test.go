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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
)

// TestEmitConditionEvents pins the transition rule: an Event per condition whose status or reason
// changed, Warning on False and Normal on True, and nothing at all when neither moved.
func TestEmitConditionEvents(t *testing.T) {
	resolved := metav1.Condition{
		Type:    addonsv1alpha1.ConditionPackageResolved,
		Status:  metav1.ConditionTrue,
		Reason:  addonsv1alpha1.ReasonResolved,
		Message: "pinned to sha256:aaaa",
	}
	failed := metav1.Condition{
		Type:    addonsv1alpha1.ConditionPackageResolved,
		Status:  metav1.ConditionFalse,
		Reason:  addonsv1alpha1.ReasonResolutionFailed,
		Message: "registry refused",
	}

	tests := []struct {
		name   string
		before []metav1.Condition
		after  []metav1.Condition
		want   []string
	}{
		{
			name:   "no change emits nothing",
			before: []metav1.Condition{resolved},
			after:  []metav1.Condition{resolved},
			want:   nil,
		},
		{
			name:   "a changed message alone emits nothing",
			before: []metav1.Condition{resolved},
			after:  []metav1.Condition{{Type: resolved.Type, Status: resolved.Status, Reason: resolved.Reason, Message: "pinned to sha256:bbbb"}},
			want:   nil,
		},
		{
			name:   "a first condition emits Normal",
			before: nil,
			after:  []metav1.Condition{resolved},
			want:   []string{"Normal Resolved PackageResolved: pinned to sha256:aaaa"},
		},
		{
			name:   "a changed status emits one Warning",
			before: []metav1.Condition{resolved},
			after:  []metav1.Condition{failed},
			want:   []string{"Warning ResolutionFailed PackageResolved: registry refused"},
		},
		{
			name:   "a changed reason at the same status emits once",
			before: []metav1.Condition{failed},
			after: []metav1.Condition{{
				Type:    failed.Type,
				Status:  metav1.ConditionFalse,
				Reason:  addonsv1alpha1.ReasonSignatureVerificationFailed,
				Message: "no signature",
			}},
			want: []string{"Warning SignatureVerificationFailed PackageResolved: no signature"},
		},
		{
			name:   "recovery emits Normal",
			before: []metav1.Condition{failed},
			after:  []metav1.Condition{resolved},
			want:   []string{"Normal Resolved PackageResolved: pinned to sha256:aaaa"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := record.NewFakeRecorder(10)
			emitConditionEvents(recorder, &addonsv1alpha1.ZarfPackageProxy{}, test.before, test.after)
			close(recorder.Events)

			var got []string
			for event := range recorder.Events {
				got = append(got, event)
			}
			if len(got) != len(test.want) {
				t.Fatalf("emitted %v, want %v", got, test.want)
			}
			for i := range got {
				if got[i] != test.want[i] {
					t.Errorf("event %d is %q, want %q", i, got[i], test.want[i])
				}
			}
		})
	}
}

// TestEmitConditionEventsNilRecorder proves the nil recorder the unit and envtest cases rely on
// is a no-op rather than a panic.
func TestEmitConditionEventsNilRecorder(_ *testing.T) {
	emitConditionEvents(nil, &addonsv1alpha1.ZarfPackageProxy{}, nil, []metav1.Condition{{
		Type:   addonsv1alpha1.ConditionPackageResolved,
		Status: metav1.ConditionTrue,
		Reason: addonsv1alpha1.ReasonResolved,
	}})
}
