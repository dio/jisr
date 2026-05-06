GOCMD := CGO_ENABLED=1 go

.PHONY: all build test tidy examples clean

all: build test

build:
	$(GOCMD) build ./...

test:
	$(GOCMD) test ./...

tidy:
	$(GOCMD) mod tidy

examples:
	$(MAKE) -C examples/hello

clean:
	$(MAKE) -C examples/hello clean
