# sse-tap

Tap an SSE (Server-Sent Events) response stream to extract token usage without
buffering the entire body.

## What it does

The filter sits on the response path and processes streaming LLM responses:

- Input tokens appear near the **start** of the stream (Anthropic `message_start`, OpenAI first usage chunk)
- Output tokens appear near the **end** (Anthropic `message_delta`, OpenAI final usage chunk)

It uses `jisr/buffer.HeadTail` to capture the first 8 KB and last 64 KB of
each response. The middle of a large response is never stored. On stream
completion, it scans both regions to extract token counts and emits Envoy
counters.

## Response mode

`ResponseModeObserve`: body chunks are delivered to the handler goroutine
**and** forwarded to the downstream client simultaneously. Zero added latency.

## SSE formats supported

| Provider | Input tokens | Output tokens |
|----------|-------------|---------------|
| Anthropic | `event: message_start` → `data.message.usage.input_tokens` | `event: message_delta` → `data.usage.output_tokens` |
| OpenAI | `data.usage.prompt_tokens` / `input_tokens` | `data.usage.completion_tokens` / `output_tokens` |

## Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `sse_tap_input_tokens` | counter | Total input tokens observed |
| `sse_tap_output_tokens` | counter | Total output tokens observed |

## Build

```sh
make
# or:
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o libsse-tap.so ./cmd
```

## Run

```sh
# start Envoy pointing at an LLM backend that returns SSE
ENVOY_DYNAMIC_MODULES_SEARCH_PATH=$(pwd) envoy -c envoy.yaml

# send a streaming request — token counts appear in Envoy metrics
curl -N http://localhost:10000/v1/chat/completions \
  -H "content-type: application/json" \
  -d '{"model":"gpt-4o-mini","stream":true,"messages":[{"role":"user","content":"hi"}]}'

# check metrics
curl -s http://localhost:9901/stats | grep sse_tap
```

## What this demonstrates

- `RegisterWithConfigAndResponse` with `ResponseModeObserve`
- `jisr/buffer.HeadTail` — zero-allocation head+tail ring for large stream scanning
- Reading from `r.Body` while the body simultaneously streams to the client
- `w.IncrementCounter` and `w.SetMetadata` from a response handler
- `ExtractUsage` exported for unit testing without Envoy
