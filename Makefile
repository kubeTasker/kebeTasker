PACKAGE=github.com/kubeTasker/kubeTasker
CURRENT_DIR=$(shell pwd)
DIST_DIR=${CURRENT_DIR}/dist
IMAGE_NAMESPACE=songjunfan

VERSION=$(shell cat ${CURRENT_DIR}/VERSION)
BUILD_DATE=$(shell date -u +'%Y-%m-%dT%H:%M:%SZ')
GIT_COMMIT=$(shell git rev-parse HEAD)
GIT_TAG=$(shell if [ -z "`git status --porcelain`" ]; then git describe --exact-match --tags HEAD 2>/dev/null; fi)
GIT_TREE_STATE=$(shell if [ -z "`git status --porcelain`" ]; then echo "clean" ; else echo "dirty"; fi)

BUILDER_IMAGE=kubetasker-builder
# NOTE: the volume mount of ${DIST_DIR}/pkg below is optional and serves only
# to speed up subsequent builds by caching ${GOPATH}/pkg between builds.
BUILDER_CMD=docker run --rm \
  -v ${CURRENT_DIR}:/root/go/src/${PACKAGE} \
  -v ${DIST_DIR}/pkg:/root/go/pkg \
  -w /root/go/src/${PACKAGE} ${BUILDER_IMAGE}

LDFLAGS = \
  -X ${PACKAGE}.version=${VERSION} \
  -X ${PACKAGE}.buildDate=${BUILD_DATE} \
  -X ${PACKAGE}.gitCommit=${GIT_COMMIT} \
  -X ${PACKAGE}.gitTreeState=${GIT_TREE_STATE}

DOCKER_PUSH=true
IMAGE_TAG=latest
ifneq (${GIT_TAG},)
IMAGE_TAG=${GIT_TAG}
LDFLAGS += -X ${PACKAGE}.gitTag=${GIT_TAG}
endif
ifneq (${IMAGE_NAMESPACE},)
LDFLAGS += -X ${PACKAGE}/cmd/tasker/commands.imageNamespace=${IMAGE_NAMESPACE}
endif
ifneq (${IMAGE_TAG},)
LDFLAGS += -X ${PACKAGE}/cmd/tasker/commands.imageTag=${IMAGE_TAG}
endif

ifeq (${DOCKER_PUSH},true)
ifndef IMAGE_NAMESPACE
$(error IMAGE_NAMESPACE must be set to push images (e.g. IMAGE_NAMESPACE=kubetasker))
endif
endif

ifdef IMAGE_NAMESPACE
IMAGE_PREFIX=${IMAGE_NAMESPACE}/
endif

builder:
	docker build -t ${BUILDER_IMAGE} -f Dockerfile-builder .

cli:
	go build -v -ldflags '${LDFLAGS}' -o ${DIST_DIR}/tasker ./cmd/tasker

cli-debug:
	go build -v -ldflags '${LDFLAGS}' -gcflags="all=-N -l" -o ${DIST_DIR}/tasker ./cmd/tasker

controller:
	go build -v -ldflags '${LDFLAGS}' -o ${DIST_DIR}/workflow-controller ./cmd/workflow-controller

controller-debug:
	go build -v -ldflags '${LDFLAGS}' -gcflags="all=-N -l" -o ${DIST_DIR}/workflow-controller ./cmd/workflow-controller

controller-linux: builder
	${BUILDER_CMD} make controller

controller-image: controller-linux
	docker build -t $(IMAGE_PREFIX)workflow-controller:$(IMAGE_TAG) -f Dockerfile-workflow-controller .
	@if [ "$(DOCKER_PUSH)" = "true" ] ; then docker push $(IMAGE_PREFIX)workflow-controller:$(IMAGE_TAG) ; fi

executor:
	go build -v -ldflags '${LDFLAGS}' -o ${DIST_DIR}/taskerexec ./cmd/taskerexec

executor-debug:
	go build -v -ldflags '${LDFLAGS}' -gcflags="all=-N -l" -o ${DIST_DIR}/taskerexec ./cmd/taskerexec

executor-linux: builder
	${BUILDER_CMD} make executor

executor-image: executor-linux
	docker build -t $(IMAGE_PREFIX)taskerexec:$(IMAGE_TAG) -f Dockerfile-taskerexec .
	@if [ "$(DOCKER_PUSH)" = "true" ] ; then docker push $(IMAGE_PREFIX)taskerexec:$(IMAGE_TAG) ; fi

# protoc,my.proto
define protoc
	# protoc $(1)
    [ -e ./vendor ] || go mod vendor
    protoc \
      -I /usr/local/include \
      -I $(CURDIR) \
      -I $(CURDIR)/vendor \
      -I $(GOPATH)/src \
      -I $(GOPATH)/pkg/mod/github.com/gogo/protobuf@v1.3.2/gogoproto \
      -I $(GOPATH)/pkg/mod/github.com/grpc-ecosystem/grpc-gateway@v1.16.0/third_party/googleapis \
      --gogofast_out=plugins=grpc:$(GOPATH)/src \
      --grpc-gateway_out=logtostderr=true:$(GOPATH)/src \
      --swagger_out=logtostderr=true,fqn_for_swagger_name=true:. \
      $(1)

endef

pkg/apis/workflow/v1alpha1/generated.proto:
	# Format proto files. Formatting changes generated code, so we do it here, rather that at lint time.
	# Why clang-format? Google uses it.
	find pkg/apiclient -name '*.proto'|xargs clang-format -i
	$(GOPATH)/bin/go-to-protobuf \
		--go-header-file=./hack/custom-boilerplate.go.txt \
		--packages=github.com/kubeTasker/kubeTasker/pkg/apis/workflow/v1alpha1 \
		--apimachinery-packages=+k8s.io/apimachinery/pkg/util/intstr,+k8s.io/apimachinery/pkg/api/resource,k8s.io/apimachinery/pkg/runtime/schema,+k8s.io/apimachinery/pkg/runtime,k8s.io/apimachinery/pkg/apis/meta/v1,k8s.io/api/core/v1,k8s.io/api/policy/v1 \
		--proto-import $(GOPATH)/src
	touch pkg/apis/workflow/v1alpha1/generated.proto

test:
	go test ./...
