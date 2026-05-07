//go:build e2e

package e2e

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

const (
	envoyAddr        = "http://localhost:10000" // hello filter
	envoyEchoAddr    = "http://localhost:10001" // hello-echo direct response
	envoyStampAddr   = "http://localhost:10002" // resp-stamp (Passthrough)
	envoyTapAddr     = "http://localhost:10003" // resp-tap (Observe)
	envoyRewriteAddr = "http://localhost:10005" // resp-rewrite (Buffer — JSON body rewrite)
	envoyHStampAddr  = "http://localhost:10006" // resp-header-stamp (Buffer — add header)
	adminAddr        = "http://localhost:9901"
)

var (
	projectRoot  string
	envoyCmd     *exec.Cmd
	echoServer   *http.Server
	resetPort    int    // TCP port that accepts then immediately RSTs — upstream reset-before-connect
	adminPortStr string // "http://127.0.0.1:<port>" of the jisr/prof admin server inside the .so
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	projectRoot = filepath.Join(filepath.Dir(file), "..", "examples", "hello")

	// 1. Start the echo backend on a random free port.
	backendPort := startEchoBackend()

	// 2. Start the RST backend — accepts TCP connections then immediately closes them.
	// Used to simulate upstream connection reset before any data.
	resetPort = startRSTBackend()

	// 3. Build libhello.so unless JISR_SKIP_BUILD=1.
	if os.Getenv("JISR_SKIP_BUILD") == "" {
		soPath := filepath.Join(projectRoot, "libhello.so")
		fmt.Fprintln(os.Stderr, "e2e: building libhello.so …")
		cmd := exec.Command("go", "build", "-trimpath", "-buildmode=c-shared", "-o", soPath, "./cmd")
		cmd.Dir = projectRoot
		cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "e2e: build failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintln(os.Stderr, "e2e: build OK")
	}

	// 4a. Temp file for the admin server port — written by the .so on init.
	portFile, err := os.CreateTemp("", "jisr-admin-port-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: failed to create port file: %v\n", err)
		os.Exit(1)
	}
	portFile.Close()
	defer os.Remove(portFile.Name())

	// 4. Start Envoy with a generated config.
	envoyBin := os.Getenv("ENVOY_BIN")
	if envoyBin == "" {
		home, _ := os.UserHomeDir()
		envoyBin = filepath.Join(home, ".local/share/boe/envoy-versions/1.37.1/bin/envoy")
	}

	cfgPath := writeEnvoyConfig(backendPort, resetPort)
	defer os.Remove(cfgPath)

	envoyCmd = exec.Command(envoyBin, "-c", cfgPath, "--log-level", "warning")
	envoyCmd.Env = append(os.Environ(),
		"GODEBUG=cgocheck=0",
		"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+projectRoot,
		"JISR_ADMIN_PORT_FILE="+portFile.Name(),
	)
	envoyCmd.Stdout = os.Stderr
	envoyCmd.Stderr = os.Stderr
	if err := envoyCmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: envoy start failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "e2e: envoy pid=%d, backend port=%d\n", envoyCmd.Process.Pid, backendPort)

	if !waitReady(15 * time.Second) {
		envoyCmd.Process.Kill()
		fmt.Fprintln(os.Stderr, "e2e: envoy not ready in time")
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	// Read admin port written by the .so on init.
	if raw, err := os.ReadFile(portFile.Name()); err == nil && len(raw) > 0 {
		adminPortStr = "http://127.0.0.1:" + string(raw)
		fmt.Fprintf(os.Stderr, "e2e: jisr admin server at %s\n", adminPortStr)
	} else {
		fmt.Fprintln(os.Stderr, "e2e: WARNING: admin port file empty — prof e2e tests will skip")
	}

	code := m.Run()

	envoyCmd.Process.Kill()
	envoyCmd.Wait()
	echoServer.Close()
	os.Exit(code)
}

// startEchoBackend starts the backend HTTP server on a random port.
// Endpoints:
//
//	/          — JSON echo of path, method, headers
//	/body      — returns a fixed JSON body (useful for byte-count tests)
//	/slow      — writes headers + one chunk, then abruptly closes (upstream disconnect after data)
//	/chunked   — sends body in two known chunks, then closes cleanly
//	/hold      — sends headers + first chunk, then blocks until request context is cancelled
func startEchoBackend() int {
	mux := http.NewServeMux()

	// Default echo handler.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		headers := make(map[string]string, len(r.Header))
		for k, v := range r.Header {
			headers[k] = v[0]
		}
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"path":    r.URL.Path,
			"method":  r.Method,
			"headers": headers,
		})
	})

	// /body — fixed body, useful for byte-count assertions.
	mux.HandleFunc("/body", func(w http.ResponseWriter, r *http.Request) {
		payload := `{"message":"hello from backend","ok":true}`
		w.Header().Set("content-type", "application/json")
		w.Header().Set("content-length", fmt.Sprintf("%d", len(payload)))
		fmt.Fprint(w, payload)
	})

	// /json — structured JSON with known fields; used for body-rewrite e2e tests.
	mux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"service": "backend",
			"version": 1,
		})
	})

	// /chunked — sends body in two chunks then closes cleanly.
	mux.HandleFunc("/chunked", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/plain")
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", 500)
			return
		}
		fmt.Fprint(w, "chunk-one ")
		flusher.Flush()
		time.Sleep(20 * time.Millisecond)
		fmt.Fprint(w, "chunk-two")
		flusher.Flush()
	})

	// /slow — abruptly closes the TCP connection mid-response (upstream disconnect after data).
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijacker", 500)
			return
		}
		conn, buf, _ := hj.Hijack()
		// Write valid HTTP response headers + one complete chunk.
		buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nTransfer-Encoding: chunked\r\n\r\n")
		buf.WriteString("5\r\nhello\r\n") // valid first chunk
		buf.Flush()
		// Abruptly close — no terminating 0-length chunk.
		// Envoy detects the incomplete response and calls OnStreamComplete.
		conn.Close()
	})

	// /hold — sends headers + first chunk, then blocks until the client disconnects.
	// Used to simulate: upstream is alive but slow; client disconnects first.
	mux.HandleFunc("/hold", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", 500)
			return
		}
		w.Header().Set("content-type", "text/plain")
		w.Header().Set("transfer-encoding", "chunked")
		// Send the status line and headers.
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		// Send one chunk so the filter's OnResponseBody fires.
		fmt.Fprint(w, "partial-data")
		flusher.Flush()
		// Block until the client or Envoy drops the connection.
		<-r.Context().Done()
	})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: echo backend listen failed: %v\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	echoServer = &http.Server{Handler: mux}
	go echoServer.Serve(ln)
	fmt.Fprintf(os.Stderr, "e2e: backend listening on 127.0.0.1:%d\n", port)
	return port
}

// startRSTBackend starts a TCP listener that accepts connections and immediately
// closes them — simulating upstream connection reset before any HTTP data.
// Envoy will see a connection error and generate a 503 response to the client.
func startRSTBackend() int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: rst backend listen failed: %v\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed at test teardown
			}
			conn.Close() // immediate RST — no data written
		}
	}()
	fmt.Fprintf(os.Stderr, "e2e: rst backend listening on 127.0.0.1:%d\n", port)
	return port
}

// writeEnvoyConfig writes a temp envoy config with backend/reset ports substituted.
func writeEnvoyConfig(backendPort, rstPort int) string {
	cfg := fmt.Sprintf(`
admin:
  address:
    socket_address: { address: 127.0.0.1, port_value: 9901 }

static_resources:
  listeners:
    # Port 10000 — hello filter: injects x-hello and forwards to backend.
    - name: main
      address:
        socket_address: { address: 0.0.0.0, port_value: 10000 }
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: ingress
                http_filters:
                  - name: hello
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                      dynamic_module_config:
                        name: hello
                      filter_name: hello
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: local
                  virtual_hosts:
                    - name: backend
                      domains: ["*"]
                      routes:
                        - match: { prefix: "/" }
                          route: { cluster: backend }

    # Port 10001 — hello-echo filter: responds directly, never hits upstream.
    - name: echo
      address:
        socket_address: { address: 0.0.0.0, port_value: 10001 }
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: echo
                http_filters:
                  - name: hello-echo
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                      dynamic_module_config:
                        name: hello
                      filter_name: hello-echo
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: echo
                  virtual_hosts:
                    - name: blackhole
                      domains: ["*"]
                      routes:
                        - match: { prefix: "/" }
                          route: { cluster: blackhole }

    # Port 10002 — resp-stamp filter: Passthrough, inspects response headers.
    - name: resp-stamp
      address:
        socket_address: { address: 0.0.0.0, port_value: 10002 }
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: resp_stamp
                http_filters:
                  - name: resp-stamp
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                      dynamic_module_config:
                        name: hello
                      filter_name: resp-stamp
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: stamp
                  virtual_hosts:
                    - name: backend
                      domains: ["*"]
                      routes:
                        - match: { prefix: "/" }
                          route: { cluster: backend }

    # Port 10003 — resp-tap filter: Observe, stamps x-jisr-body-bytes on response.
    - name: resp-tap
      address:
        socket_address: { address: 0.0.0.0, port_value: 10003 }
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: resp_tap
                http_filters:
                  - name: resp-tap
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                      dynamic_module_config:
                        name: hello
                      filter_name: resp-tap
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: tap
                  virtual_hosts:
                    - name: backend
                      domains: ["*"]
                      routes:
                        - match: { prefix: "/" }
                          route: { cluster: backend }

    # Port 10004 — resp-tap on noconnect cluster (upstream RST before connect).
    - name: resp-tap-rst
      address:
        socket_address: { address: 0.0.0.0, port_value: 10004 }
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: resp_tap_rst
                http_filters:
                  - name: resp-tap
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                      dynamic_module_config:
                        name: hello
                      filter_name: resp-tap
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: tap_rst
                  virtual_hosts:
                    - name: noconnect
                      domains: ["*"]
                      routes:
                        - match: { prefix: "/" }
                          route: { cluster: noconnect }

    # Port 10005 — resp-rewrite filter: Buffer mode, rewrites JSON response body.
    - name: resp-rewrite
      address:
        socket_address: { address: 0.0.0.0, port_value: 10005 }
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: resp_rewrite
                http_filters:
                  - name: resp-rewrite
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                      dynamic_module_config:
                        name: hello
                      filter_name: resp-rewrite
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: rewrite
                  virtual_hosts:
                    - name: backend
                      domains: ["*"]
                      routes:
                        - match: { prefix: "/" }
                          route: { cluster: backend }

    # Port 10006 — resp-header-stamp filter: Buffer, adds x-jisr-stamp header.
    - name: resp-header-stamp
      address:
        socket_address: { address: 0.0.0.0, port_value: 10006 }
      filter_chains:
        - filters:
            - name: envoy.filters.network.http_connection_manager
              typed_config:
                "@type": type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager
                stat_prefix: resp_header_stamp
                http_filters:
                  - name: resp-header-stamp
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.dynamic_modules.v3.DynamicModuleFilter
                      dynamic_module_config:
                        name: hello
                      filter_name: resp-header-stamp
                  - name: envoy.filters.http.router
                    typed_config:
                      "@type": type.googleapis.com/envoy.extensions.filters.http.router.v3.Router
                route_config:
                  name: hstamp
                  virtual_hosts:
                    - name: backend
                      domains: ["*"]
                      routes:
                        - match: { prefix: "/" }
                          route: { cluster: backend }

  clusters:
    - name: backend
      type: STATIC
      load_assignment:
        cluster_name: backend
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address: { address: 127.0.0.1, port_value: %d }

    # noconnect — points to RST backend (accepts then immediately closes).
    - name: noconnect
      type: STATIC
      load_assignment:
        cluster_name: noconnect
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address: { address: 127.0.0.1, port_value: %d }

    # Never reached — hello-echo always responds directly via w.SendBytes.
    - name: blackhole
      type: STATIC
      load_assignment:
        cluster_name: blackhole
        endpoints:
          - lb_endpoints:
              - endpoint:
                  address:
                    socket_address: { address: 127.0.0.1, port_value: 1 }
`, backendPort, rstPort)

	f, err := os.CreateTemp("", "jisr-e2e-*.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: failed to create temp config: %v\n", err)
		os.Exit(1)
	}
	f.WriteString(cfg)
	f.Close()
	return f.Name()
}

func waitReady(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(adminAddr + "/ready")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false
}

// envoyHealthy checks that Envoy is still serving via the admin endpoint.
func envoyHealthy(t *testing.T) bool {
	t.Helper()
	resp, err := http.Get(adminAddr + "/ready")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// dialAndSendRequest opens a raw TCP connection to addr, sends a minimal
// HTTP/1.1 GET request, and returns the connection (caller owns close).
func dialAndSendRequest(addr, path string) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n", path)
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// readStatusLine reads just the first line of an HTTP response.
func readStatusLine(conn net.Conn) (string, error) {
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(conn)
	return r.ReadString('\n')
}
