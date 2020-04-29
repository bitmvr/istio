// Copyright 2019 Istio Authors
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

package helmreconciler

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"istio.io/istio/operator/pkg/cache"
	"istio.io/istio/operator/pkg/name"
	"istio.io/istio/operator/pkg/object"
	"istio.io/istio/operator/pkg/util"
)

const (
	// MetadataNamespace is the namespace for mesh metadata (labels, annotations)
	MetadataNamespace = "install.operator.istio.io"

	// OwnerNameKey represents the name of the owner to which the resource relates
	OwnerNameKey = MetadataNamespace + "/owner-name"
)

var (
	// ordered by which types should be deleted, first to last
	namespacedResources = []schema.GroupVersionKind{
		{Group: "autoscaling", Version: "v2beta1", Kind: "HorizontalPodAutoscaler"},
		{Group: "policy", Version: "v1beta1", Kind: "PodDisruptionBudget"},
		{Group: "apps", Version: "v1", Kind: "Deployment"},
		{Group: "apps", Version: "v1", Kind: "DaemonSet"},
		{Group: "", Version: "v1", Kind: "Service"},
		{Group: "", Version: "v1", Kind: "ConfigMap"},
		{Group: "", Version: "v1", Kind: "PersistentVolumeClaim"},
		{Group: "", Version: "v1", Kind: "Pod"},
		{Group: "", Version: "v1", Kind: "Secret"},
		{Group: "", Version: "v1", Kind: "ServiceAccount"},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding"},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "Role"},
		{Group: "networking.istio.io", Version: "v1alpha3", Kind: "DestinationRule"},
		{Group: "networking.istio.io", Version: "v1alpha3", Kind: "EnvoyFilter"},
		{Group: "networking.istio.io", Version: "v1alpha3", Kind: "Gateway"},
		{Group: "networking.istio.io", Version: "v1alpha3", Kind: "VirtualService"},
		{Group: "security.istio.io", Version: "v1beta1", Kind: "PeerAuthentication"},
	}

	// ordered by which types should be deleted, first to last
	nonNamespacedResources = []schema.GroupVersionKind{
		{Group: "admissionregistration.k8s.io", Version: "v1beta1", Kind: "MutatingWebhookConfiguration"},
		{Group: "admissionregistration.k8s.io", Version: "v1beta1", Kind: "ValidatingWebhookConfiguration"},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRoleBinding"},
		// Cannot currently prune CRDs because this will also wipe out user config.
		// {Group: "apiextensions.k8s.io", Version: "v1beta1", Kind: "CustomResourceDefinition"},
	}
)

// Prune removes any resources not specified in manifests generated by HelmReconciler h.
func (h *HelmReconciler) Prune(manifests name.ManifestMap) error {
	return h.runForAllTypes(func(labels map[string]string, objects *unstructured.UnstructuredList) error {
		var errs util.Errors
		for cname, manifest := range manifests.Consolidated() {
			errs = util.AppendErr(errs, h.pruneUnlistedResources(object.AllObjectHashes(manifest), labels, cname, objects))
		}
		return errs.ToError()
	})
}

// pruneAllTypes will collect all existing resource types we care about. For each type, the callback function
// will be called with the labels used to select this type, and all objects.
// This is in internal function meant to support prune and delete
func (h *HelmReconciler) runForAllTypes(callback func(labels map[string]string, objects *unstructured.UnstructuredList) error) error {
	var errs util.Errors
	// Ultimately, we want to prune based on component labels. Each of these share a common set of labels
	// Rather than do N List() calls for each component, we will just filter for the common subset here
	// and each component will do its own filtering
	// Because we are filtering by the core labels, List() will only return items that some components will care
	// about, so we are not querying for an overly broad set of resources.
	labels, err := h.getCoreOwnerLabels()
	if err != nil {
		return err
	}
	selector := klabels.Set(labels).AsSelectorPreValidated()
	for _, gvk := range append(namespacedResources, nonNamespacedResources...) {
		// First, we collect all objects for the provided GVK
		objects := &unstructured.UnstructuredList{}
		objects.SetGroupVersionKind(gvk)
		componentRequirement, err := klabels.NewRequirement(istioComponentLabelStr, selection.Exists, nil)
		if err != nil {
			return err
		}
		selector = selector.Add(*componentRequirement)
		if err := h.client.List(context.TODO(), objects, client.MatchingLabelsSelector{Selector: selector}, client.InNamespace(h.iop.Namespace)); err != nil {
			// we only want to retrieve resources clusters
			scope.Warnf("retrieving resources to prune type %s: %s not found", gvk.String(), err)
			continue
		}

		errs = util.AppendErr(errs, callback(labels, objects))
	}
	return errs.ToError()
}

// Delete removes all resources associated with componentName.
func (h *HelmReconciler) DeleteComponent(componentName string) error {
	return h.runForAllTypes(func(labels map[string]string, objects *unstructured.UnstructuredList) error {
		return h.pruneUnlistedResources(map[string]bool{}, labels, componentName, objects)
	})
}

// pruneUnlistedResources prunes any resources from the given component that are not in the excluded map. Resource
// labels are used to identify the resources belonging to the component.
func (h *HelmReconciler) pruneUnlistedResources(excluded map[string]bool, coreLabels map[string]string,
	componentName string, objects *unstructured.UnstructuredList) error {
	var errs util.Errors
	labels := h.addComponentLabels(coreLabels, componentName)
	selector := klabels.Set(labels).AsSelectorPreValidated()
	for _, o := range objects.Items {
		oh := object.NewK8sObject(&o, nil, nil).Hash()
		// Label mismatch. Provided objects don't select against the component, so this likely means the object
		// is for another component.
		if !selector.Matches(klabels.Set(o.GetLabels())) {
			continue
		}
		if excluded[oh] {
			continue
		}
		h.opts.Log.LogAndPrintf("howardjohn: object %v not excluded: %v", o.GetName(), excluded)
		if h.opts.DryRun {
			h.opts.Log.LogAndPrintf("Not pruning object %s because of dry run.", oh)
			continue
		}

		err := h.client.Delete(context.TODO(), &o, client.PropagationPolicy(metav1.DeletePropagationBackground))
		if err != nil {
			errs = util.AppendErr(errs, err)
		}
		h.removeFromObjectCache(componentName, oh)
		h.opts.Log.LogAndPrintf("Pruned object %s.", oh)
	}

	return errs.ToError()
}

// RemoveObject removes object with objHash in componentName from the object cache.
func (h *HelmReconciler) removeFromObjectCache(componentName, objHash string) {
	crHash, err := h.getCRHash(componentName)
	if err != nil {
		scope.Error(err.Error())
	}
	cache.RemoveObject(crHash, objHash)
	scope.Infof("Removed object %s from Cache.", objHash)
}