OUTPUT  := hello.so
GOCMD   := CGO_ENABLED=1 go

.PHONY: all build clean tidy

all: build

build:
	$(GOCMD) build -trimpath -buildmode=c-shared -o $(OUTPUT) ./cmd

clean:
	rm -f $(OUTPUT) $(OUTPUT:.so=.h)

tidy:
	$(GOCMD) mod tidy
