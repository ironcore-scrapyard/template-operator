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
	templatev1alpha1 "github.com/onmetal/template-operator/api/v1alpha1"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"time"
)

var _ = Describe("TemplateController", func() {
	var (
		namespace string

		configMapName string
		configMap     *corev1.ConfigMap

		secretName string

		templateName string
		template     *templatev1alpha1.Template
	)
	BeforeEach(func() {
		namespace = corev1.NamespaceDefault

		configMapName = "my-cm"
		configMap = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      configMapName,
			},
			Data: map[string]string{
				"bar": "baz",
			},
		}

		secretName = "my-secret"

		templateName = "my-template"
		template = &templatev1alpha1.Template{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      templateName,
			},
			Spec: templatev1alpha1.TemplateSpec{
				CommonLabels: map[string]string{
					"foo": "bar",
				},
				GroupKinds: []metav1.GroupKind{
					{
						Group: "",
						Kind:  "Secret",
					},
				},
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						"foo": "bar",
					},
				},
				Sources: []templatev1alpha1.TemplateSource{
					{
						Name: "config",
						Object: &templatev1alpha1.LocalObjectReference{
							APIVersion: "v1",
							Kind:       "ConfigMap",
							Name:       configMapName,
						},
					},
				},
				Data: templatev1alpha1.TemplateData{
					Inline: fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  namespace: {{ .Template.metadata.namespace }}
  name: %s
data:
  foo: "{{ .Values.config.data.bar | b64enc }}"
`, secretName),
				},
				Prune: true,
			},
		}
	})

	It("should apply a template", func() {
		ctx := context.Background()

		By("creating a config map")
		Expect(k8sClient.Create(ctx, configMap)).To(Succeed())

		By("creating a template")
		Expect(k8sClient.Create(ctx, template)).To(Succeed())

		By("waiting for the secret to be created")
		secret := &corev1.Secret{}
		Eventually(func() error {
			return k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: secretName}, secret)
		}, 10*time.Second).Should(Succeed())

		By("inspecting the secret labels")
		Expect(secret.Labels).To(Equal(map[string]string{"foo": "bar"}))

		By("inspecting the secret data")
		Expect(secret.Data).To(Equal(map[string][]byte{"foo": []byte("baz")}))

		By("updating the source config map")
		configMap.Data["bar"] = "qux"
		Expect(k8sClient.Update(ctx, configMap)).To(Succeed())

		By("waiting for the secret to be updated")
		Eventually(func() (*corev1.Secret, error) {
			err := k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: secretName}, secret)
			return secret, err
		}, 10*time.Second).Should(WithTransform(func(secret *corev1.Secret) map[string][]byte {
			return secret.Data
		}, Equal(map[string][]byte{"foo": []byte("qux")})))
	})
})
