# ws-proxy

An embedded WebSocket proxy for the OpenAI Realtime API, implemented entirely
inside an Envoy dynamic module `.so`.

## What it does

- Intercepts WebSocket upgrade requests at `/v1/responses`
- Proxies frames bidirectionally between the downstream client and `wss://api.openai.com/v1/responses`
- Injects the `Authorization: Bearer $OPENAI_API_KEY` header on the upstream connection
- Taps frames in both directions via `SessionTap`:
  - **Client → upstream**: extracts the `model` name from `response.create` messages
  - **Upstream → client**: extracts token usage from `response.completed` messages

Normal HTTP traffic (non-WebSocket) passes through to the upstream cluster unchanged.

## Architecture

```
Client ──WS──► Envoy (port 10000, upgrade route)
                  │ ──► ws-proxy-local cluster (127.0.0.1:<random>)
                  │             │
                  │     WSProxy.ServeHTTP
                  │             │ ──► wss://api.openai.com/v1/responses
                  │             │     Authorization: Bearer $OPENAI_API_KEY
                  │
Normal HTTP ──► upstream cluster
```

## Configuration

Set `OPENAI_API_KEY` in the environment before starting Envoy.

The `.so` binds the proxy on a loopback port at startup. By default it uses a
random free port and logs it:

```text
ws-proxy: listening on 127.0.0.1:XXXXX
```

For repeatable local configs or e2e tests, set `listen_addr` in the filter
config and point Envoy's `ws-proxy-local` STATIC cluster at the same address:

```yaml
filter_config:
  "@type": type.googleapis.com/google.protobuf.StringValue
  value: '{"listen_addr":"127.0.0.1:10001","upstream_url":"ws://127.0.0.1:18080","auth_value":"","otel_endpoint":"127.0.0.1:4317"}'
```

## Build

```sh
make
# or:
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o libws-proxy.so ./cmd
```

## Run

```sh
# set your OpenAI API key
export OPENAI_API_KEY=sk-...

# start Envoy
ENVOY_DYNAMIC_MODULES_SEARCH_PATH=$(pwd) envoy -c envoy.yaml
# stderr: ws-proxy: listening on 127.0.0.1:XXXXX
# (update ws-proxy-local cluster port in envoy.yaml if needed)

# normal HTTP — passes through to upstream
curl http://localhost:10000/v1/models -H "authorization: Bearer $OPENAI_API_KEY"

# WebSocket upgrade — proxied through the embedded WS server to OpenAI
# (use a WebSocket client that supports the Realtime API protocol)
```

## What this demonstrates

- `RegisterRaw` for filters that need full HTTP upgrade control
- `jisr/server.Group` for binding the proxy server to Envoy's filter lifecycle
- Bidirectional WebSocket proxying with frame-level tapping via callbacks
- Why jisr's `HandlerFunc` model cannot intercept WebSocket frames (they are raw TCP after the 101 handshake) — and how `RegisterRaw` + embedded server is the escape hatch
- Environment variable expansion for secret injection at runtime
- Embedded actor observability: structured session logs from `WSProxy.ServeHTTP`,
  actor-side metrics through `github.com/dio/logging`, e2e coverage with a
  local mock upstream, and OpenTelemetry guidance in
  [OBSERVABILITY.md](OBSERVABILITY.md)
