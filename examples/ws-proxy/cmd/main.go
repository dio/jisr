// Package main builds the ws-proxy example as a standalone Envoy dynamic module.
//
// Build:
//
//	make
//	# or:
//	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o libws-proxy.so ./cmd
package main

import (
	sdk "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go"
	_ "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/abi"

	"github.com/dio/jisr"
	_ "github.com/dio/jisr/examples/ws-proxy" // registers ws-proxy via init()
)

func init() {
	sdk.RegisterHttpFilterConfigFactories(jisr.WellKnownHttpFilterConfigFactories())
}

func main() {}
