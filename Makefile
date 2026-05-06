GOCMD := CGO_ENABLED=1 go

# All Go modules in this repo.
MODULES := . server buffer prof e2e \
           examples/hello examples/decoder examples/sse-tap \
           examples/multi-actor examples/ws-proxy examples/spa

.PHONY: all build test tidy tidy-check examples e2e clean

all: build test

build:
	$(GOCMD) build ./...

test:
	go test ./...

## tidy: run go mod tidy in every module
tidy:
	@for m in $(MODULES); do \
		echo "tidy $$m"; \
		(cd $$m && GOWORK=off go mod tidy); \
	done

## tidy-check: tidy all modules and fail if any go.mod/go.sum is dirty
tidy-check: tidy
	git diff --exit-code -- $(foreach m,$(MODULES),$(m)/go.mod $(m)/go.sum)

examples:
	$(MAKE) -C examples/hello

e2e: examples
	cd e2e && go test -v -tags e2e -timeout 60s ./...

clean:
	$(MAKE) -C examples/hello clean
