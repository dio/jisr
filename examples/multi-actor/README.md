# multi-actor

Demonstrates that `jisr/server.Group` is a lifecycle manager, not a
WebSocket-specific library. It runs three independent background actors inside
one `.so`, all tied to the same Envoy filter config lifecycle.

## Actors

| Actor | Type | Description |
|-------|------|-------------|
| HTTP server | `g.AddListener` | Serves `/status` on a random loopback port. Envoy routes traffic here via a STATIC cluster |
| Cache refresh | `g.AddGoroutine` | Periodic goroutine that runs every 10 seconds. Stops cleanly on `OnDestroy` |
| Raw TCP listener | `g.Add` | Listens for a custom ping/pong health probe. Write `ping\n`, receive `pong\n` |

All three start when `Create()` is called and stop atomically when `OnDestroy`
is called — the Group's stop channel propagates shutdown to every actor.

## Architecture

```
Envoy filter chain
  └── "multi-actor" filter (passes all HTTP traffic through)

Background actors (managed by server.Group):
  1. HTTP server    — port advertised in Envoy STATIC cluster
  2. Goroutine      — periodic cache refresh
  3. TCP listener   — raw ping/pong health probe
```

## Build

```sh
make
# or:
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o libmulti-actor.so ./cmd
```

## What this demonstrates

- `jisr/server.Group` for managing multiple background actors with a unified lifecycle
- `g.AddListener` — wrap any `net.Listener` + `http.Handler` as a Group actor
- `g.AddGoroutine` — run a long-lived goroutine that stops on context cancellation
- `g.Add(run, stop)` — integrate any service with a start/stop pair
- `RegisterRaw` for filters that need lifecycle hooks (`Create` / `OnDestroy`) beyond what `Register` provides
- How to expose a server port from a `.so` without hardcoding it
