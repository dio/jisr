OUTPUT     := libhello.so
ENVOY      ?= $(HOME)/.local/share/boe/envoy-versions/1.37.1/bin/envoy
ENVOY_PORT := 10000
ADMIN_PORT := 9901
GOCMD      := CGO_ENABLED=1 go

.PHONY: all build clean tidy run check backend

all: build

build:
	$(GOCMD) build -trimpath -buildmode=c-shared -o $(OUTPUT) ./cmd

clean:
	rm -f $(OUTPUT) $(OUTPUT:.so=.h)

tidy:
	$(GOCMD) mod tidy

# Start a JSON echo backend that reflects all received headers.
# Needed to verify jisr injects x-hello into the upstream request.
backend:
	python3 -c "\
import http.server, json; \
E = type('E', (http.server.BaseHTTPRequestHandler,), { \
  'do_GET': lambda s: [s.send_response(200), s.send_header('content-type','application/json'), \
    s.end_headers(), s.wfile.write(json.dumps({'path':s.path,'headers':dict(s.headers)},indent=2).encode())] and None, \
  'log_message': lambda *a: None}); \
http.server.HTTPServer(('127.0.0.1', 8080), E).serve_forever()"

# Run Envoy with the hello filter loaded from the current directory.
run: build
	GODEBUG=cgocheck=0 \
	ENVOY_DYNAMIC_MODULES_SEARCH_PATH=$(CURDIR) \
	$(ENVOY) -c envoy.yaml --log-level warning

# Fire a request through Envoy and show the x-hello header the backend received.
check:
	curl -s http://localhost:$(ENVOY_PORT)/ | python3 -m json.tool | grep x-hello
