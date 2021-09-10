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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TemplateSpec defines the desired state of Template
type TemplateSpec struct {
	// CommonLabels are common labels that should be applied for all resulting resources of this template.
	CommonLabels map[string]string `json:"commonLabels,omitempty"`

	// GroupKinds are metav1.GroupKinds that are produced by this template.
	GroupKinds []metav1.GroupKind `json:"groupKinds,omitempty"`
	// Selector is a metav1.LabelSelector to select resources produced by this template.
	Selector *metav1.LabelSelector `json:"selector,omitempty"`

	// Sources is a list of TemplateSource to draw template values from
	Sources []TemplateSource `json:"sources,omitempty"`

	// TemplateData holds the definition of the template.
	Data TemplateData `json:"data"`

	// Prune indicates whether to prune unused resources of a template.
	Prune bool `json:"prune,omitempty"`
}

// TemplateSource is a source for the values of a template.
type TemplateSource struct {
	// Name is the name the source shall be registered with in the values.
	Name string `json:"name"`
	// ObjectReference is a reference to an object to serve as source.
	Object *LocalObjectReference `json:"object,omitempty"`
	// Value is a literal yaml value to use as source.
	// +optional
	Value *apiextensionsv1.JSON `json:"value,omitempty"`
}

// LocalObjectReference references an object in a specific api version.
type LocalObjectReference struct {
	// APIVersion is the api version of the target object to use.
	APIVersion string `json:"apiVersion"`
	// Kind is the kind of the target object.
	Kind string `json:"kind"`
	// Name is the name of the target object.
	Name string `json:"name"`
}

// ConfigMapKeySelector is a reference to a specific 'key' within a ConfigMap resource.
// In some instances, `key` is a required field.
type ConfigMapKeySelector struct {
	// The name of the ConfigMap resource being referred to.
	corev1.LocalObjectReference `json:",inline"`
	// The key of the entry in the ConfigMap resource's `data` field to be used.
	// Some instances of this field may be defaulted, in others it may be
	// required.
	// +optional
	Key string `json:"key,omitempty"`
}

// DefaultConfigMapTemplateKey is the default key of the template definition in a config map.
const DefaultConfigMapTemplateKey = "template.yaml"

// TemplateData contains where the template definition should be drawn from.
type TemplateData struct {
	// Inline is an inline template definition.
	Inline string `json:"inline,omitempty"`
	// ConfigMapRef is the reference to a config map containing the template.
	// If key is not specified, it defaults to DefaultConfigMapTemplateKey.
	ConfigMapRef *ConfigMapKeySelector `json:"configMapRef,omitempty"`
}

// TemplateStatus defines the observed state of Template
type TemplateStatus struct {
	// Conditions is a list of TemplateCondition referring to individual state
	// information of a Template.
	Conditions []TemplateCondition `json:"conditions,omitempty"`
	// ManagedResources are resources that are managed by this template.
	ManagedResources []LocalObjectReference `json:"managedResources,omitempty"`
}

// TemplateConditionType is a type of a TemplateCondition.
type TemplateConditionType string

const (
	// TemplateApplied indicates whether a template could be successfully applied.
	TemplateApplied TemplateConditionType = "Applied"
)

// TemplateCondition is a status information of an aspect of a Template.
type TemplateCondition struct {
	// Type is the TemplateConditionType of this condition.
	Type TemplateConditionType `json:"type"`
	// Status reports the status of the condition.
	Status corev1.ConditionStatus `json:"status"`
	// Reason is a machine- and human-readable short explanation of the condition.
	Reason string `json:"reason"`
	// Message is a human-readable detailed explanation of the condition reason.
	Message string `json:"message"`
	// LastUpdateTime is the last time a condition has been updated.
	LastUpdateTime metav1.Time `json:"lastUpdateTime"`
	// LastTransitionTime is the last time a condition transitioned between two statuses.
	LastTransitionTime metav1.Time `json:"lastTransitionTime"`
	// ObservedGeneration is the observed generation for which a condition is reported.
	ObservedGeneration int64 `json:"observedGeneration"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:printcolumn:name="Applied",type=string,JSONPath=`.status.conditions[?(@.type == "Applied")].reason`,description="whether the template has been applied successfully"

// Template is the Schema for the templates API
type Template struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TemplateSpec   `json:"spec,omitempty"`
	Status TemplateStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// TemplateList contains a list of Template
type TemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Template `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Template{}, &TemplateList{})
}
