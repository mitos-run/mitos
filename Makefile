IMG_CONTROLLER ?= ghcr.io/paperclipinc/mitos-controller:latest
IMG_FORKD ?= ghcr.io/paperclipinc/mitos-forkd:latest

.PHONY: all build test test-linux test-netlink generate manifests proto docker-build docker-push install deploy

# Linux container used to exercise //go:build linux packages (guest agent,
# netlink) from a darwin dev host. Override with a local mirror if Docker Hub
# rate-limits.
GO_LINUX_IMG ?= golang:1.26

all: build

build:
	go build -o bin/controller ./cmd/controller/
	go build -o bin/forkd ./cmd/forkd/

test-unit:
	go test ./internal/fork/... ./internal/workspace/... ./internal/vsock/... -v -count=1

# Run the linux-only test packages locally in a throwaway container. These never
# compile on darwin (all //go:build linux), so this is the fast pre-CI loop for
# the guest agent and guest networking: seconds, versus a cluster e2e cycle.
test-linux:
	docker run --rm -v "$(PWD)":/src -v "$(HOME)/go/pkg/mod":/go/pkg/mod -w /src $(GO_LINUX_IMG) \
		go test ./guest/agent/... ./internal/guestnet/...

# Drive the real rtnetlink datapath against a live kernel (CAP_NET_ADMIN), so a
# guest-networking change is verified end to end without a KVM run. Gated behind
# the nettest tag; runs on the loopback interface.
test-netlink:
	docker run --rm --privileged -v "$(PWD)":/src -v "$(HOME)/go/pkg/mod":/go/pkg/mod -w /src $(GO_LINUX_IMG) \
		go test -tags nettest ./internal/guestnet/ -run Integration -count=1 -v

test-controller:
	eval $$(go run sigs.k8s.io/controller-runtime/tools/setup-envtest@latest use 1.31 -p env) && \
		go test ./internal/controller/... -v -count=1 -timeout 120s

test-python:
	cd sdk/python && PYTHONPATH=. python3 -m pytest tests/ -v

test-e2e:
	bash hack/e2e-test.sh

test: test-unit test-python

generate:
	controller-gen object paths="./api/..."

manifests:
	controller-gen crd paths="./api/..." output:crd:artifacts:config=deploy/crds

proto:
	protoc \
		--go_out=. --go_opt=module=mitos.run/mitos \
		--go-grpc_out=. --go-grpc_opt=module=mitos.run/mitos \
		proto/forkd.proto

docker-build:
	docker build -f Dockerfile.controller -t $(IMG_CONTROLLER) .
	docker build -f Dockerfile.forkd -t $(IMG_FORKD) .

docker-push:
	docker push $(IMG_CONTROLLER)
	docker push $(IMG_FORKD)

install:
	kubectl apply -f deploy/controller/namespace.yaml
	kubectl apply -f deploy/crds/
	kubectl apply -f deploy/controller/
	kubectl apply -f deploy/daemon/

deploy: docker-build docker-push install

lint:
	golangci-lint run ./...

clean:
	rm -rf bin/
