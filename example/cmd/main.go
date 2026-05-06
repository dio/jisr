// Package main builds the example jisr filter as a standalone Envoy dynamic module.
//
// Build:
//
//	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared \
//	  -o example-filter.so ./example/cmd
package main

import (
	// abi pulls in all CGo-exported symbols Envoy calls on a native dynamic module.
	_ "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/abi"

	sdk "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go"

	"github.com/dio/jisr"
	_ "github.com/dio/jisr/example" // side-effect: registers handlers via init()
)

func init() {
	sdk.RegisterHttpFilterConfigFactories(jisr.WellKnownHttpFilterConfigFactories())
}

func main() {}
