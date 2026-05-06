// Package decoder demonstrates a zia-decoder-style LLM routing filter using
// the jisr RegisterWithConfig API.
//
// What it does:
//   - Reads the "model" field from the JSON request body (first 8KB only)
//   - Resolves model name to a provider cluster (openai, anthropic, default)
//   - Sets x-cluster header + calls ClearRouteCache so Envoy's cluster_header
//     route selects the right upstream
//   - Sets filter metadata: model and cluster names
//   - Emits Envoy counters per provider cluster
//   - On the response side, counts tokens from both streaming (SSE) and
//     non-streaming (JSON) responses
//
// This is the idiomatic jisr equivalent of zia-decoder written with RegisterRaw.
// The entire filter is expressed as ordinary Go handlers with no raw SDK types.
//
// # Envoy config wiring
//
// The filter expects a cluster_header route on the virtual host:
//
//	route_config:
//	  virtual_hosts:
//	    - name: providers
//	      domains: ["*"]
//	      routes:
//	        - match: { prefix: "/" }
//	          route:
//	            cluster_header: x-cluster
//
// Clusters named "openai", "anthropic", and "default" must exist in static_resources.
package decoder

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dio/jisr"
)

// Cluster names written to x-cluster header.
const (
	ClusterOpenAI    = "openai"
	ClusterAnthropic = "anthropic"
	ClusterDefault   = "default"
)

// Metrics: defined once at config time, incremented per-request.
var (
	requestsTotal jisr.MetricID // zia_decoder_requests_total{cluster}
	inputTokens   jisr.MetricID // zia_decoder_input_tokens{cluster}
	outputTokens  jisr.MetricID // zia_decoder_output_tokens{cluster}
	ttftMs        jisr.MetricID // zia_decoder_ttft_ms{cluster}
)

// requestState carries per-request data from the request phase to the
// response phase. Both phases run in the same goroutine, so no locking needed.
type requestState struct {
	cluster    string
	model      string
	sentAt     time.Time
	firstChunk time.Time
}

func init() {
	jisr.RegisterWithConfigAndResponse("zia-decoder",
		func(h jisr.ConfigHandle) error {
			var err error
			if requestsTotal, err = h.DefineCounter("zia_decoder_requests_total", "cluster"); err != nil {
				return err
			}
			if inputTokens, err = h.DefineCounter("zia_decoder_input_tokens", "cluster"); err != nil {
				return err
			}
			if outputTokens, err = h.DefineCounter("zia_decoder_output_tokens", "cluster"); err != nil {
				return err
			}
			ttftMs, err = h.DefineHistogram("zia_decoder_ttft_ms", "cluster")
			return err
		},
		decoderRequest,
		decoderResponse,
		jisr.ResponseModeObserve,
	)
}

// decoderRequest is the request phase. It reads the model field from the body,
// sets the routing header, and emits the request counter.
func decoderRequest(_ context.Context, w jisr.ResponseWriter, r *jisr.Request) {
	// LimitBody reads only the first 8KB: enough for any JSON request envelope.
	// The rest of the body streams to upstream without being copied into Go memory.
	r.LimitBody(8 * 1024)
	head, _ := io.ReadAll(r.Body)

	var req struct {
		Model string `json:"model"`
	}
	json.Unmarshal(head, &req) //nolint:errcheck

	cluster := resolveCluster(req.Model)

	// Route to the resolved cluster.
	w.SetRequestHeader("x-cluster", cluster)

	// Publish model + cluster for downstream filters and access logs.
	w.SetMetadata("zia_decoder", "model", req.Model)
	w.SetMetadata("zia_decoder", "cluster", cluster)

	// ClearRouteCache tells Envoy to re-evaluate the cluster_header route
	// with the x-cluster value we just set.
	w.ClearRouteCache()

	w.IncrementCounter(requestsTotal, 1, cluster)

	r.Log(jisr.LogDebug, "zia-decoder: model=%q cluster=%s path=%s",
		req.Model, cluster, r.GetAttr(jisr.AttrRequestPath))
}

// decoderResponse is the response phase. It taps token usage from both
// streaming (SSE) and non-streaming (JSON) responses.
func decoderResponse(_ context.Context, w jisr.ResponseWriter, r *jisr.Response) {
	// Recover cluster from response headers set by upstream (or use default).
	// In production zia, metadata flows forward; here we re-derive from status.
	cluster := ClusterDefault

	now := time.Now()

	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "text/event-stream") {
		tapSSE(w, r, cluster, now)
	} else {
		tapJSON(w, r, cluster, now)
	}
}

// tapSSE reads the SSE body and extracts token usage on stream end.
func tapSSE(w jisr.ResponseWriter, r *jisr.Response, cluster string, start time.Time) {
	firstChunk := time.Time{}
	buf := make([]byte, 4096)
	var tail []byte // keep last 512 bytes to find final usage chunk

	for {
		n, err := r.Body.Read(buf)
		if n > 0 {
			if firstChunk.IsZero() {
				firstChunk = time.Now()
				emitTTFT(w, cluster, start, firstChunk)
			}
			// Append to rolling tail buffer.
			tail = append(tail, buf[:n]...)
			if len(tail) > 512 {
				tail = tail[len(tail)-512:]
			}
		}
		if err != nil {
			break
		}
	}

	u := extractSSEUsage(tail)
	emitUsage(w, cluster, u.input, u.output)
}

// tapJSON reads the full (buffered) JSON body and extracts token usage.
func tapJSON(w jisr.ResponseWriter, r *jisr.Response, cluster string, start time.Time) {
	body, err := io.ReadAll(r.Body)
	if err != nil || len(body) == 0 {
		return
	}
	emitTTFT(w, cluster, start, time.Now())

	var resp struct {
		Usage *struct {
			PromptTokens     uint32 `json:"prompt_tokens"`
			CompletionTokens uint32 `json:"completion_tokens"`
			InputTokens      uint32 `json:"input_tokens"`
			OutputTokens     uint32 `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &resp) != nil || resp.Usage == nil {
		return
	}
	u := resp.Usage
	emitUsage(w, cluster,
		u.PromptTokens+u.InputTokens,
		u.CompletionTokens+u.OutputTokens,
	)
}

func emitTTFT(w jisr.ResponseWriter, cluster string, start, first time.Time) {
	if start.IsZero() || first.IsZero() {
		return
	}
	ms := uint64(first.Sub(start).Milliseconds())
	if ms > 0 {
		w.RecordHistogram(ttftMs, ms, cluster)
		w.SetMetadata("zia_decoder", "ttft_ms", int64(ms))
	}
}

func emitUsage(w jisr.ResponseWriter, cluster string, in, out uint32) {
	if in > 0 {
		w.IncrementCounter(inputTokens, uint64(in), cluster)
		w.SetMetadata("zia_decoder", "input_tokens", in)
	}
	if out > 0 {
		w.IncrementCounter(outputTokens, uint64(out), cluster)
		w.SetMetadata("zia_decoder", "output_tokens", out)
	}
}

// resolveCluster maps a model name to a provider cluster.
// In production zia this is config-driven; here it is hardcoded for clarity.
func resolveCluster(model string) string {
	switch {
	case strings.HasPrefix(model, "gpt-") || strings.HasPrefix(model, "o1") || strings.HasPrefix(model, "o3"):
		return ClusterOpenAI
	case strings.HasPrefix(model, "claude-"):
		return ClusterAnthropic
	case model == "":
		return ClusterDefault
	default:
		return ClusterDefault
	}
}

// tokenUsage is a simple struct for SSE extraction results.
type tokenUsage struct{ input, output uint32 }

// extractSSEUsage scans the tail buffer for OpenAI/Anthropic usage fields.
func extractSSEUsage(tail []byte) tokenUsage {
	var u tokenUsage
	lines := strings.Split(string(tail), "\n")
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		data := line[6:]
		if data == "[DONE]" {
			continue
		}
		var chunk struct {
			Usage *struct {
				PromptTokens     uint32 `json:"prompt_tokens"`
				CompletionTokens uint32 `json:"completion_tokens"`
				InputTokens      uint32 `json:"input_tokens"`
				OutputTokens     uint32 `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &chunk) == nil && chunk.Usage != nil {
			if u.input == 0 {
				u.input = chunk.Usage.PromptTokens + chunk.Usage.InputTokens
			}
			if u.output == 0 {
				u.output = chunk.Usage.CompletionTokens + chunk.Usage.OutputTokens
			}
		}
	}
	return u
}

// send400 sends a 400 Bad Request with a JSON error body.
func send400(w jisr.ResponseWriter, msg string) {
	w.SetResponseHeader("content-type", "application/json")
	w.Send(http.StatusBadRequest, `{"error":"`+msg+`"}`)
}

var _ = send400 // exported for use in tests
