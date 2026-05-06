build:
	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o libdecoder.so ./cmd

test:
	go test -race ./...

.PHONY: build test
