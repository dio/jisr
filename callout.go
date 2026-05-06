package jisr

import (
	"context"
	"fmt"
	"net/http"

	"github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"
)

// CalloutResponse holds the result of a Do call.
type CalloutResponse struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// calloutResult is sent on the result channel by the callout callback.
type calloutResult struct {
	resp *CalloutResponse
	err  error
}

// calloutCallback implements shared.HttpCalloutCallback, sending results on a channel.
type calloutCallback struct {
	ch chan<- calloutResult
}

func (cb *calloutCallback) OnHttpCalloutDone(
	_ uint64,
	result shared.HttpCalloutResult,
	headers [][2]shared.UnsafeEnvoyBuffer,
	bodyChunks []shared.UnsafeEnvoyBuffer,
) {
	if result != shared.HttpCalloutSuccess {
		cb.ch <- calloutResult{err: fmt.Errorf("jisr: callout failed with result %d", result)}
		return
	}

	// Copy response headers into Go memory using canonical keys
	// so resp.Header.Get() works with any casing — consistent with jisr.Request.Header.
	h := make(http.Header, len(headers))
	var status int
	for _, kv := range headers {
		k := kv[0].ToString()
		v := kv[1].ToString()
		if k == ":status" {
			fmt.Sscanf(v, "%d", &status)
			continue
		}
		h.Add(k, v)
	}

	// Assemble body.
	var body []byte
	for _, chunk := range bodyChunks {
		body = append(body, chunk.ToBytes()...)
	}

	cb.ch <- calloutResult{resp: &CalloutResponse{
		StatusCode: status,
		Header:     h,
		Body:       body,
	}}
}

// Do performs a blocking HTTP callout to an Envoy-managed upstream cluster.
// It is safe to call from inside a HandlerFunc goroutine.
//
// cluster is the upstream cluster name as defined in envoy.yaml.
// headers should include at minimum ":method", ":path", ":authority".
// timeoutMs is the callout timeout in milliseconds.
//
// Do uses Envoy's native HttpCallout (connection pooling, retries, circuit
// breaking from cluster config) while presenting a blocking call to the handler.
// It respects ctx cancellation — if the context is cancelled, Do returns
// context.Canceled immediately.
func Do(
	ctx context.Context,
	scheduler shared.Scheduler,
	handle shared.HttpFilterHandle,
	cluster string,
	headers [][2]string,
	body []byte,
	timeoutMs uint64,
) (*CalloutResponse, error) {
	resultCh := make(chan calloutResult, 1)

	// HttpCallout must be initiated from the Envoy worker thread.
	// Schedule the call, then block the goroutine on the result channel.
	scheduler.Schedule(func() {
		result, _ := handle.HttpCallout(cluster, headers, body, timeoutMs,
			&calloutCallback{ch: resultCh})
		if result != shared.HttpCalloutInitSuccess {
			resultCh <- calloutResult{
				err: fmt.Errorf("jisr: callout init failed: %v", result),
			}
		}
	})

	select {
	case res := <-resultCh:
		return res.resp, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
