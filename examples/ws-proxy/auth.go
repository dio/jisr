package wsproxy

import (
	"context"
	"os"
	"strings"

	"github.com/dio/jisr"
)

const AuthExtensionName = "ws-auth"

func init() {
	jisr.Register(AuthExtensionName, jisr.Chain(authHandler, authLogMiddleware))
}

// AuthConfig holds credentials for a single upstream cluster.
// Loaded once at filter config time from environment variables.
type AuthConfig struct {
	// StripHeaders are request headers removed before forwarding upstream
	// (e.g. "authorization" — strip the client's key, inject the provider's).
	StripHeaders []string

	// InjectHeaders are headers added to the upstream request.
	// Values support ${ENV_VAR} expansion.
	InjectHeaders map[string]string
}

// DefaultAuthConfig returns a default config reading from well-known env vars.
func DefaultAuthConfig() AuthConfig {
	return AuthConfig{
		StripHeaders: []string{"authorization", "x-api-key"},
		InjectHeaders: map[string]string{
			"authorization": "Bearer ${OPENAI_API_KEY}",
		},
	}
}

// resolveValue expands ${ENV_VAR} references in v.
func resolveValue(v string) string {
	if !strings.HasPrefix(v, "${") || !strings.HasSuffix(v, "}") {
		return v
	}
	name := v[2 : len(v)-1]
	if val := os.Getenv(name); val != "" {
		return val
	}
	return v // return unexpanded if not set
}

// authHandler is a jisr HandlerFunc that runs as an upstream HTTP filter.
// It strips client auth headers and injects provider credentials.
//
// As an upstream filter it runs on the connection to the upstream cluster —
// not on the downstream request. This is the correct place to inject
// provider-specific credentials (Bearer tokens, x-api-key, etc.).
//
// Note: upstream filters in Envoy are configured under
// typed_extension_protocol_options on the cluster, not in the listener's
// http_filters chain. See envoy.yaml for the correct placement.
func authHandler(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
	r.SkipBody() // upstream filter: only touches headers

	cfg := DefaultAuthConfig()

	for _, h := range cfg.StripHeaders {
		// Use canonical form to match how jisr copies headers.
		r.Header.Del(h)
		w.SetRequestHeader(h, "") // signal jisr to remove it
	}

	for k, v := range cfg.InjectHeaders {
		resolved := resolveValue(v)
		if resolved != v { // only inject if env var was found
			w.SetRequestHeader(k, resolved)
		}
	}
}

func authLogMiddleware(next jisr.HandlerFunc) jisr.HandlerFunc {
	return func(ctx context.Context, w jisr.ResponseWriter, r *jisr.Request) {
		r.Log(jisr.LogDebug, "[%s] upstream auth: injecting credentials", r.FilterName)
		next(ctx, w, r)
	}
}
