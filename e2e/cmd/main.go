// Package main builds the e2e dynamic module used by the integration suite.
//
// It intentionally registers multiple example filters in one .so so the e2e
// Envoy process can exercise regular jisr filters and embedded actor filters
// with a single dynamic module search path.
package main

import (
	sdk "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go"
	_ "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/abi"

	"github.com/dio/jisr"
	_ "github.com/dio/jisr/examples/hello"
	_ "github.com/dio/jisr/examples/ws-proxy"
)

func init() {
	sdk.RegisterHttpFilterConfigFactories(jisr.WellKnownHttpFilterConfigFactories())
}

func main() {}
