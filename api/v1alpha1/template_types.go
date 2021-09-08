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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// TemplateSpec defines the desired state of Template
type TemplateSpec struct {
	CommonLabels map[string]string `json:"commonLabels,omitempty"`

	GroupKinds []metav1.GroupKind    `json:"groupKinds,omitempty"`
	Selector   *metav1.LabelSelector `json:"selector,omitempty"`

	Sources []TemplateSource `json:"sources,omitempty"`

	Data TemplateData `json:"data"`

	Prune bool `json:"prune,omitempty"`
}

type TemplateSource struct {
	Name   string           `json:"name"`
	Object *ObjectReference `json:"object,omitempty"`
}

type ObjectReference struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
}

type TemplateData struct {
	Inline string `json:"inline,omitempty"`
}

// TemplateStatus defines the observed state of Template
type TemplateStatus struct {
	Conditions       []TemplateCondition `json:"conditions,omitempty"`
	ManagedResources []ObjectReference   `json:"managedResources,omitempty"`
}

type TemplateConditionType string

const (
	TemplateApplied TemplateConditionType = "Applied"
)

type TemplateCondition struct {
	Type               TemplateConditionType  `json:"type"`
	Status             corev1.ConditionStatus `json:"status"`
	Reason             string                 `json:"reason"`
	Message            string                 `json:"message"`
	LastUpdateTime     metav1.Time            `json:"lastUpdateTime"`
	LastTransitionTime metav1.Time            `json:"lastTransitionTime"`
	ObservedGeneration int64                  `json:"observedGeneration"`
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
