##@ Go (go/: ferund, ADR 0015)

GO_DIR := go
GO_OUT := target/go
# The Makefile defines it after including this file.
LOCALBIN ?= $(shell pwd)/bin
# The toolchain go/go.mod names; go downloads it when it is not there.
GO ?= go
PROTOC_GEN_GO_VERSION ?= v1.36.12
PROTOC_GEN_GO_GRPC_VERSION ?= v1.6.2
GO_MODULE := github.com/ironcore-dev/feos/go
# Every proto/feos/<api>/v1/<api>.proto becomes the Go package
# $(GO_MODULE)/gen/feos/<api>/v1, named <api>v1. The protos name no Go
# package of their own, so the mapping is given to protoc here.
GO_PROTOS := $(sort $(wildcard proto/feos/*/v1/*.proto))
GO_PROTO_MAP := $(foreach p,$(GO_PROTOS),M$(patsubst proto/%,%,$(p))=$(GO_MODULE)/gen/$(patsubst proto/%/,%,$(dir $(p)));$(word 2,$(subst /, ,$(patsubst proto/%,%,$(p))))v1)

.PHONY: go-gen go-build go-fmt-check go-vet go-lint go-test go-test-unit web

$(LOCALBIN)/protoc-gen-go: | $(LOCALBIN)
	GOBIN=$(LOCALBIN) $(GO) install google.golang.org/protobuf/cmd/protoc-gen-go@$(PROTOC_GEN_GO_VERSION)

$(LOCALBIN)/protoc-gen-go-grpc: | $(LOCALBIN)
	GOBIN=$(LOCALBIN) $(GO) install google.golang.org/grpc/cmd/protoc-gen-go-grpc@$(PROTOC_GEN_GO_GRPC_VERSION)

go-gen: $(LOCALBIN)/protoc-gen-go $(LOCALBIN)/protoc-gen-go-grpc ## Generate go/gen from proto/.
	rm -rf $(GO_DIR)/gen/feos
	mkdir -p $(GO_DIR)/gen
	protoc -I proto \
		--plugin=protoc-gen-go=$(LOCALBIN)/protoc-gen-go \
		--plugin=protoc-gen-go-grpc=$(LOCALBIN)/protoc-gen-go-grpc \
		--go_out=$(GO_DIR)/gen --go_opt=module=$(GO_MODULE)/gen \
		$(foreach m,$(GO_PROTO_MAP),'--go_opt=$(m)') \
		--go-grpc_out=$(GO_DIR)/gen --go-grpc_opt=module=$(GO_MODULE)/gen \
		$(foreach m,$(GO_PROTO_MAP),'--go-grpc_opt=$(m)') \
		$(GO_PROTOS)

go-build: ## Build ferund and ferun-testnode, static, into target/go/.
	mkdir -p $(GO_OUT)
	cd $(GO_DIR) && CGO_ENABLED=0 $(GO) build -trimpath -o ../$(GO_OUT)/ferund ./cmd/ferund
	cd $(GO_DIR) && CGO_ENABLED=0 $(GO) build -trimpath -o ../$(GO_OUT)/ferun-testnode ./cmd/ferun-testnode

go-fmt-check: ## Check the Go code's formatting.
	@out=$$(cd $(GO_DIR) && gofmt -l $$(find . -name '*.go' -not -path './gen/*')); \
	if [ -n "$$out" ]; then echo "gofmt: $$out"; exit 1; fi

go-vet: ## Vet the Go code.
	cd $(GO_DIR) && $(GO) vet ./...

go-lint: go-fmt-check go-vet ## gofmt and go vet.

go-test-unit: ## The Go tests that need no database: those that do skip.
	cd $(GO_DIR) && $(GO) test ./...

go-test: ## All Go tests, against a PostgreSQL in docker unless FERUN_TEST_DATABASE_URL names one.
	hack/go/with-postgres sh -c 'cd $(GO_DIR) && $(GO) test -count=1 ./...'

web: ## Build the management UI into go/internal/ui/dist.
	cd web && npm ci && npm run build
