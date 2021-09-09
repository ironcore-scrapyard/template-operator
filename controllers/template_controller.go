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
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/Masterminds/sprig"
	yaml2 "github.com/ghodss/yaml"
	"github.com/go-logr/logr"
	templatev1alpha1 "github.com/onmetal/template-operator/api/v1alpha1"
	"github.com/onmetal/template-operator/pkg/source"
	"io"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"strings"
	"sync"
	gotemplate "text/template"
	"time"
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

	parentGVKUsage *gvkUsageMap
	parents        *source.Dynamic

	childGVKUsage *gvkUsageMap
	children      *source.Dynamic
}

func mkObjRef(obj client.Object) templatev1alpha1.ObjectReference {
	gvk := obj.GetObjectKind().GroupVersionKind()
	return templatev1alpha1.ObjectReference{
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
		Namespace:  obj.GetNamespace(),
		Name:       obj.GetName(),
	}
}

func mkManagedObjRefs(objs []unstructured.Unstructured) []templatev1alpha1.ObjectReference {
	if len(objs) == 0 {
		return nil
	}
	res := make([]templatev1alpha1.ObjectReference, 0, len(objs))
	for _, obj := range objs {
		res = append(res, mkObjRef(&obj))
	}
	return res
}

func gvkFromObjRef(ref templatev1alpha1.ObjectReference) (schema.GroupVersionKind, error) {
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil {
		return schema.GroupVersionKind{}, nil
	}

	return gv.WithKind(ref.Kind), nil
}

// recursion is the maximum allowed number of recursion inside a template.
const recursionMaxNums = 1000

// toYAML takes an interface, marshals it to yaml, and returns a string. It will
// always return a string, even on marshal error (empty string).
//
// This is designed to be called from a template.
func toYAML(v interface{}) string {
	data, err := yaml2.Marshal(v)
	if err != nil {
		// Swallow errors inside of a template.
		return ""
	}
	return strings.TrimSuffix(string(data), "\n")
}

// fromYAML converts a YAML document into a map[string]interface{}.
//
// This is not a general-purpose YAML parser, and will not parse all valid
// YAML documents. Additionally, because its intended use is within templates
// it tolerates errors. It will insert the returned error message string into
// m["Error"] in the returned map.
func fromYAML(str string) map[string]interface{} {
	m := map[string]interface{}{}

	if err := yaml2.Unmarshal([]byte(str), &m); err != nil {
		m["Error"] = err.Error()
	}
	return m
}

func include(t *gotemplate.Template) func(name string, data interface{}) (string, error) {
	includedNames := make(map[string]int)
	return func(name string, data interface{}) (string, error) {
		var buf strings.Builder
		if v, ok := includedNames[name]; ok {
			if v > recursionMaxNums {
				return "", fmt.Errorf("unable to execute template: %w",
					fmt.Errorf("rendering template has a nested reference name: %s", name))
			}
			includedNames[name]++
		} else {
			includedNames[name] = 1
		}
		err := t.ExecuteTemplate(&buf, name, data)
		includedNames[name]--
		return buf.String(), err
	}
}

func funcMap(t *gotemplate.Template) gotemplate.FuncMap {
	f := sprig.TxtFuncMap()
	f["include"] = include(t)
	f["fromYaml"] = fromYAML
	f["toYaml"] = toYAML
	delete(f, "env")
	delete(f, "expandenv")
	return f
}

//+kubebuilder:rbac:groups=template.onmetal.de,resources=templates,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=template.onmetal.de,resources=templates/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=template.onmetal.de,resources=templates/finalizers,verbs=update

// Reconcile reconciles the template, potentially producing additional resources.
func (r *TemplateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	template := &templatev1alpha1.Template{}
	if err := r.Get(ctx, req.NamespacedName, template); err != nil {
		if apierrors.IsNotFound(err) {
			if err := r.deleteWatches(req.NamespacedName); err != nil {
				logger.Error(err, "Could not clean up watches for key", "key", req.NamespacedName)
			}
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !template.DeletionTimestamp.IsZero() {
		return r.delete(ctx, logger, template)
	}
	return r.reconcile(ctx, logger, template)
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
		for _, template := range templates.Items {
			ids.Insert(client.ObjectKeyFromObject(&template))
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
			for _, template := range templates.Items {
				for _, src := range template.Spec.Sources {
					if src.Object != nil {
						srcGVK, err := gvkFromObjRef(*src.Object)
						if err != nil {
							logger.Error(err, "Object has invalid api version.")
							continue
						}

						if gvk == srcGVK && src.Object.Namespace == object.GetNamespace() && src.Object.Name == object.GetName() {
							keySet[client.ObjectKeyFromObject(&template)] = struct{}{}
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

func (r *TemplateReconciler) resolveSources(ctx context.Context, logger logr.Logger, template *templatev1alpha1.Template) (map[string]interface{}, error) {
	values := make(map[string]interface{})
	for _, src := range template.Spec.Sources {
		if _, ok := values[src.Name]; ok {
			return nil, fmt.Errorf("source name %q is already in use", src.Name)
		}

		switch {
		case src.Object != nil:
			u := &unstructured.Unstructured{}
			u.SetAPIVersion(src.Object.APIVersion)
			u.SetKind(src.Object.Kind)
			key := client.ObjectKey{Namespace: src.Object.Namespace, Name: src.Object.Name}
			logger.V(2).Info("Resolving object reference.", "ref", mkObjRef(u))
			if err := r.Get(ctx, key, u); err != nil {
				return nil, fmt.Errorf("could not fetch object with key %s: %w", key, err)
			}

			values[src.Name] = u.Object
		default:
			return nil, fmt.Errorf("invalid source definition %q", src.Name)
		}
	}

	return values, nil
}

func (r *TemplateReconciler) executeTemplate(logger logr.Logger, template *templatev1alpha1.Template, data interface{}) (io.Reader, error) {
	logger.V(1).Info("Creating / Parsing template")
	tmpl := gotemplate.New(fmt.Sprintf("%s/%s", template.Namespace, template.Name))
	fMap := funcMap(tmpl)
	tmpl.Funcs(fMap)

	var err error
	tmpl, err = tmpl.Parse(template.Spec.Data.Inline)
	if err != nil {
		return nil, fmt.Errorf("invalid template: %w", err)
	}

	logger.V(1).Info("Executing template")
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("error executing template: %w", err)
	}

	return &buf, nil
}

func (r *TemplateReconciler) buildObjects(in io.Reader) ([]unstructured.Unstructured, error) {
	var objs []unstructured.Unstructured
	rd := yaml.NewYAMLReader(bufio.NewReader(in))
	for {
		data, err := rd.Read()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}

		u := &unstructured.Unstructured{}
		if _, _, err := scheme.Codecs.UniversalDeserializer().Decode(data, nil, u); err != nil {
			return nil, fmt.Errorf("error decoding object: %w", err)
		}

		objs = append(objs, *u)
	}

	return objs, nil
}

type gkSet map[metav1.GroupKind]struct{}

func newGKSet(gks []metav1.GroupKind) gkSet {
	s := make(gkSet)
	for _, gk := range gks {
		s[gk] = struct{}{}
	}
	return s
}

func (s gkSet) Has(gk metav1.GroupKind) bool {
	_, ok := s[gk]
	return ok
}

func (r *TemplateReconciler) prepareObjects(template *templatev1alpha1.Template, objs []unstructured.Unstructured) error {
	gks := newGKSet(template.Spec.GroupKinds)
	sel, err := metav1.LabelSelectorAsSelector(template.Spec.Selector)
	if err != nil {
		return fmt.Errorf("could not parse template label selector: %w", err)
	}

	for _, obj := range objs {
		if obj.GetGenerateName() != "" {
			return fmt.Errorf("generate name is not allowed on target objects")
		}
		if obj.GetName() == "" {
			return fmt.Errorf("object has no name")
		}

		gvk, err := apiutil.GVKForObject(&obj, r.Scheme)
		if err != nil {
			return fmt.Errorf("could not determine gvk for object: %w", err)
		}

		if !gks.Has(metav1.GroupKind{Group: gvk.Group, Kind: gvk.Kind}) {
			return fmt.Errorf("object group kind %v not contained in group kinds", gvk.GroupKind())
		}

		lbls := obj.GetLabels()
		for k, v := range template.Spec.CommonLabels {
			if actualV, ok := lbls[k]; ok && actualV != v {
				return fmt.Errorf("object label mismatch: %s=%s but should be %s=%s", k, actualV, k, v)
			}
			if lbls == nil {
				lbls = make(map[string]string)
			}
			lbls[k] = v
		}
		obj.SetLabels(lbls)

		if !sel.Matches(labels.Set(lbls)) {
			return fmt.Errorf("labels of object do not match selector")
		}

		if err := controllerutil.SetOwnerReference(template, &obj, r.Scheme); err != nil {
			return fmt.Errorf("could not set owner reference on object: %w", err)
		}
	}
	return nil
}

func (r *TemplateReconciler) applyObjects(ctx context.Context, logger logr.Logger, objs []unstructured.Unstructured) error {
	for _, obj := range objs {
		logger.V(2).Info("Applying object", "ref", mkObjRef(&obj))
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
	managedResources []templatev1alpha1.ObjectReference,
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
			gvk, err := gvkFromObjRef(*src.Object)
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

func (r *TemplateReconciler) resolveTemplateData(ctx context.Context, logger logr.Logger, template *templatev1alpha1.Template) (interface{}, error) {
	values, err := r.resolveSources(ctx, logger, template)
	if err != nil {
		return nil, err
	}

	u := &unstructured.Unstructured{}
	if err := r.Scheme.Convert(template, u, nil); err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"Values":   values,
		"Template": u.Object,
	}, nil
}

func (r *TemplateReconciler) reconcile(ctx context.Context, logger logr.Logger, template *templatev1alpha1.Template) (ctrl.Result, error) {
	logger.V(1).Info("Setting up watches.")
	if err := r.applyWatches(template); err != nil {
		if err := r.updateStatus(ctx, template,
			nil,
			corev1.ConditionFalse,
			"CannotApplyWatches",
			fmt.Sprintf("Watching objects resulted in an error: %v", err),
		); err != nil {
			logger.Error(err, "Error updating status.")
		}
		return ctrl.Result{}, fmt.Errorf("could not watch objects: %w", err)
	}

	logger.V(1).Info("Resolving template data.")
	data, err := r.resolveTemplateData(ctx, logger, template)
	if err != nil {
		if err := r.updateStatus(ctx, template,
			nil,
			corev1.ConditionFalse,
			"CannotResolveData",
			fmt.Sprintf("Resolving the template data resulted in an error: %v", err),
		); err != nil {
			logger.Error(err, "Error updating status.")
		}
		return ctrl.Result{}, fmt.Errorf("could not resolve template data: %w", err)
	}

	logger.V(1).Info("Executing template.")
	rd, err := r.executeTemplate(logger, template, data)
	if err != nil {
		if err := r.updateStatus(ctx, template,
			nil,
			corev1.ConditionFalse,
			"InvalidTemplate",
			fmt.Sprintf("Executing the template resulted in an error: %v", err),
		); err != nil {
			logger.Error(err, "Error updating status.")
		}
		return ctrl.Result{}, fmt.Errorf("error executing template: %w", err)
	}

	logger.V(1).Info("Building target objects.")
	objs, err := r.buildObjects(rd)
	if err != nil {
		if err := r.updateStatus(ctx, template,
			nil,
			corev1.ConditionFalse,
			"InvalidObject",
			fmt.Sprintf("Building object for applying resulted in an error: %v", err),
		); err != nil {
			logger.Error(err, "Error updating status.")
		}
		return ctrl.Result{}, fmt.Errorf("error building objects: %w", err)
	}

	logger.V(1).Info("Preparing objects.")
	if err := r.prepareObjects(template, objs); err != nil {
		if err := r.updateStatus(ctx, template,
			nil,
			corev1.ConditionFalse,
			"UnmanageableObject",
			fmt.Sprintf("Updating objects for management resulted in an error: %v", err),
		); err != nil {
			logger.Error(err, "Error updating status.")
		}
		return ctrl.Result{}, fmt.Errorf("error preparing objects: %w", err)
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
		mkManagedObjRefs(objs),
		corev1.ConditionTrue,
		"Successful",
		"All objects have been successfully applied.",
	); err != nil {
		return ctrl.Result{}, fmt.Errorf("error updating template status: %w", err)
	}
	return ctrl.Result{}, nil
}
