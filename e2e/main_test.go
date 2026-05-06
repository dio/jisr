//go:build e2e

package e2e

import (
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
	envoyAddr = "http://localhost:10000"
	adminAddr = "http://localhost:9901"
)

var (
	projectRoot string
	envoyCmd    *exec.Cmd
	echoServer  *http.Server
	echoPort    int
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	projectRoot = filepath.Join(filepath.Dir(file), "..", "examples", "hello")

	// 1. Start the echo backend on a random free port.
	echoPort = startEchoBackend()

	// 2. Build libhello.so unless JISR_SKIP_BUILD=1.
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

	// 3. Start Envoy, injecting the random echo port via an env var that
	//    envoy.yaml reads with %REF% or the simpler approach: write a
	//    temp config with the port substituted.
	envoyBin := os.Getenv("ENVOY_BIN")
	if envoyBin == "" {
		home, _ := os.UserHomeDir()
		envoyBin = filepath.Join(home, ".local/share/boe/envoy-versions/1.37.1/bin/envoy")
	}

	// Write a temp envoy config with the actual echo backend port substituted.
	cfgPath := writeEnvoyConfig(echoPort)
	defer os.Remove(cfgPath)

	envoyCmd = exec.Command(envoyBin,
		"-c", cfgPath,
		"--log-level", "warning",
	)
	envoyCmd.Env = append(os.Environ(),
		"GODEBUG=cgocheck=0",
		"ENVOY_DYNAMIC_MODULES_SEARCH_PATH="+projectRoot,
	)
	envoyCmd.Stdout = os.Stderr
	envoyCmd.Stderr = os.Stderr
	if err := envoyCmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: envoy start failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "e2e: envoy pid=%d, echo backend port=%d\n", envoyCmd.Process.Pid, echoPort)

	if !waitReady(15 * time.Second) {
		envoyCmd.Process.Kill()
		fmt.Fprintln(os.Stderr, "e2e: envoy not ready in time")
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "e2e: envoy ready")

	code := m.Run()

	envoyCmd.Process.Kill()
	envoyCmd.Wait()
	echoServer.Close()
	os.Exit(code)
}

// startEchoBackend binds on a random free port and returns the port number.
func startEchoBackend() int {
	mux := http.NewServeMux()
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

	// :0 → OS assigns a free port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: echo backend listen failed: %v\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	echoServer = &http.Server{Handler: mux}
	go echoServer.Serve(ln)
	fmt.Fprintf(os.Stderr, "e2e: echo backend listening on 127.0.0.1:%d\n", port)
	return port
}

// writeEnvoyConfig creates a temp file with the backend port substituted in.
func writeEnvoyConfig(backendPort int) string {
	tmpl := fmt.Sprintf(`
admin:
  address:
    socket_address: { address: 127.0.0.1, port_value: 9901 }

static_resources:
  listeners:
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
`, backendPort)

	f, err := os.CreateTemp("", "jisr-e2e-envoy-*.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: failed to create temp config: %v\n", err)
		os.Exit(1)
	}
	f.WriteString(tmpl)
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
