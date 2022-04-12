/*
 * Copyright (c) 2021 by the OnMetal authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package controllers

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/go-logr/logr"
	templatev1alpha1 "github.com/onmetal/template-operator/api/v1alpha1"
	"github.com/onmetal/template-operator/pkg/source"
	"github.com/onmetal/template-operator/pkg/template"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type empty struct{}

type idSet map[client.ObjectKey]empty

func newIDSet(keys ...client.ObjectKey) idSet {
	s := make(idSet)
	s.Insert(keys...)
	return s
}

func (s idSet) Insert(keys ...client.ObjectKey) {
	for _, key := range keys {
		s[key] = empty{}
	}
}

func (s idSet) Delete(items ...client.ObjectKey) {
	for _, item := range items {
		delete(s, item)
	}
}

func (s idSet) Has(key client.ObjectKey) bool {
	_, ok := s[key]
	return ok
}

func (s idSet) Difference(other idSet) idSet {
	res := newIDSet()
	for item := range s {
		if !other.Has(item) {
			res.Insert(item)
		}
	}
	return res
}

type gvkSet map[schema.GroupVersionKind]empty

func newGVKSet(items ...schema.GroupVersionKind) gvkSet {
	s := make(gvkSet)
	s.Insert(items...)
	return s
}

func (s gvkSet) Insert(items ...schema.GroupVersionKind) {
	for _, item := range items {
		s[item] = empty{}
	}
}

func (s gvkSet) Delete(items ...schema.GroupVersionKind) {
	for _, item := range items {
		delete(s, item)
	}
}

func (s gvkSet) Has(item schema.GroupVersionKind) bool {
	_, ok := s[item]
	return ok
}

type gvkUsageMap struct {
	gvkToIDs map[schema.GroupVersionKind]idSet
	idToGVKs map[client.ObjectKey]gvkSet
}

func (m *gvkUsageMap) ids() idSet {
	res := newIDSet()
	for id := range m.idToGVKs {
		res.Insert(id)
	}
	return res
}

func (m *gvkUsageMap) apply(id client.ObjectKey, gvks gvkSet) (added, deleted []schema.GroupVersionKind) {
	// Determine if there are any gvks that are not used by any template anymore.
	for _, oldGVKs := range m.idToGVKs {
		for oldGVK := range oldGVKs {
			// If the oldGVK is present among the new ones it means that it is still in use.
			if gvks.Has(oldGVK) {
				continue
			}

			gvkIDs := m.gvkToIDs[oldGVK]
			gvkIDs.Delete(id)
			if len(gvkIDs) == 0 {
				delete(m.gvkToIDs, oldGVK)
				deleted = append(deleted, oldGVK)
			}
		}
	}

	// After determining old / unneeded gvks, we can now check what wasn't present yet and add it.
	idGVKs := m.idToGVKs[id]
	for gvk := range gvks {
		if idGVKs.Has(gvk) {
			// The gvk was already in use, nothing new was added.
			continue
		}

		gvkIDs := m.gvkToIDs[gvk]
		if gvkIDs == nil {
			added = append(added, gvk)
			gvkIDs = newIDSet()
		}
		gvkIDs.Insert(id)
		m.gvkToIDs[gvk] = gvkIDs
	}
	return
}

func mkGVKUsageMap() *gvkUsageMap {
	return &gvkUsageMap{
		gvkToIDs: make(map[schema.GroupVersionKind]idSet),
		idToGVKs: make(map[client.ObjectKey]gvkSet),
	}
}

// TemplateReconciler reconciles a Template object
type TemplateReconciler struct {
	mu sync.Mutex

	FieldOwner string
	client.Client
	Scheme           *runtime.Scheme
	RESTMapper       meta.RESTMapper
	PruneWatchPeriod time.Duration

	engine *template.Engine

	parentGVKUsage *gvkUsageMap
	parents        *source.Dynamic

	childGVKUsage *gvkUsageMap
	children      *source.Dynamic
}

func mkLocalObjRef(obj client.Object) templatev1alpha1.LocalObjectReference {
	gvk := obj.GetObjectKind().GroupVersionKind()
	return templatev1alpha1.LocalObjectReference{
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
		Name:       obj.GetName(),
	}
}

func mkManagedLocalObjRefs(objs []unstructured.Unstructured) []templatev1alpha1.LocalObjectReference {
	if len(objs) == 0 {
		return nil
	}
	res := make([]templatev1alpha1.LocalObjectReference, 0, len(objs))
	for _, obj := range objs {
		res = append(res, mkLocalObjRef(&obj))
	}
	return res
}

func gvkFromLocalObjRef(ref templatev1alpha1.LocalObjectReference) (schema.GroupVersionKind, error) {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return schema.GroupVersionKind{}, nil
	}

	return gv.WithKind(ref.Kind), nil
}

//+kubebuilder:rbac:groups=template.onmetal.de,resources=templates,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=template.onmetal.de,resources=templates/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=template.onmetal.de,resources=templates/finalizers,verbs=update

// Reconcile reconciles the template, potentially producing additional resources.
func (r *TemplateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	tmpl := &templatev1alpha1.Template{}
	if err := r.Get(ctx, req.NamespacedName, tmpl); err != nil {
		if apierrors.IsNotFound(err) {
			if err := r.deleteWatches(req.NamespacedName); err != nil {
				logger.Error(err, "Could not clean up watches for key", "key", req.NamespacedName)
			}
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !tmpl.DeletionTimestamp.IsZero() {
		return r.delete(ctx, logger, tmpl)
	}
	return r.reconcile(ctx, logger, tmpl)
}

func (r *TemplateReconciler) pruneWatches(ctx context.Context) error {
	wait.UntilWithContext(ctx, func(ctx context.Context) {
		logger := log.FromContext(ctx).WithName("prune-watches")

		templates := &templatev1alpha1.TemplateList{}
		if err := r.List(ctx, templates); err != nil {
			logger.Error(err, "Could not list templates")
			return
		}

		r.mu.Lock()
		defer r.mu.Unlock()

		ids := newIDSet()
		for _, tmpl := range templates.Items {
			ids.Insert(client.ObjectKeyFromObject(&tmpl))
		}

		unusedParentIDs := r.parentGVKUsage.ids().Difference(ids)
		for unusedParentID := range unusedParentIDs {
			if err := r.deleteParentWatches(unusedParentID); err != nil {
				logger.Error(err, "Error pruning parent id", "parentID", unusedParentID)
				return
			}
		}

		unusedChildIDs := r.childGVKUsage.ids().Difference(ids)
		for unusedChildID := range unusedChildIDs {
			if err := r.deleteChildWatches(unusedChildID); err != nil {
				logger.Error(err, "Error pruning child id", "parentID", unusedChildID)
				return
			}
		}
	}, r.PruneWatchPeriod)
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *TemplateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	ctx := context.Background()
	logger := mgr.GetLogger().WithName("TemplateReconciler.SetupWithManager")

	r.engine = template.NewEngine(r.Client, r.Scheme)

	var err error
	r.parents, err = source.NewDynamic(source.DynamicOptions{
		Cache:  mgr.GetCache(),
		Scheme: r.Scheme,
	})
	if err != nil {
		return fmt.Errorf("error creating parent dynamic source: %w", err)
	}
	r.parentGVKUsage = mkGVKUsageMap()

	r.children, err = source.NewDynamic(source.DynamicOptions{
		Cache:  mgr.GetCache(),
		Scheme: r.Scheme,
	})
	if err != nil {
		return fmt.Errorf("error creating child dynamic source: %w", err)
	}
	r.childGVKUsage = mkGVKUsageMap()

	if r.PruneWatchPeriod == 0 {
		r.PruneWatchPeriod = 20 * time.Minute
	}
	if err := mgr.Add(manager.RunnableFunc(r.pruneWatches)); err != nil {
		return fmt.Errorf("could not setup watch-pruning")
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&templatev1alpha1.Template{}).
		Watches(r.parents, handler.EnqueueRequestsFromMapFunc(func(object client.Object) []reconcile.Request {
			gvk, err := apiutil.GVKForObject(object, r.Scheme)
			if err != nil {
				logger.Error(err, "Error obtaining gvk for object.")
				return nil
			}

			templates := &templatev1alpha1.TemplateList{}
			if err := r.List(ctx, templates); err != nil {
				logger.Error(err, "Error listing templates.")
				return nil
			}

			keySet := map[client.ObjectKey]struct{}{}
			for _, tmpl := range templates.Items {
				for _, src := range tmpl.Spec.Sources {
					if src.Object != nil {
						srcGVK, err := gvkFromLocalObjRef(*src.Object)
						if err != nil {
							logger.Error(err, "Object has invalid api version.")
							continue
						}

						if gvk == srcGVK && tmpl.Namespace == object.GetNamespace() && src.Object.Name == object.GetName() {
							keySet[client.ObjectKeyFromObject(&tmpl)] = struct{}{}
						}
					}
				}
			}

			requests := make([]reconcile.Request, 0, len(keySet))
			for key := range keySet {
				requests = append(requests, reconcile.Request{NamespacedName: key})
			}
			return requests
		}), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(r.children, &handler.EnqueueRequestForOwner{
			OwnerType: &templatev1alpha1.Template{},
		}).
		Complete(r)
}

func (r *TemplateReconciler) delete(ctx context.Context, logger logr.Logger, template *templatev1alpha1.Template) (ctrl.Result, error) {
	logger.V(1).Info("Deleting watches.")
	if err := r.deleteWatches(client.ObjectKeyFromObject(template)); err != nil {
		return ctrl.Result{}, fmt.Errorf("error deleting watches: %w", err)
	}

	return ctrl.Result{}, nil
}

func (r *TemplateReconciler) applyObjects(ctx context.Context, logger logr.Logger, objs []unstructured.Unstructured) error {
	for _, obj := range objs {
		logger.V(2).Info("Applying object", "ref", mkLocalObjRef(&obj))
		if err := r.Patch(ctx, &obj, client.Apply, client.FieldOwner(r.FieldOwner)); err != nil {
			return fmt.Errorf("error applying object: %w", err)
		}
	}
	return nil
}

func (r *TemplateReconciler) pruneObjects(ctx context.Context, logger logr.Logger, template *templatev1alpha1.Template, managedObjs []unstructured.Unstructured) error {
	sel, err := metav1.LabelSelectorAsSelector(template.Spec.Selector)
	if err != nil {
		return fmt.Errorf("could not create label selector: %w", err)
	}

	for _, gk := range template.Spec.GroupKinds {
		mapping, err := r.RESTMapper.RESTMapping(schema.GroupKind{Group: gk.Group, Kind: gk.Kind})
		if err != nil {
			return fmt.Errorf("could not determine rest mapping for group kind %v: %w", gk, err)
		}

		list := &unstructured.UnstructuredList{}
		list.SetAPIVersion(mapping.GroupVersionKind.GroupVersion().String())
		list.SetKind(mapping.GroupVersionKind.Kind)

		opts := []client.ListOption{client.MatchingLabelsSelector{Selector: sel}}
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			opts = append(opts, client.InNamespace(template.Namespace))
		}

		if err := r.List(ctx, list, opts...); err != nil {
			return fmt.Errorf("could not list %v objects: %w", gk, err)
		}

	PruneLoop:
		for _, actualObj := range list.Items {
			for _, managedObj := range managedObjs {
				managedGVK, err := apiutil.GVKForObject(&managedObj, r.Scheme)
				if err != nil {
					return fmt.Errorf("could not determine gvk for object: %w", err)
				}

				managedGK := metav1.GroupKind{Group: managedGVK.Group, Kind: managedGVK.Kind}
				if gk == managedGK &&
					actualObj.GetNamespace() == managedObj.GetNamespace() &&
					actualObj.GetName() == managedObj.GetName() {
					continue PruneLoop
				}
			}

			logger.Info("Pruning object", "groupKind", gk, "key", client.ObjectKeyFromObject(&actualObj))
			if err := r.Delete(ctx, &actualObj); err != nil {
				return fmt.Errorf("error pruning object: %w", err)
			}
		}
	}
	return nil
}

func findCondition(conditions []templatev1alpha1.TemplateCondition, typ templatev1alpha1.TemplateConditionType) (templatev1alpha1.TemplateCondition, bool) {
	for _, condition := range conditions {
		if condition.Type == typ {
			return condition, true
		}
	}
	return templatev1alpha1.TemplateCondition{}, false
}

func (r *TemplateReconciler) updateStatus(
	ctx context.Context,
	template *templatev1alpha1.Template,
	managedResources []templatev1alpha1.LocalObjectReference,
	status corev1.ConditionStatus,
	reason, message string,
) error {
	now := metav1.Now()

	previousCond, _ := findCondition(template.Status.Conditions, templatev1alpha1.TemplateApplied)
	var lastTransitionTime metav1.Time
	if previousCond.Status != status {
		lastTransitionTime = metav1.Now()
	} else {
		lastTransitionTime = previousCond.LastTransitionTime
	}

	template.Status = templatev1alpha1.TemplateStatus{
		ManagedResources: managedResources,
		Conditions: []templatev1alpha1.TemplateCondition{
			{
				Type:               templatev1alpha1.TemplateApplied,
				Status:             status,
				Reason:             reason,
				Message:            message,
				LastUpdateTime:     now,
				LastTransitionTime: lastTransitionTime,
				ObservedGeneration: template.Generation,
			},
		},
	}
	if err := r.Client.Status().Update(ctx, template); err != nil {
		return fmt.Errorf("error updating status: %w", err)
	}
	return nil
}

func applyWatches(src *source.Dynamic, added, removed []schema.GroupVersionKind) error {
	for _, addition := range added {
		if err := src.AddWatchForKind(addition); err != nil {
			return fmt.Errorf("could not add watch: %w", err)
		}
	}
	for _, removal := range removed {
		if err := src.RemoveWatchForKind(removal); err != nil {
			return fmt.Errorf("could not remove watch: %w", err)
		}
	}
	return nil
}

func (r *TemplateReconciler) determineUsedParentGVKs(template *templatev1alpha1.Template) (gvkSet, error) {
	usedGVKs := newGVKSet()
	for _, src := range template.Spec.Sources {
		switch {
		case src.Object != nil:
			gvk, err := gvkFromLocalObjRef(*src.Object)
			if err != nil {
				return nil, fmt.Errorf("could not determine gvk for source %v: %w", src.Object, err)
			}

			usedGVKs.Insert(gvk)
		}
	}

	return usedGVKs, nil
}

func (r *TemplateReconciler) applyParentWatches(template *templatev1alpha1.Template) error {
	usedGVKs, err := r.determineUsedParentGVKs(template)
	if err != nil {
		return fmt.Errorf("error determining used parent gvks: %w", err)
	}

	added, removed := r.parentGVKUsage.apply(client.ObjectKeyFromObject(template), usedGVKs)

	if err := applyWatches(r.parents, added, removed); err != nil {
		return fmt.Errorf("could not apply parent watches: %w", err)
	}
	return nil
}

func (r *TemplateReconciler) deleteParentWatches(key client.ObjectKey) error {
	added, removed := r.parentGVKUsage.apply(key, nil)

	if err := applyWatches(r.parents, added, removed); err != nil {
		return fmt.Errorf("could not apply parent watches: %w", err)
	}
	return nil
}

func (r *TemplateReconciler) determineUsedChildGVKs(template *templatev1alpha1.Template) (gvkSet, error) {
	usedGVKs := newGVKSet()
	for _, gk := range template.Spec.GroupKinds {
		mapping, err := r.RESTMapper.RESTMapping(schema.GroupKind{Group: gk.Group, Kind: gk.Kind})
		if err != nil {
			return nil, fmt.Errorf("could not determine rest mapping for %v: %w", gk, err)
		}

		usedGVKs.Insert(schema.GroupVersionKind{
			Group:   gk.Group,
			Kind:    gk.Kind,
			Version: mapping.GroupVersionKind.Version,
		})
	}
	return usedGVKs, nil
}

func (r *TemplateReconciler) applyChildWatches(template *templatev1alpha1.Template) error {
	usedGVKs, err := r.determineUsedChildGVKs(template)
	if err != nil {
		return fmt.Errorf("error determine used child gvks: %w", err)
	}

	added, removed := r.childGVKUsage.apply(client.ObjectKeyFromObject(template), usedGVKs)

	if err := applyWatches(r.children, added, removed); err != nil {
		return fmt.Errorf("could not apply child watches: %w", err)
	}
	return nil
}

func (r *TemplateReconciler) deleteChildWatches(key client.ObjectKey) error {
	added, removed := r.childGVKUsage.apply(key, nil)

	if err := applyWatches(r.children, added, removed); err != nil {
		return fmt.Errorf("could not apply child watches: %w", err)
	}
	return nil
}

func (r *TemplateReconciler) applyWatches(template *templatev1alpha1.Template) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.applyParentWatches(template); err != nil {
		return fmt.Errorf("error applying parent watches: %w", err)
	}
	if err := r.applyChildWatches(template); err != nil {
		return fmt.Errorf("error applying child watches: %w", err)
	}

	return nil
}

func (r *TemplateReconciler) deleteWatches(key client.ObjectKey) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if err := r.deleteParentWatches(key); err != nil {
		return fmt.Errorf("error deleting parent watches: %w", err)
	}
	if err := r.deleteChildWatches(key); err != nil {
		return fmt.Errorf("error deleting child watches: %w", err)
	}

	return nil
}

func (r *TemplateReconciler) reconcile(ctx context.Context, logger logr.Logger, template *templatev1alpha1.Template) (ctrl.Result, error) {
	if err := r.applyWatches(template); err != nil {
		if err := r.updateStatus(ctx, template,
			nil,
			corev1.ConditionUnknown,
			"CannotApplyWatches",
			fmt.Sprintf("Applying watches resulted in an error: %v", err),
		); err != nil {
			logger.Error(err, "Error updating status.")
		}
		return ctrl.Result{}, fmt.Errorf("error applying watches: %w", err)
	}

	objs, err := r.engine.Render(ctx, template)
	if err != nil {
		if err := r.updateStatus(ctx, template,
			nil,
			corev1.ConditionUnknown,
			"CannotRender",
			fmt.Sprintf("Rendering the template resulted in an error: %v", err),
		); err != nil {
			logger.Error(err, "Error updating status.")
		}
		return ctrl.Result{}, fmt.Errorf("error applying objects: %w", err)
	}

	logger.V(1).Info("Applying objects.")
	if err := r.applyObjects(ctx, logger, objs); err != nil {
		if err := r.updateStatus(ctx, template,
			nil,
			corev1.ConditionUnknown,
			"UnknownApplyError",
			fmt.Sprintf("Applying the objects resulted in an error: %v", err),
		); err != nil {
			logger.Error(err, "Error updating status.")
		}
		return ctrl.Result{}, fmt.Errorf("error applying objects: %w", err)
	}

	if template.Spec.Prune {
		logger.V(1).Info("Pruning objects.")
		if err := r.pruneObjects(ctx, logger, template, objs); err != nil {
			if err := r.updateStatus(ctx, template,
				nil,
				corev1.ConditionUnknown,
				"ErrorPruning",
				fmt.Sprintf("Pruning the objects resulted in an error: %v", err),
			); err != nil {
				logger.Error(err, "Error updating status.")
			}
			return ctrl.Result{}, fmt.Errorf("error pruning objects: %w", err)
		}
	}

	logger.V(1).Info("Writing success status.")
	if err := r.updateStatus(ctx, template,
		mkManagedLocalObjRefs(objs),
		corev1.ConditionTrue,
		"Successful",
		"All objects have been successfully applied.",
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("error updating template status: %w", err)
	}
	return ctrl.Result{}, nil
}
