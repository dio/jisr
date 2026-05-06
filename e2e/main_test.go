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
	echoAddr  = "127.0.0.1:8080"
)

var (
	projectRoot string
	envoyCmd    *exec.Cmd
	echoServer  *http.Server
)

func TestMain(m *testing.M) {
	_, file, _, _ := runtime.Caller(0)
	// e2e/ is one level below the repo root; examples/hello is what we test.
	projectRoot = filepath.Join(filepath.Dir(file), "..", "examples", "hello")

	// 1. Start the echo backend (pure Go, no Python needed).
	startEchoBackend()

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

	// 3. Start Envoy.
	envoyBin := os.Getenv("ENVOY_BIN")
	if envoyBin == "" {
		home, _ := os.UserHomeDir()
		envoyBin = filepath.Join(home, ".local/share/boe/envoy-versions/1.37.1/bin/envoy")
	}
	envoyCmd = exec.Command(envoyBin,
		"-c", filepath.Join(projectRoot, "envoy.yaml"),
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
	fmt.Fprintf(os.Stderr, "e2e: envoy pid=%d\n", envoyCmd.Process.Pid)

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

// startEchoBackend starts a pure-Go HTTP server on :8080 that reflects all
// received headers back as JSON. This lets tests verify what headers Envoy
// injected into the upstream request.
func startEchoBackend() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		headers := make(map[string]string, len(r.Header))
		for k, v := range r.Header {
			headers[k] = v[0]
		}
		body := map[string]any{
			"path":    r.URL.Path,
			"method":  r.Method,
			"headers": headers,
		}
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(body)
	})

	ln, err := net.Listen("tcp", echoAddr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: echo backend listen failed: %v\n", err)
		os.Exit(1)
	}
	echoServer = &http.Server{Handler: mux}
	go echoServer.Serve(ln)
	fmt.Fprintf(os.Stderr, "e2e: echo backend listening on %s\n", echoAddr)
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
