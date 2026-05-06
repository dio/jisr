package boehello

import (
	"fmt"
	"os"

	"github.com/dio/jisr/prof"
)

// AdminPort is the TCP port of the pprof/admin server started when the .so loads.
// Zero means the server failed to start (bind error). Read from e2e tests via
// the JISR_ADMIN_PORT environment variable — we write it on startup.
var AdminPort int

func init() {
	srv, _, err := prof.Start("") // "" = random free loopback port
	if err != nil {
		fmt.Fprintf(os.Stderr, "hello: admin server failed to start: %v\n", err)
		return
	}
	AdminPort = srv.Port()
	// Write port to env so the e2e harness can discover it without shared memory.
	// The .so and the test binary are separate processes, so we use a file instead.
	portFile := os.Getenv("JISR_ADMIN_PORT_FILE")
	if portFile != "" {
		_ = os.WriteFile(portFile, []byte(fmt.Sprintf("%d", AdminPort)), 0o644)
	}
	fmt.Fprintf(os.Stderr, "hello: admin server listening on %s\n", srv.Addr())
}
