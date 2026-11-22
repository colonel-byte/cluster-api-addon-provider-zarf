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
	"bytes"
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	addonsv1alpha1 "github.com/colonel-byte/cluster-api-addon-provider-zarf/api/v1alpha1"
	"github.com/colonel-byte/cluster-api-addon-provider-zarf/internal/deployspec"
)

// writeDeploySpecs renders and writes each matched cluster's deploy-spec Secret - the deploy
// pod's entire input (§6 step 6), built only from a resolved proxy so the package reference is
// digest-pinned by construction. Rendering runs here, in the controller, because the pod cannot
// and must not read Cluster metadata (docs/agent/choice-deploy-job.md).
func (r *ZarfPackageProxyReconciler) writeDeploySpecs(ctx context.Context, proxy *addonsv1alpha1.ZarfPackageProxy, matched []clusterv1.Cluster) error {
	if !meta.IsStatusConditionTrue(proxy.Status.Conditions, addonsv1alpha1.ConditionPackageResolved) ||
		proxy.Status.ResolvedDigest == "" {
		return nil
	}
	log := ctrl.LoggerFrom(ctx)

	// The repository comes from resolvedRef, not the live spec: an ApplyOnce proxy whose spec
	// moved after pinning keeps deploying what it pinned. The ref is repository:version, and
	// the last colon is the version separator (a registry port sits before a slash).
	ref := proxy.Status.ResolvedRef
	repository := ref
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		repository = ref[:i]
	}
	packageRef := repository + "@" + proxy.Status.ResolvedDigest

	for i := range matched {
		cluster := &matched[i]
		release, err := r.findRelease(ctx, client.ObjectKeyFromObject(proxy), cluster)
		if err != nil {
			return err
		}
		if release == nil {
			continue
		}

		spec, renderErr := r.renderSpec(proxy, cluster, packageRef)
		releaseBefore := release.DeepCopy()
		if renderErr != nil {
			meta.SetStatusCondition(&release.Status.Conditions, metav1.Condition{
				Type:    addonsv1alpha1.ConditionDeploySpecReady,
				Status:  metav1.ConditionFalse,
				Reason:  addonsv1alpha1.ReasonRenderFailed,
				Message: renderErr.Error(),
			})
			if err := r.Status().Patch(ctx, release, client.MergeFrom(releaseBefore)); err != nil {
				return err
			}
			log.Info("deploy spec rendering refused", "release", client.ObjectKeyFromObject(release),
				"error", renderErr.Error())
			continue
		}

		if err := r.ensureSpecSecret(ctx, release, spec); err != nil {
			return err
		}

		meta.SetStatusCondition(&release.Status.Conditions, metav1.Condition{
			Type:    addonsv1alpha1.ConditionDeploySpecReady,
			Status:  metav1.ConditionTrue,
			Reason:  addonsv1alpha1.ReasonDeploySpecWritten,
			Message: fmt.Sprintf("deploy spec rendered for %s", packageRef),
		})
		// The release mirrors the proxy's verification verdict: the digest it deploys is the
		// digest that was verified (§2B's example shows the condition here).
		if verified := meta.FindStatusCondition(proxy.Status.Conditions, addonsv1alpha1.ConditionPackageSignatureVerified); verified != nil {
			meta.SetStatusCondition(&release.Status.Conditions, metav1.Condition{
				Type:    addonsv1alpha1.ConditionPackageSignatureVerified,
				Status:  verified.Status,
				Reason:  verified.Reason,
				Message: verified.Message,
			})
		}
		if err := r.Status().Patch(ctx, release, client.MergeFrom(releaseBefore)); err != nil {
			return err
		}
	}
	return nil
}

// renderSpec builds one cluster's deploy spec, substituting {{ .Cluster... }} in values and set
// entries against the target cluster's object content.
func (r *ZarfPackageProxyReconciler) renderSpec(proxy *addonsv1alpha1.ZarfPackageProxy, cluster *clusterv1.Cluster, packageRef string) ([]byte, error) {
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cluster)
	if err != nil {
		return nil, err
	}

	spec := deployspec.Spec{
		Package:    packageRef,
		PackageAPI: string(proxy.Spec.Package.API),
	}
	for _, component := range proxy.Spec.Components {
		spec.Components = append(spec.Components, deployspec.Component{
			Name:   component.Name,
			Flavor: component.Flavor,
		})
	}
	if proxy.Spec.Values != nil {
		rendered, err := deployspec.Render(string(proxy.Spec.Values.Raw), content)
		if err != nil {
			return nil, err
		}
		spec.Values = rendered
	}
	if len(proxy.Spec.Set) > 0 {
		spec.Set = map[string]string{}
		for key, value := range proxy.Spec.Set {
			rendered, err := deployspec.Render(value, content)
			if err != nil {
				return nil, err
			}
			spec.Set[key] = rendered
		}
	}
	return spec.Marshal()
}

// ensureSpecSecret writes the release's deploy-spec Secret, controller-owned by the release so
// it garbage-collects with it, updating in place only when the rendering changed.
func (r *ZarfPackageProxyReconciler) ensureSpecSecret(ctx context.Context, release *addonsv1alpha1.ZarfPackageRelease, spec []byte) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			// shortcut: not hash-truncated, same stance as the release name itself.
			Name:      release.Name + "-deploy-spec",
			Namespace: release.Namespace,
		},
	}
	existing := secret.DeepCopy()
	err := r.Get(ctx, client.ObjectKeyFromObject(secret), existing)
	switch {
	case apierrors.IsNotFound(err):
		secret.Data = map[string][]byte{deployspec.FileName: spec}
		if err := controllerutil.SetControllerReference(release, secret, r.Scheme()); err != nil {
			return err
		}
		return r.Create(ctx, secret)
	case err != nil:
		return err
	}
	if bytes.Equal(existing.Data[deployspec.FileName], spec) {
		return nil
	}
	before := existing.DeepCopy()
	existing.Data = map[string][]byte{deployspec.FileName: spec}
	return r.Patch(ctx, existing, client.MergeFrom(before))
}
