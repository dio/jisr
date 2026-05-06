GOCMD := CGO_ENABLED=1 go

.PHONY: all build test tidy examples e2e clean

all: build test

build:
	$(GOCMD) build ./...

test:
	go test ./...

tidy:
	go mod tidy
	$(MAKE) -C examples/hello tidy
	cd e2e && go mod tidy

examples:
	$(MAKE) -C examples/hello

e2e: examples
	cd e2e && go test -v -tags e2e -timeout 60s ./...

clean:
	$(MAKE) -C examples/hello clean
