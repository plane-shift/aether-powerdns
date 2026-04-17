REGISTRY ?= ghcr.io/plane-shift
IMAGE    ?= $(REGISTRY)/aether-powerdns
TAG      ?= dev
PLATFORMS ?= linux/amd64,linux/arm64

.PHONY: tidy build vet test fmt image image-push deploy undeploy crd

tidy:
	go mod tidy

fmt:
	go fmt ./...

vet:
	go vet ./...

build:
	CGO_ENABLED=0 go build -o bin/aether-powerdns ./cmd/operator

test:
	go test ./...

# Local image build (current arch only).
image:
	docker build -t $(IMAGE):$(TAG) .

# Multi-arch build + push (requires `docker buildx` and a logged-in registry).
image-push:
	docker buildx build --platform $(PLATFORMS) --no-cache -t $(IMAGE):$(TAG) --push .

deploy: crd
	kubectl apply -k config/

undeploy:
	kubectl delete -k config/ --ignore-not-found

crd:
	kubectl apply -f config/crd/dns.aetherplatform.cloud_powerdnsservers.yaml
