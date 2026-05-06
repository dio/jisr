# decoder

An LLM model-routing filter: the idiomatic jisr equivalent of a decoder-style
filter written with `RegisterRaw`. The entire filter is expressed as ordinary
Go handlers with no raw SDK types.

## What it does

1. Reads the `model` field from the JSON request body (first 8 KB only via `r.LimitBody`)
2. Maps the model name to a provider cluster (`openai`, `anthropic`, `default`)
3. Sets `x-cluster` header and calls `w.ClearRouteCache()` so Envoy's `cluster_header` route selects the right upstream
4. Sets filter metadata (`router/model`, `router/cluster`) for access logs and downstream filters
5. On the response side, taps token usage from both SSE streams and non-streaming JSON responses
6. Records a TTFT (time-to-first-token) histogram

## Envoy config wiring

The filter requires a `cluster_header` route:

```yaml
route_config:
  virtual_hosts:
    - name: providers
      domains: ["*"]
      routes:
        - match: { prefix: "/" }
          route:
            cluster_header: x-cluster
```

Clusters named `openai`, `anthropic`, and `default` must exist.

## Metrics

| Metric | Type | Tags | Description |
|--------|------|------|-------------|
| `router_requests_total` | counter | `cluster` | Requests routed per provider |
| `router_input_tokens` | counter | `cluster` | Input tokens counted per provider |
| `router_output_tokens` | counter | `cluster` | Output tokens counted per provider |
| `router_ttft_ms` | histogram | `cluster` | Time to first token in milliseconds |

## Build

```sh
make
# or:
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o libdecoder.so ./cmd
```

## What this demonstrates

- `RegisterWithConfigAndResponse` — config setup, request handler, response handler, and mode in one call
- `r.LimitBody(8192)` — read the first 8 KB for routing, stream the rest zero-copy to upstream
- `w.ClearRouteCache()` — re-evaluate the route after setting a routing header
- `w.IncrementCounter` / `w.RecordHistogram` — Envoy-native metrics from a handler
- `ResponseModeObserve` — tap SSE body while it streams to the client with zero added latency
- `w.SetMetadata` — publish routing decisions for access logs and downstream filters
