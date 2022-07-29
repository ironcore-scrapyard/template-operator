# template-operator

[![Go Report Card](https://goreportcard.com/badge/github.com/onmetal/template-operator)](https://goreportcard.com/report/github.com/onmetal/template-operator)
[![Go Reference](https://pkg.go.dev/badge/github.com/onmetal/template-operator.svg)](https://pkg.go.dev/github.com/onmetal/template-operator)
[![Build and Publish Docker Image](https://github.com/onmetal/template-operator/actions/workflows/publish-docker.yml/badge.svg)](https://github.com/onmetal/template-operator/actions/workflows/publish-docker.yml)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg?style=flat-square)](https://makeapullrequest.com)
[![GitHub License](https://img.shields.io/static/v1?label=License&message=Apache-2.0&color=blue&style=flat-square)](LICENSE)

## Overview

The template operator is an operator to create Kubernetes objects from other objects *at runtime*.

The need for this operator came up when we created certificates and their corresponding secrets via `cert-manager` and
wanted to use the generated certificate inside a kubeconfig that then should be passed into a pod (via Kubernetes
secret).

### Installation

The project also comes with a well-defined Makefile. The CRDs can be deployed using

```bash
make install
```

To run the controllers locally, just run

```bash
make run
```

To deploy the controllers into the currently selected (determined by your current kubeconfig) cluster, just run

```bash
make deploy
```

This will apply the [default kustomization)[config/default] with correct RBAC permissions.

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

## Contributing

We'd love to get feedback from you. Please report bugs, suggestions or post questions by opening a GitHub issue.

## License

[Apache-2.0](LICENSE)
