# hello

The canonical jisr example. Six filters in one `.so` covering all three
response modes and the most common filter patterns.

## Filters

| Filter name | Mode | What it does |
|-------------|------|--------------|
| `hello` | request-only | Injects `x-hello: from-jisr` into every upstream request. Increments `hello_requests_total` counter |
| `hello-echo` | request-only | Responds directly to the client with a JSON echo of path, method, headers, and body. No upstream involved |
| `resp-stamp` | Passthrough | Runs on the response path. Inspects `r.StatusCode` and `r.Header` without touching the body |
| `resp-tap` | Observe | Consumes the response body as it streams to the client simultaneously. Zero added latency |
| `resp-rewrite` | Buffer | Reads the full JSON response body, injects `x_jisr_rewritten: true`, updates `content-length`, replaces body |
| `resp-header-stamp` | Buffer | Calls `r.SkipBody()` and adds `x-jisr-stamp: <status>` to the upstream response headers |

## Build

```sh
make
# or:
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o libhello.so ./cmd
```

## Run

```sh
# start a backend on port 8080 (any HTTP server)
python3 -m http.server 8080

# start Envoy
ENVOY_DYNAMIC_MODULES_SEARCH_PATH=$(pwd) envoy -c envoy.yaml

# hello filter: injects x-hello header
curl http://localhost:10000/

# hello-echo: direct response, no upstream
curl -X POST http://localhost:10001/ping -d '{"hello":"world"}' -H content-type:application/json

# resp-rewrite: JSON body gets x_jisr_rewritten injected
curl http://localhost:10000/
```

## Admin server

On startup, the `.so` binds a pprof/admin server on a random loopback port
and logs the address:

```
hello: admin server listening on 127.0.0.1:XXXXX
```

Endpoints: `/healthz`, `/readyz`, `/version`, `/debug/pprof/*`

Set `JISR_ADMIN_PORT_FILE=/tmp/port` before starting Envoy to write the port
to a file (used by the e2e test harness).

## What this demonstrates

- `RegisterWithConfig` for defining Envoy counters at `.so` load time
- `RegisterWithResponse` with all three modes (Passthrough / Observe / Buffer)
- `r.GetAttr(jisr.AttrRequestPath)` for pre-snapshotted stream attributes
- `r.SkipBody()` on both `Request` and `Response`
- `w.ReplaceBody` + `w.SetUpstreamResponseHeader` for response rewriting
- `jisr/prof` admin server wired via `init()`
