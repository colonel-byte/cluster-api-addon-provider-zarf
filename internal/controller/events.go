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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
)

// patchStatus patches obj's status against before and, on success, emits one Event per changed
// condition. Every status write in this controller goes through here, so condition events have a
// single emission point rather than a call at each site that happens to set a condition.
func (r *ZarfPackageProxyReconciler) patchStatus(ctx context.Context, obj, before client.Object) error {
	if err := r.Status().Patch(ctx, obj, client.MergeFrom(before)); err != nil {
		return err
	}
	emitConditionEvents(r.Recorder, obj, statusConditions(before), statusConditions(obj))
	return nil
}

// emitConditionEvents emits an Event for each condition in after whose status or reason differs
// from before: Warning when the new status is False, Normal otherwise.
//
// It keys on transitions rather than on state because this controller reconciles on resync and on
// every Cluster, release and Job event, so a condition that is simply still False would otherwise
// produce an Event per pass - and an event stream that reports nothing new is one nobody reads.
// A changed message alone is not a transition: messages carry digests, Job names and timestamps
// that churn without the verdict changing.
func emitConditionEvents(recorder record.EventRecorder, obj client.Object, before, after []metav1.Condition) {
	if recorder == nil {
		return
	}
	// One event per reason per pass. The fleet-level Ready condition deliberately borrows the
	// reason of whatever is blocking it - a refused signature surfaces as
	// SignatureVerificationFailed on Ready too, so `kubectl get` names the actual cause - which
	// means a single failure changes two conditions carrying one reason. Two Warnings saying
	// the same word is the noise the transition keying above exists to avoid, so the second is
	// dropped. Conditions that are genuinely different facts carry different reasons and both
	// still emit.
	emitted := make(map[string]bool, len(after))
	for _, condition := range after {
		old := meta.FindStatusCondition(before, condition.Type)
		if old != nil && old.Status == condition.Status && old.Reason == condition.Reason {
			continue
		}
		if emitted[condition.Reason] {
			continue
		}
		emitted[condition.Reason] = true
		eventType := corev1.EventTypeNormal
		if condition.Status == metav1.ConditionFalse {
			eventType = corev1.EventTypeWarning
		}
		recorder.Eventf(obj, eventType, condition.Reason, "%s: %s", condition.Type, condition.Message)
	}
}

// statusConditions reads .status.conditions off either of this provider's kinds.
func statusConditions(obj client.Object) []metav1.Condition {
	switch typed := obj.(type) {
	case *addonsv1alpha1.ZarfPackageProxy:
		return typed.Status.Conditions
	case *addonsv1alpha1.ZarfPackageRelease:
		return typed.Status.Conditions
	default:
		return nil
	}
}
