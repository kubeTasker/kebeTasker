#!/bin/bash
set -eux -o pipefail

go get k8s.io/code-generator/cmd/go-to-protobuf

bash ${GOPATH}/pkg/mod/k8s.io/code-generator@v0.28.4/generate-groups.sh \
  "deepcopy,client,informer,lister" \
  github.com/kubeTasker/kubeTasker/pkg/client github.com/kubeTasker/kubeTasker/pkg/apis \
  workflow:v1alpha1 \
  --go-header-file ./hack/custom-boilerplate.go.txt
