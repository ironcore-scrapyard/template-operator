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

package template

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
	"io"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"strings"
	gotemplate "text/template"
)

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

type Engine struct {
	client client.Client
	scheme *runtime.Scheme
}

func mkLocalObjRef(obj client.Object) templatev1alpha1.LocalObjectReference {
	gvk := obj.GetObjectKind().GroupVersionKind()
	return templatev1alpha1.LocalObjectReference{
		APIVersion: gvk.GroupVersion().String(),
		Kind:       gvk.Kind,
		Name:       obj.GetName(),
	}
}

func (e *Engine) resolveTemplateDefinition(ctx context.Context, template *templatev1alpha1.Template) (string, error) {
	dataSpec := template.Spec.Data
	switch {
	case dataSpec.Inline != "":
		return dataSpec.Inline, nil
	case dataSpec.ConfigMapRef != nil:
		configMap := &corev1.ConfigMap{}
		key := client.ObjectKey{Namespace: template.Namespace, Name: dataSpec.ConfigMapRef.Name}
		if err := e.client.Get(ctx, key, configMap); err != nil {
			return "", fmt.Errorf("error getting config map %s: %w", key, err)
		}

		dataKey := dataSpec.ConfigMapRef.Key
		if dataKey == "" {
			dataKey = templatev1alpha1.DefaultConfigMapTemplateKey
		}

		data, ok := configMap.Data[dataKey]
		if !ok || data == "" {
			return "", fmt.Errorf("config map %s has no data at %s", key, dataKey)
		}

		return data, nil
	default:
		return "", fmt.Errorf("invalid data spec: no valid source supplied")
	}
}

func (e *Engine) resolveSources(ctx context.Context, logger logr.Logger, template *templatev1alpha1.Template) (map[string]interface{}, error) {
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
			key := client.ObjectKey{Namespace: template.Namespace, Name: src.Object.Name}
			logger.V(2).Info("Resolving object reference.", "ref", mkLocalObjRef(u))
			if err := e.client.Get(ctx, key, u); err != nil {
				return nil, fmt.Errorf("could not fetch object with key %s: %w", key, err)
			}

			values[src.Name] = u.Object
		default:
			values[src.Name] = src.Value
		}
	}

	return values, nil
}

func (e *Engine) resolveTemplateData(ctx context.Context, logger logr.Logger, template *templatev1alpha1.Template) (interface{}, error) {
	values, err := e.resolveSources(ctx, logger, template)
	if err != nil {
		return nil, err
	}

	u := &unstructured.Unstructured{}
	if err := e.scheme.Convert(template, u, nil); err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"Values":   values,
		"Template": u.Object,
	}, nil
}

func (e *Engine) executeTemplate(logger logr.Logger, template *templatev1alpha1.Template, templateDefinition string, data interface{}) (io.Reader, error) {
	logger.V(1).Info("Creating / Parsing template")
	tmpl := gotemplate.New(fmt.Sprintf("%s/%s", template.Namespace, template.Name))
	fMap := funcMap(tmpl)
	tmpl.Funcs(fMap)

	var err error
	tmpl, err = tmpl.Parse(templateDefinition)
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

func (e *Engine) parseObjects(in io.Reader) ([]unstructured.Unstructured, error) {
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

func (e *Engine) prepareObjects(template *templatev1alpha1.Template, objs []unstructured.Unstructured) error {
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

		gvk, err := apiutil.GVKForObject(&obj, e.scheme)
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

		if err := controllerutil.SetOwnerReference(template, &obj, e.scheme); err != nil {
			return fmt.Errorf("could not set owner reference on object: %w", err)
		}
	}
	return nil
}

func (e *Engine) Render(ctx context.Context, template *templatev1alpha1.Template) ([]unstructured.Unstructured, error) {
	log := ctrl.LoggerFrom(ctx)
	log.V(1).Info("Resolving template data.")
	data, err := e.resolveTemplateData(ctx, log, template)
	if err != nil {
		return nil, err
	}

	log.V(1).Info("Resolving template definition.")
	templateDefinition, err := e.resolveTemplateDefinition(ctx, template)
	if err != nil {
		return nil, err
	}

	log.V(1).Info("Executing template.")
	rd, err := e.executeTemplate(log, template, templateDefinition, data)
	if err != nil {
		return nil, err
	}

	log.V(1).Info("Parsing objects.")
	objs, err := e.parseObjects(rd)
	if err != nil {
		return nil, err
	}

	log.V(1).Info("Preparing objects.")
	if err := e.prepareObjects(template, objs); err != nil {
		return nil, err
	}

	return objs, nil
}

func NewEngine(c client.Client, scheme *runtime.Scheme) *Engine {
	return &Engine{
		client: c,
		scheme: scheme,
	}
}
