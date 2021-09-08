# template-operator

[![Go Report Card](https://goreportcard.com/badge/github.com/onmetal/template-operator)](https://goreportcard.com/report/github.com/onmetal/template-operator)
[![Go Reference](https://pkg.go.dev/badge/github.com/onmetal/template-operator.svg)](https://pkg.go.dev/github.com/onmetal/template-operator)

The template operator is an operator to create Kubernetes objects from other objects *at runtime*.

The need for this operator came up when we created certificates and their corresponding secrets via `cert-manager` and
wanted to use the generated certificate inside a kubeconfig that then should be passed into a pod (via Kubernetes
secret).

## Installation

### Helm

Helm can be used to install the `template-operator` into your Kubernetes cluster. The Helm chart repository can be added via:

```shell
helm repo add template-operator https://onmetal.github.io/template-operator/
helm repo update
```

To install the `template-operator` Helm chart run:

```shell
helm install my-templ-op template-operator/template-operator
```

## Usage

The main resource of the template operator is a `Template`. This resource manages the actual go template, the source
values and how they are obtained as well as the pruning in case any object templated via that template isn't needed
anymore.

Given an existing `ConfigMap` in the cluster like

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  namespace: default
  name: my-cm
data:
  foo: "bar"
```

We can create a `Template` that creates a secret from the `ConfigMap`'s data by applying a template with

```yaml
apiVersion: template.onmetal.de/v1alpha1
kind: Template
metadata:
  name: my-template
spec:
  groupKinds:
    - group: ""
      kind: Secret
  commonLabels:
    managed-by: my-template
  selector:
    matchLabels:
      managed-by: my-template
  prune: true
  sources:
    - name: myCM
      object:
        apiVersion: v1
        kind: ConfigMap
        namespace: default
        name: my-cm
  data:
    inline: |-
      apiVersion: v1
      kind: Secret
      metadata:
        namespace: default
        name: my-secret
      type: Opaque
      data:
        foo: "{{ .Values.myCM.data.foo | b64enc }}"
```

After a short while, our cluster should then have a secret

```yaml
apiVersion: v1
kind: Secret
metadata:
  namespace: default
  name: kubeconfig
  labels:
    managed-by: my-template
type: Opaque
data:
  foo: YmFy
```
