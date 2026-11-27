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
	"sort"
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

	proxy.Status.StageCount = int32(len(proxy.Spec.Stages))
	proxy.Status.Version = declaredVersion(proxy)
	if proxy.Spec.Package == nil {
		return 0, r.resolveStagedPackages(ctx, proxy)
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
	publicKey, reason, message, err := r.cosignKey(ctx, proxy.Namespace, proxy.Spec.Package.SignatureKeySecretRef)
	if err != nil {
		return 0, err
	}
	if reason != "" {
		setResolved(metav1.ConditionFalse, reason, message)
		return 0, nil
	}
	opts.CosignPublicKey = publicKey
	reason, message, err = r.registryAccess(ctx, proxy.Namespace, proxy.Spec.Package, &opts)
	if err != nil {
		return 0, err
	}
	if reason != "" {
		setResolved(metav1.ConditionFalse, reason, message)
		return 0, nil
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
		proxy.Status.PackageName = result.Name
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

// cosignKey loads the Cosign public key keyRef names, resolving cross-namespace per the
// --secret-namespaces flag. A nil keyRef returns an empty key. A refusal comes back as a
// condition (reason, message) pair for the caller to surface; the error return is a transient
// API failure worth a retry.
func (r *ZarfPackageProxyReconciler) cosignKey(ctx context.Context, namespace string, keyRef *addonsv1alpha1.NamespacedReference) ([]byte, string, string, error) {
	if keyRef == nil {
		return nil, "", "", nil
	}
	key, err := keyRef.Key(namespace, r.SecretNamespaces, "--secret-namespaces")
	if err != nil {
		return nil, addonsv1alpha1.ReasonCrossNamespaceNotEnabled, err.Error(), nil
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, key, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, addonsv1alpha1.ReasonResolutionFailed,
				fmt.Sprintf("signatureKeySecretRef names %s, which does not exist", key), nil
		}
		return nil, "", "", err
	}
	publicKey, ok := secret.Data["cosign.pub"]
	if !ok || len(publicKey) == 0 {
		return nil, addonsv1alpha1.ReasonResolutionFailed,
			fmt.Sprintf("secret %s carries no cosign.pub key", key), nil
	}
	return publicKey, "", "", nil
}

// resolveInitPackage pins and verifies spec.init.package - §6 step 5's auto-init deploy path -
// through resolvePackage's exact chain, onto status.initResolvedDigest and the
// InitPackageResolved condition, with one extra refusal: the artifact must be a ZarfInitConfig.
// Pin-once, no follow-tag and no drift re-check: bumping init.package.version is the update path.
func (r *ZarfPackageProxyReconciler) resolveInitPackage(ctx context.Context, proxy *addonsv1alpha1.ZarfPackageProxy) error {
	if r.Resolver == nil {
		return nil
	}
	initOpts := proxy.Spec.Init
	if initOpts != nil && initOpts.PackageRef != "" {
		// The staged form: the init package resolves as a spec.packages entry, and
		// resolveStagedPackages mirrors its verdict onto the init status fields.
		return nil
	}
	if initOpts == nil || initOpts.Package == nil {
		// Auto-init was un-declared; a stale pin must not keep stamping init Jobs.
		proxy.Status.InitResolvedDigest = ""
		proxy.Status.InitResolvedRef = ""
		meta.RemoveStatusCondition(&proxy.Status.Conditions, addonsv1alpha1.ConditionInitPackageResolved)
		return nil
	}

	setResolved := func(status metav1.ConditionStatus, reason, message string) {
		meta.SetStatusCondition(&proxy.Status.Conditions, metav1.Condition{
			Type:               addonsv1alpha1.ConditionInitPackageResolved,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: proxy.Generation,
		})
	}

	pkg := initOpts.Package
	ref := pkg.Repository + ":" + pkg.Version
	if proxy.Status.InitResolvedDigest != "" && proxy.Status.InitResolvedRef == ref {
		return nil
	}

	opts := resolver.Options{PlainHTTP: r.RegistryPlainHTTP}
	publicKey, reason, message, err := r.cosignKey(ctx, proxy.Namespace, pkg.SignatureKeySecretRef)
	if err != nil {
		return err
	}
	if reason != "" {
		setResolved(metav1.ConditionFalse, reason, message)
		return nil
	}
	opts.CosignPublicKey = publicKey
	reason, message, err = r.registryAccess(ctx, proxy.Namespace, pkg, &opts)
	if err != nil {
		return err
	}
	if reason != "" {
		setResolved(metav1.ConditionFalse, reason, message)
		return nil
	}

	result, err := r.Resolver.Resolve(ctx, pkg.Repository, pkg.Version, opts)
	if err != nil {
		reason := addonsv1alpha1.ReasonResolutionFailed
		if len(opts.CosignPublicKey) > 0 {
			reason = addonsv1alpha1.ReasonSignatureVerificationFailed
		}
		setResolved(metav1.ConditionFalse, reason, err.Error())
		return nil
	}
	if declared := pkg.API; declared != "" && string(declared) != result.APIVersion {
		setResolved(metav1.ConditionFalse, addonsv1alpha1.ReasonPackageAPIMismatch,
			fmt.Sprintf("init.package.api declares %s but the artifact is %s", declared, result.APIVersion))
		return nil
	}
	if !result.IsInitPackage {
		setResolved(metav1.ConditionFalse, addonsv1alpha1.ReasonNotAnInitPackage,
			fmt.Sprintf("%s resolved to a %s, not the ZarfInitConfig auto-init needs", ref, packageKind(result)))
		return nil
	}

	proxy.Status.InitResolvedDigest = result.Digest
	proxy.Status.InitResolvedRef = ref
	setResolved(metav1.ConditionTrue, addonsv1alpha1.ReasonResolved,
		fmt.Sprintf("%s pinned to %s", ref, result.Digest))
	return nil
}

// resolveStagedPackages pins every spec.packages entry onto status.packages[], pin-once per
// entry with re-resolve on a changed ref, through resolvePackage's exact chain per source. The
// PackageResolved condition is the fleet verdict: True when every entry pinned, False with the
// first failing entry's reason. No follow-tag and no drift re-check on staged sources: bumping
// a package's version is the update path. When init.packageRef names an entry, its verdict is
// mirrored onto the init status fields so the existing init machinery runs unchanged.
func (r *ZarfPackageProxyReconciler) resolveStagedPackages(ctx context.Context, proxy *addonsv1alpha1.ZarfPackageProxy) error {
	setResolved := func(status metav1.ConditionStatus, reason, message string) {
		meta.SetStatusCondition(&proxy.Status.Conditions, metav1.Condition{
			Type:               addonsv1alpha1.ConditionPackageResolved,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: proxy.Generation,
		})
	}

	previous := map[string]addonsv1alpha1.PackageResolution{}
	for _, entry := range proxy.Status.Packages {
		previous[entry.Name] = entry
	}

	keys := make([]string, 0, len(proxy.Spec.Packages))
	for key := range proxy.Spec.Packages {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	entries := make([]addonsv1alpha1.PackageResolution, 0, len(keys))
	failReason, failMessage := "", ""
	for _, key := range keys {
		pkg := proxy.Spec.Packages[key]
		ref := pkg.Repository + ":" + pkg.Version
		if prev, ok := previous[key]; ok && prev.ResolvedDigest != "" && prev.ResolvedRef == ref {
			entries = append(entries, prev)
			continue
		}

		fail := func(reason, message string) {
			if failReason == "" {
				failReason, failMessage = reason, fmt.Sprintf("packages[%s]: %s", key, message)
			}
			if prev, ok := previous[key]; ok {
				entries = append(entries, prev)
			}
		}

		opts := resolver.Options{PlainHTTP: r.RegistryPlainHTTP}
		publicKey, reason, message, err := r.cosignKey(ctx, proxy.Namespace, pkg.SignatureKeySecretRef)
		if err != nil {
			return err
		}
		if reason != "" {
			fail(reason, message)
			continue
		}
		opts.CosignPublicKey = publicKey
		reason, message, err = r.registryAccess(ctx, proxy.Namespace, &pkg, &opts)
		if err != nil {
			return err
		}
		if reason != "" {
			fail(reason, message)
			continue
		}

		result, err := r.Resolver.Resolve(ctx, pkg.Repository, pkg.Version, opts)
		if err != nil {
			reason := addonsv1alpha1.ReasonResolutionFailed
			if len(opts.CosignPublicKey) > 0 {
				reason = addonsv1alpha1.ReasonSignatureVerificationFailed
			}
			fail(reason, err.Error())
			continue
		}
		if declared := pkg.API; declared != "" && string(declared) != result.APIVersion {
			fail(addonsv1alpha1.ReasonPackageAPIMismatch,
				fmt.Sprintf("api declares %s but the artifact is %s", declared, result.APIVersion))
			continue
		}

		entries = append(entries, addonsv1alpha1.PackageResolution{
			Name:           key,
			ResolvedRef:    ref,
			ResolvedDigest: result.Digest,
			PackageName:    result.Name,
			PackageKind:    packageKind(result),
		})
	}
	proxy.Status.Packages = entries

	if failReason != "" {
		setResolved(metav1.ConditionFalse, failReason, failMessage)
	} else {
		setResolved(metav1.ConditionTrue, addonsv1alpha1.ReasonResolved,
			fmt.Sprintf("%d packages pinned", len(entries)))
	}

	if proxy.Spec.Init != nil && proxy.Spec.Init.PackageRef != "" {
		r.mirrorInitResolution(proxy, proxy.Spec.Init.PackageRef)
	}
	return nil
}

// mirrorInitResolution writes init.packageRef's status.packages verdict onto the init status
// fields and the InitPackageResolved condition - the shim that lets convergeInit and
// settleInitJob run unchanged on the staged form.
func (r *ZarfPackageProxyReconciler) mirrorInitResolution(proxy *addonsv1alpha1.ZarfPackageProxy, key string) {
	setResolved := func(status metav1.ConditionStatus, reason, message string) {
		meta.SetStatusCondition(&proxy.Status.Conditions, metav1.Condition{
			Type:               addonsv1alpha1.ConditionInitPackageResolved,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: proxy.Generation,
		})
	}
	for _, entry := range proxy.Status.Packages {
		if entry.Name != key || entry.ResolvedDigest == "" {
			continue
		}
		if entry.PackageKind != "ZarfInitConfig" {
			setResolved(metav1.ConditionFalse, addonsv1alpha1.ReasonNotAnInitPackage,
				fmt.Sprintf("init.packageRef %s resolved to a %s, not the ZarfInitConfig auto-init needs", key, entry.PackageKind))
			return
		}
		proxy.Status.InitResolvedDigest = entry.ResolvedDigest
		proxy.Status.InitResolvedRef = entry.ResolvedRef
		setResolved(metav1.ConditionTrue, addonsv1alpha1.ReasonResolved,
			fmt.Sprintf("%s pinned to %s", entry.ResolvedRef, entry.ResolvedDigest))
		return
	}
	// Unresolved: PackageResolved already carries the why; the init condition just waits.
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
