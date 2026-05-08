SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

GOCMD := CGO_ENABLED=1 go

# All Go modules in this repo.
MODULES := . server buffer prof e2e \
           examples/hello examples/auth examples/decoder examples/sse-tap \
           examples/multi-actor examples/ws-proxy examples/spa

# Go modules that should run regular unit/race/vet checks. e2e is build-tagged
# and is exercised through the e2e target.
TEST_MODULES := . server buffer prof \
                examples/hello examples/auth examples/decoder examples/sse-tap \
                examples/multi-actor examples/ws-proxy examples/spa

EXAMPLE_MODULES := examples/hello examples/auth examples/decoder examples/sse-tap \
                   examples/multi-actor examples/ws-proxy examples/spa

define run-go-all
	@for m in $(TEST_MODULES); do \
		echo "$(1) $$m"; \
		if [ "$$m" = "examples/spa" ]; then \
			(cd $$m && go list ./... | grep -v '/ui/node_modules/' | xargs $(2)); \
		else \
			(cd $$m && $(2) ./...); \
		fi; \
	done
endef

.PHONY: all build test fmt-check vet-all test-all race-all tidy tidy-check \
        build-spa-ui build-examples build-e2e-module verify examples e2e clean

all: build test

build:
	$(GOCMD) build ./...

test:
	go test ./...

fmt-check:
	@test -z "$$(gofmt -l $$(find . -path './examples/spa/ui/node_modules' -prune -o -name '*.go' -print))"

vet-all:
	$(call run-go-all,vet,go vet)

test-all:
	$(call run-go-all,test,go test)

race-all:
	$(call run-go-all,race,go test -race)

## tidy: run go mod tidy in every module
tidy:
	@for m in $(MODULES); do \
		echo "tidy $$m"; \
		(cd $$m && GOWORK=off go mod tidy); \
	done

## tidy-check: tidy all modules and fail if any go.mod/go.sum is dirty
tidy-check: tidy
	git diff --exit-code -- $(foreach m,$(MODULES),$(m)/go.mod $(m)/go.sum)

build-spa-ui:
	$(MAKE) -C examples/spa ui

build-examples: build-spa-ui
	@for m in $(EXAMPLE_MODULES); do \
		echo "build $$m"; \
		if [ "$$m" = "examples/spa" ]; then \
			$(MAKE) -C $$m build-so; \
		else \
			$(MAKE) -C $$m build; \
		fi; \
	done

build-e2e-module:
	cd e2e && $(GOCMD) build -trimpath -buildmode=c-shared -o libe2e.so ./cmd

verify: fmt-check build-spa-ui vet-all test-all race-all build-examples e2e

examples:
	$(MAKE) -C examples/hello build

e2e: build-e2e-module
	cd e2e && go test -v -tags e2e -timeout 60s ./...

clean:
	$(MAKE) -C examples/hello clean
