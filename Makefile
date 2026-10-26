GO ?= go
BUF ?= buf
DOCKER ?= docker
BIN_DIR := bin

MODULES := api cli drone mind
BINARIES := mind drone holism

.PHONY: all generate build test lint docker clean

all: generate build test

PROTO_FILES := $(shell find proto -name '*.proto' 2>/dev/null)

generate:
ifneq ($(PROTO_FILES),)
	$(BUF) generate
	@# protoc-gen-validate writes the vendored validate.proto's Go code under
	@# api/vendor; the canonical import is github.com/envoyproxy/protoc-gen-validate,
	@# so we keep only holism/v1 and drop the vendored copy.
	@rm -rf api/vendor
	@sed -i.bak 's|github.com/torrefatto/holism/api/vendor/validate|github.com/envoyproxy/protoc-gen-validate/validate|' api/holism/v1/*.pb.go
	@rm -f api/holism/v1/*.bak
	(cd api && go mod tidy)
else
	@echo "no .proto files yet, skipping generate"
endif

build:
	$(GO) build -o $(BIN_DIR)/mind ./mind/cmd/mind
	$(GO) build -o $(BIN_DIR)/drone ./drone/cmd/drone
	$(GO) build -o $(BIN_DIR)/holism ./cli/cmd/holism

test:
	@for m in $(MODULES); do \
		echo "== test ./$$m"; \
		(cd $$m && $(GO) test ./...) || exit 1; \
	done

lint:
ifneq ($(PROTO_FILES),)
	$(BUF) lint
else
	@echo "no .proto files yet, skipping buf lint"
endif
	@for m in $(MODULES); do \
		echo "== vet ./$$m"; \
		(cd $$m && $(GO) vet ./...) || exit 1; \
	done

docker:
	$(DOCKER) build -f mind/Dockerfile -t holism/mind .
	$(DOCKER) build -f drone/Dockerfile -t holism/drone .
	$(DOCKER) build -f cli/Dockerfile -t holism/cli .

clean:
	rm -rf $(BIN_DIR)
