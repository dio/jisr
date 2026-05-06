package auth

import (
	"context"
	"net/http"
	"testing"

	"github.com/dio/jisr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAuth constructs an AuthFilter directly — no Envoy, no global state.
// This is the testability advantage of RegisterFactory.
func newAuth(keys ...string) *AuthFilter {
	allowed := make(map[string]struct{}, len(keys))
	for _, k := range keys {
		allowed[k] = struct{}{}
	}
	return &AuthFilter{
		cfg:     &Config{AllowedKeys: keys, MetadataNS: "auth"},
		allowed: allowed,
		// MetricIDs are zero — EmptyHttpFilterHandle stubs are no-ops.
	}
}

// ── mock ResponseWriter ───────────────────────────────────────────────────────

type mockWriter struct {
	code    int
	body    string
	reqHdrs map[string]string
	meta    map[string]any
}

func newMock() *mockWriter {
	return &mockWriter{reqHdrs: map[string]string{}, meta: map[string]any{}}
}

func (m *mockWriter) Send(code int, body string)                              { m.code = code; m.body = body }
func (m *mockWriter) SendBytes(code int, body []byte)                         { m.code = code; m.body = string(body) }
func (m *mockWriter) SetRequestHeader(k, v string)                            { m.reqHdrs[k] = v }
func (m *mockWriter) SetResponseHeader(_, _ string)                           {}
func (m *mockWriter) SetMetadata(_ string, k string, v any)                   { m.meta[k] = v }
func (m *mockWriter) SetUpstreamResponseHeader(_, _ string)                   {}
func (m *mockWriter) ReplaceBody(_ []byte)                                    {}
func (m *mockWriter) ClearRouteCache()                                        {}
func (m *mockWriter) IncrementCounter(_ jisr.MetricID, _ uint64, _ ...string) {}
func (m *mockWriter) RecordHistogram(_ jisr.MetricID, _ uint64, _ ...string)  {}
func (m *mockWriter) Stream(_ context.Context, _ [][2]string) (jisr.StreamWriter, error) {
	return nil, nil
}

func reqWithKey(key string) *jisr.Request {
	h := http.Header{":path": {"/api/data"}, ":method": {"GET"}}
	if key != "" {
		h["X-Api-Key"] = []string{key}
	}
	return &jisr.Request{Header: h}
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestAuth_AllowedKey(t *testing.T) {
	f := newAuth("key-admin", "key-readonly")
	w := newMock()

	f.HandleRequest(context.Background(), w, reqWithKey("key-admin"))

	assert.Equal(t, 0, w.code, "should not send local response")
	assert.Equal(t, "key-admin", w.reqHdrs["x-user-id"])
	assert.Equal(t, "allowed", w.meta["result"])
	assert.Equal(t, "key-admin", w.meta["key"])
}

func TestAuth_RejectedKey(t *testing.T) {
	f := newAuth("key-admin")
	w := newMock()

	f.HandleRequest(context.Background(), w, reqWithKey("bad-key"))

	assert.Equal(t, http.StatusUnauthorized, w.code)
	assert.Contains(t, w.body, "invalid or missing api key")
	assert.Equal(t, "rejected", w.meta["result"])
	assert.Empty(t, w.reqHdrs["x-user-id"])
}

func TestAuth_MissingKey(t *testing.T) {
	f := newAuth("key-admin")
	w := newMock()

	f.HandleRequest(context.Background(), w, reqWithKey(""))

	assert.Equal(t, http.StatusUnauthorized, w.code)
}

func TestAuth_TwoInstances_IndependentConfig(t *testing.T) {
	// This is the core value of RegisterFactory: two instances,
	// each with a different allowed key set, no shared state.
	adminFilter := newAuth("key-admin")
	publicFilter := newAuth("key-public", "key-guest")

	wAdmin := newMock()
	adminFilter.HandleRequest(context.Background(), wAdmin, reqWithKey("key-admin"))
	assert.Equal(t, 0, wAdmin.code, "admin key allowed on admin filter")

	wAdminOnPublic := newMock()
	publicFilter.HandleRequest(context.Background(), wAdminOnPublic, reqWithKey("key-admin"))
	assert.Equal(t, http.StatusUnauthorized, wAdminOnPublic.code, "admin key rejected on public filter")

	wPublic := newMock()
	publicFilter.HandleRequest(context.Background(), wPublic, reqWithKey("key-public"))
	assert.Equal(t, 0, wPublic.code, "public key allowed on public filter")
}

func TestAuth_Response_StatusBucket(t *testing.T) {
	cases := []struct {
		code   int
		bucket string
	}{
		{200, "2xx"},
		{201, "2xx"},
		{400, "4xx"},
		{403, "4xx"},
		{500, "5xx"},
		{503, "5xx"},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.bucket, statusBucket(tc.code), "code=%d", tc.code)
	}
}

func TestAuth_Response_RecordsUpstreamStatus(t *testing.T) {
	f := newAuth("key-admin")
	w := newMock()

	resp := &jisr.Response{StatusCode: 200}
	f.HandleResponse(context.Background(), w, resp)

	require.Equal(t, 200, w.meta["upstream_status"])
}
