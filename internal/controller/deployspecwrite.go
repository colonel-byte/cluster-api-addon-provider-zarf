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
	"maps"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
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

	packageRef := pinnedRef(proxy.Status.ResolvedRef, proxy.Status.ResolvedDigest)

	// The init spec renders only from a resolved init package; until then the main spec is
	// written alone and the init Job has nothing to start from (§6 step 5).
	initPackageRef := ""
	if proxy.Spec.Init != nil && proxy.Spec.Init.Package != nil &&
		meta.IsStatusConditionTrue(proxy.Status.Conditions, addonsv1alpha1.ConditionInitPackageResolved) {
		initPackageRef = pinnedRef(proxy.Status.InitResolvedRef, proxy.Status.InitResolvedDigest)
	}

	for i := range matched {
		cluster := &matched[i]
		release, err := r.findRelease(ctx, client.ObjectKeyFromObject(proxy), cluster)
		if err != nil {
			return err
		}
		if release == nil {
			continue
		}

		files, renderErr := r.renderSpecs(proxy, cluster, packageRef, initPackageRef)
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

		if err := r.ensureSpecSecret(ctx, release, files); err != nil {
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

// pinnedRef turns a pinned "repository:version" status ref and its digest into the
// "repository@digest" reference a deploy spec carries. The repository comes from the status ref,
// not the live spec: an ApplyOnce proxy whose spec moved after pinning keeps deploying what it
// pinned. The last colon is the version separator (a registry port sits before a slash).
func pinnedRef(ref, digest string) string {
	repository := ref
	if i := strings.LastIndex(ref, ":"); i >= 0 {
		repository = ref[:i]
	}
	return repository + "@" + digest
}

// renderSpecs builds one cluster's deploy-spec files, substituting {{ .Cluster... }} in values
// and set entries against the target cluster's object content: the main spec under
// deployspec.FileName, and - when initPackageRef names a resolved init package - the auto-init
// spec under deployspec.InitFileName (§6 step 5).
func (r *ZarfPackageProxyReconciler) renderSpecs(proxy *addonsv1alpha1.ZarfPackageProxy, cluster *clusterv1.Cluster, packageRef, initPackageRef string) (map[string][]byte, error) {
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(cluster)
	if err != nil {
		return nil, err
	}

	spec, err := buildSpec(packageRef, proxy.Spec.Package.API, proxy.Spec.Values, proxy.Spec.Set, r.RegistryPlainHTTP, content)
	if err != nil {
		return nil, err
	}
	for _, component := range proxy.Spec.Components {
		spec.Components = append(spec.Components, deployspec.Component{
			Name:   component.Name,
			Flavor: component.Flavor,
		})
	}
	raw, err := spec.Marshal()
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{deployspec.FileName: raw}

	if initPackageRef != "" {
		initOpts := proxy.Spec.Init
		initSpec, err := buildSpec(initPackageRef, initOpts.Package.API, initOpts.Values, initOpts.Set, r.RegistryPlainHTTP, content)
		if err != nil {
			return nil, err
		}
		for _, component := range initOpts.Components {
			initSpec.Components = append(initSpec.Components, deployspec.Component{
				Name:   component.Name,
				Flavor: component.Flavor,
			})
		}
		initRaw, err := initSpec.Marshal()
		if err != nil {
			return nil, err
		}
		files[deployspec.InitFileName] = initRaw
	}
	return files, nil
}

// buildSpec assembles one deploy-spec document from a source's fields, rendering its values and
// set entries against the target cluster's object content.
func buildSpec(packageRef string, api addonsv1alpha1.ZarfPackageAPI, values *apiextensionsv1.JSON, set map[string]string, plainHTTP bool, content map[string]any) (deployspec.Spec, error) {
	spec := deployspec.Spec{
		Package:    packageRef,
		PackageAPI: string(api),
		PlainHTTP:  plainHTTP,
	}
	if values != nil {
		rendered, err := deployspec.Render(string(values.Raw), content)
		if err != nil {
			return deployspec.Spec{}, err
		}
		spec.Values = rendered
	}
	if len(set) > 0 {
		spec.Set = map[string]string{}
		for key, value := range set {
			rendered, err := deployspec.Render(value, content)
			if err != nil {
				return deployspec.Spec{}, err
			}
			spec.Set[key] = rendered
		}
	}
	return spec, nil
}

// ensureSpecSecret writes the release's deploy-spec Secret, controller-owned by the release so
// it garbage-collects with it, updating in place only when the rendering changed.
func (r *ZarfPackageProxyReconciler) ensureSpecSecret(ctx context.Context, release *addonsv1alpha1.ZarfPackageRelease, files map[string][]byte) error {
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
		secret.Data = files
		if err := controllerutil.SetControllerReference(release, secret, r.Scheme()); err != nil {
			return err
		}
		return r.Create(ctx, secret)
	case err != nil:
		return err
	}
	if maps.EqualFunc(existing.Data, files, bytes.Equal) {
		return nil
	}
	before := existing.DeepCopy()
	existing.Data = files
	return r.Patch(ctx, existing, client.MergeFrom(before))
}
