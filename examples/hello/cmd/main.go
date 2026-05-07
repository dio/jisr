// Package main builds the hello example as a standalone Envoy dynamic module.
//
// Build:
//
//	make
//	# or manually:
//	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o hello.so ./cmd
package main

import (
	sdk "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go"
	_ "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/abi"

	"github.com/dio/jisr"
	_ "github.com/dio/jisr/examples/hello" // registers handlers via init()
)

func init() {
	sdk.RegisterHttpFilterConfigFactories(jisr.WellKnownHttpFilterConfigFactories())
}

func main() {}
