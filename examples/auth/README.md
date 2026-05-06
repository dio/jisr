# auth

Demonstrates `RegisterFactory` — the struct-based handler pattern for filters
that need per-config state without package-level variables.

## Why not RegisterWithConfig?

`RegisterWithConfig` stores metric IDs and parsed config in package-level vars.
When a single `.so` is loaded by two Envoy listeners with **different**
`filter_config` bytes (e.g. different allowed key sets per route), the
`ConfigFunc` is called twice and the second call silently overwrites the first.

`RegisterFactory` constructs a new `*AuthFilter` per `Create` call. Each
listener gets its own independent instance:

```
Listener A  →  Create()  →  &AuthFilter{allowed: {"key-admin"}, ...}
Listener B  →  Create()  →  &AuthFilter{allowed: {"key-public","key-guest"}, ...}
```

No shared state. No race. Testable by constructing the struct directly.

## What the filter does

**Request phase:**
- Reads `x-api-key` header
- Rejects with `401` if the key is not in the configured allow-list
- Injects `x-user-id: <key>` for accepted requests
- Increments `auth_requests_total{result=allowed|rejected}`

**Response phase** (Passthrough — zero body overhead):
- Records upstream status code in filter metadata
- Increments `auth_responses_total{status=2xx|4xx|5xx}`

## Config

Pass JSON as `filter_config` in envoy.yaml:

```yaml
- name: auth
  typed_config:
    "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
    dynamic_module_config:
      name: auth
    filter_name: auth
    filter_config:
      "@type": type.googleapis.com/google.protobuf.StringValue
      value: '{"allowed_keys":["key-admin","key-readonly"],"metadata_ns":"auth"}'
```

Fields:

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `allowed_keys` | `[]string` | `[]` (all rejected) | API keys that may pass |
| `metadata_ns` | `string` | `"auth"` | Dynamic metadata namespace |

## Metrics

| Metric | Type | Tags | Description |
|--------|------|------|-------------|
| `auth_requests_total` | counter | `result` | `allowed` or `rejected` |
| `auth_responses_total` | counter | `status` | `2xx`, `4xx`, or `5xx` |

## Build

```sh
make
# or:
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o libauth.so ./cmd
```

## Run

```sh
# start a backend on port 8080
python3 -m http.server 8080

# start Envoy
ENVOY_DYNAMIC_MODULES_SEARCH_PATH=$(pwd) envoy -c envoy.yaml

# port 10000 — admin keys only
curl -H "x-api-key: key-admin" http://localhost:10000/
curl -H "x-api-key: key-readonly" http://localhost:10000/
curl -H "x-api-key: key-public" http://localhost:10000/   # 401 — not in admin list

# port 10001 — public keys only
curl -H "x-api-key: key-public" http://localhost:10001/
curl -H "x-api-key: key-admin" http://localhost:10001/    # 401 — not in public list
```

## Tests

The struct-based pattern makes tests straightforward — no global state, no Envoy:

```go
// Construct the filter directly with different configs.
adminFilter := newAuth("key-admin")
publicFilter := newAuth("key-public", "key-guest")
```

Run:

```sh
go test -race ./...
```

## What this demonstrates

- `RegisterFactoryWithResponse` — factory constructs `*AuthFilter` once at config time
- Each Envoy listener that uses `auth` gets its own struct instance with its own config and metric IDs
- `jisr.Chain` + `jisr.Middleware` — `loggingMiddleware` wraps `f.HandleRequest`; middleware composed inside the factory so it shares the same struct instance
- Struct methods as `HandlerFunc` / `ResponseFunc` — `f.HandleRequest`, `f.HandleResponse`
- `ResponseModePassthrough` for response header inspection without body overhead
- Per-config metric IDs — `auth_requests_total` is separate per listener
- Tests that construct the struct directly, independent of Envoy and global state
