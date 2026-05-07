package spa_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"strings"
	"testing"

	"github.com/dio/jisr"
	spa "github.com/dio/jisr/examples/spa"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── mockWriter ────────────────────────────────────────────────────────────────

type mockWriter struct {
	code    int
	body    []byte
	headers map[string]string
}

func newMock() *mockWriter { return &mockWriter{headers: map[string]string{}} }

func (m *mockWriter) Send(code int, body string)                              { m.code = code; m.body = []byte(body) }
func (m *mockWriter) SendBytes(code int, body []byte)                         { m.code = code; m.body = body }
func (m *mockWriter) SetRequestHeader(k, v string)                            {}
func (m *mockWriter) SetResponseHeader(k, v string)                           { m.headers[k] = v }
func (m *mockWriter) SetMetadata(_, _ string, _ any)                          {}
func (m *mockWriter) SetUpstreamResponseHeader(_, _ string)                   {}
func (m *mockWriter) ReplaceBody(_ []byte)                                    {}
func (m *mockWriter) ClearRouteCache()                                        {}
func (m *mockWriter) IncrementCounter(_ jisr.MetricID, _ uint64, _ ...string) {}
func (m *mockWriter) RecordHistogram(_ jisr.MetricID, _ uint64, _ ...string)  {}
func (m *mockWriter) Stream(_ context.Context, _ [][2]string) (jisr.StreamWriter, error) {
	return nil, nil
}

func req(path string) *jisr.Request {
	return &jisr.Request{
		Header: http.Header{
			":path":   {path},
			":method": {"GET"},
		},
		Body: strings.NewReader(""),
	}
}

// assetPath returns the first embedded asset path matching the given extension.
// Vite fingerprints filenames (e.g. index-4zgxRtfG.css), so tests must
// discover the actual filename rather than hardcoding it.
func assetPath(t *testing.T, ext string) string {
	t.Helper()
	var found string
	fs.WalkDir(spa.UIFS, "ui/dist/assets", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasSuffix(path, ext) {
			// Strip "ui/dist" prefix to get the URL path.
			found = strings.TrimPrefix(path, "ui/dist")
		}
		return nil
	})
	require.NotEmpty(t, found, "no asset with extension %q found in ui/dist/assets", ext)
	return found
}

// ── SPA handler tests ─────────────────────────────────────────────────────────

func TestSPA_ServesIndexHTML(t *testing.T) {
	w := newMock()
	spa.SPAHandler(context.Background(), w, req("/"))

	assert.Equal(t, http.StatusOK, w.code)
	assert.Contains(t, w.headers["content-type"], "text/html")
	assert.Contains(t, string(w.body), "<html")
	// Vite injects the fingerprinted script tag — just check the SPA shell is present.
	assert.Contains(t, string(w.body), `id="root"`)
}

func TestSPA_ServesJSAsset(t *testing.T) {
	js := assetPath(t, ".js")
	w := newMock()
	spa.SPAHandler(context.Background(), w, req(js))

	assert.Equal(t, http.StatusOK, w.code)
	assert.Contains(t, w.headers["content-type"], "javascript")
	assert.Greater(t, len(w.body), 0)
}

func TestSPA_ServesCSSAsset(t *testing.T) {
	css := assetPath(t, ".css")
	w := newMock()
	spa.SPAHandler(context.Background(), w, req(css))

	assert.Equal(t, http.StatusOK, w.code)
	assert.Contains(t, w.headers["content-type"], "css")
	assert.Greater(t, len(w.body), 0)
}

func TestSPA_FallsBackToIndexHTML_UnknownRoute(t *testing.T) {
	w := newMock()
	spa.SPAHandler(context.Background(), w, req("/dashboard/settings"))

	assert.Equal(t, http.StatusOK, w.code)
	assert.Contains(t, w.headers["content-type"], "text/html")
	assert.Contains(t, string(w.body), "<html")
}

func TestSPA_FallsBackToIndexHTML_DeepRoute(t *testing.T) {
	w := newMock()
	spa.SPAHandler(context.Background(), w, req("/app/users/123/edit"))

	assert.Equal(t, http.StatusOK, w.code)
	assert.Contains(t, w.headers["content-type"], "text/html")
}

func TestSPA_CacheControl_Assets(t *testing.T) {
	js := assetPath(t, ".js")
	w := newMock()
	spa.SPAHandler(context.Background(), w, req(js))

	assert.Equal(t, http.StatusOK, w.code)
	assert.Contains(t, w.headers["cache-control"], "immutable")
}

func TestSPA_CacheControl_Index(t *testing.T) {
	w := newMock()
	spa.SPAHandler(context.Background(), w, req("/"))

	assert.Equal(t, "no-cache", w.headers["cache-control"])
}

func TestSPA_StripQueryString(t *testing.T) {
	js := assetPath(t, ".js")
	w := newMock()
	spa.SPAHandler(context.Background(), w, req(js+"?v=123"))

	assert.Equal(t, http.StatusOK, w.code)
	assert.Contains(t, w.headers["content-type"], "javascript")
}

// ── API handler tests ─────────────────────────────────────────────────────────

func TestAPI_Hello(t *testing.T) {
	w := newMock()
	spa.APIHandler(context.Background(), w, req("/api/hello"))

	require.Equal(t, http.StatusOK, w.code)
	assert.Contains(t, w.headers["content-type"], "application/json")

	var resp map[string]any
	require.NoError(t, json.Unmarshal(w.body, &resp))
	assert.Equal(t, "hello from inside the .so", resp["message"])
}

func TestAPI_Time(t *testing.T) {
	w := newMock()
	spa.APIHandler(context.Background(), w, req("/api/time"))

	require.Equal(t, http.StatusOK, w.code)

	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.body, &resp))
	assert.NotEmpty(t, resp["time"])
}

func TestAPI_NotFound(t *testing.T) {
	w := newMock()
	spa.APIHandler(context.Background(), w, req("/api/unknown"))

	assert.Equal(t, http.StatusNotFound, w.code)

	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.body, &resp))
	assert.Equal(t, "not found", resp["error"])
}
