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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/resolver"
)

// defaultFollowTagInterval is what the follow-tag-interval annotation falls back to.
const defaultFollowTagInterval = 15 * time.Minute

// resolvePackage runs §6 step 4 on the proxy: pin the tag to a digest, verify the signature
// when a key is named, check the spec.package.api claim, and re-check the tag for drift. It
// mutates only the in-memory proxy status - Reconcile owns the single status patch - and
// returns the requeue interval a follow-tag annotation asks for (zero otherwise).
func (r *ZarfPackageProxyReconciler) resolvePackage(ctx context.Context, proxy *addonsv1alpha1.ZarfPackageProxy) (time.Duration, error) {
	log := ctrl.LoggerFrom(ctx)

	// Nil in tests that exercise matching or stamping alone; main always wires one.
	if r.Resolver == nil {
		return 0, nil
	}

	setResolved := func(status metav1.ConditionStatus, reason, message string) {
		meta.SetStatusCondition(&proxy.Status.Conditions, metav1.Condition{
			Type:               addonsv1alpha1.ConditionPackageResolved,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: proxy.Generation,
		})
	}

	// ApplyOnce proxies take no part in re-resolution or tag drift (§3): a seed's identity was
	// fixed the day it pinned, and the escape hatch is on the workload cluster, not here.
	ref := proxy.Spec.Package.Repository + ":" + proxy.Spec.Package.Version
	resolvedAlready := proxy.Status.ResolvedDigest != "" && proxy.Status.ResolvedRef == ref
	if proxy.Spec.Strategy == addonsv1alpha1.StrategyApplyOnce && resolvedAlready {
		return 0, nil
	}

	opts := resolver.Options{PlainHTTP: r.RegistryPlainHTTP}
	if keyRef := proxy.Spec.Package.SignatureKeySecretRef; keyRef != nil {
		key, err := keyRef.Key(proxy.Namespace, r.SecretNamespaces, "--secret-namespaces")
		if err != nil {
			setResolved(metav1.ConditionFalse, addonsv1alpha1.ReasonCrossNamespaceNotEnabled, err.Error())
			return 0, nil
		}
		secret := &corev1.Secret{}
		if err := r.Get(ctx, key, secret); err != nil {
			if apierrors.IsNotFound(err) {
				setResolved(metav1.ConditionFalse, addonsv1alpha1.ReasonResolutionFailed,
					fmt.Sprintf("signatureKeySecretRef names %s, which does not exist", key))
				return 0, nil
			}
			return 0, err
		}
		publicKey, ok := secret.Data["cosign.pub"]
		if !ok || len(publicKey) == 0 {
			setResolved(metav1.ConditionFalse, addonsv1alpha1.ReasonResolutionFailed,
				fmt.Sprintf("secret %s carries no cosign.pub key", key))
			return 0, nil
		}
		opts.CosignPublicKey = publicKey
	}

	followTag, interval := r.followTag(ctx, proxy)

	if !resolvedAlready || followTag {
		result, err := r.Resolver.Resolve(ctx, proxy.Spec.Package.Repository, proxy.Spec.Package.Version, opts)
		if err != nil {
			reason := addonsv1alpha1.ReasonResolutionFailed
			if len(opts.CosignPublicKey) > 0 {
				// With a key named, verification is a precondition of loading; a refusal
				// surfaces here, before any deploy Job could exist.
				reason = addonsv1alpha1.ReasonSignatureVerificationFailed
			}
			setResolved(metav1.ConditionFalse, reason, err.Error())
			return interval, nil
		}

		if declared := proxy.Spec.Package.API; declared != "" && string(declared) != result.APIVersion {
			// §4C: the declaration is a claim the controller verifies against the artifact.
			setResolved(metav1.ConditionFalse, addonsv1alpha1.ReasonPackageAPIMismatch,
				fmt.Sprintf("spec.package.api declares %s but the artifact is %s", declared, result.APIVersion))
			return interval, nil
		}

		if resolvedAlready && result.Digest != proxy.Status.ResolvedDigest {
			log.Info("follow-tag re-pinned the digest", "from", proxy.Status.ResolvedDigest, "to", result.Digest)
		}
		proxy.Status.ResolvedDigest = result.Digest
		proxy.Status.ResolvedRef = ref
		proxy.Status.PackageKind = packageKind(result)
		setResolved(metav1.ConditionTrue, addonsv1alpha1.ReasonResolved,
			fmt.Sprintf("%s pinned to %s", ref, result.Digest))
		if result.Verified {
			meta.SetStatusCondition(&proxy.Status.Conditions, metav1.Condition{
				Type:               addonsv1alpha1.ConditionPackageSignatureVerified,
				Status:             metav1.ConditionTrue,
				Reason:             addonsv1alpha1.ReasonSignatureVerified,
				Message:            "cosign signature verified against the pinned digest",
				ObservedGeneration: proxy.Generation,
			})
		} else {
			// Absent, not True: a signature with no key to check it against is not
			// verification (the Signature Trust Model).
			meta.RemoveStatusCondition(&proxy.Status.Conditions, addonsv1alpha1.ConditionPackageSignatureVerified)
			log.Info("no signature key named; deploying unverified", "ref", ref)
		}
		// A fresh pin is by definition current; clear any standing drift warning.
		meta.RemoveStatusCondition(&proxy.Status.Conditions, addonsv1alpha1.ConditionTagDrift)
		return interval, nil
	}

	// Already pinned: re-check the tag (§3). Drift is a warning, never an adoption.
	current, err := r.Resolver.ResolveDigest(ctx, proxy.Spec.Package.Repository, proxy.Spec.Package.Version, opts)
	if err != nil {
		log.Info("unable to re-check the tag against the pinned digest", "error", err.Error())
		return interval, nil
	}
	if current != proxy.Status.ResolvedDigest {
		meta.SetStatusCondition(&proxy.Status.Conditions, metav1.Condition{
			Type:               addonsv1alpha1.ConditionTagDrift,
			Status:             metav1.ConditionTrue,
			Reason:             addonsv1alpha1.ReasonTagContentChanged,
			Message:            fmt.Sprintf("tag %s now resolves to %s; the fleet stays pinned to %s - bump spec.package.version to adopt", ref, current, proxy.Status.ResolvedDigest),
			ObservedGeneration: proxy.Generation,
		})
	} else {
		meta.RemoveStatusCondition(&proxy.Status.Conditions, addonsv1alpha1.ConditionTagDrift)
	}
	return interval, nil
}

// followTag reads the §3 tag-following annotations: enabled, and at what interval.
func (r *ZarfPackageProxyReconciler) followTag(ctx context.Context, proxy *addonsv1alpha1.ZarfPackageProxy) (bool, time.Duration) {
	if proxy.Annotations[addonsv1alpha1.AnnotationFollowTag] != "true" {
		return false, 0
	}
	interval := defaultFollowTagInterval
	if raw, ok := proxy.Annotations[addonsv1alpha1.AnnotationFollowTagInterval]; ok {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			ctrl.LoggerFrom(ctx).Info("invalid follow-tag-interval; falling back to the default",
				"value", raw, "default", defaultFollowTagInterval.String())
		} else {
			interval = parsed
		}
	}
	return true, interval
}

// packageKind names the resolved artifact's kind for status.
func packageKind(result resolver.Result) string {
	if result.IsInitPackage {
		return "ZarfInitConfig"
	}
	return "ZarfPackageConfig"
}
