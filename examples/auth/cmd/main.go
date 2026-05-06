// Package main builds the auth example as a standalone Envoy dynamic module.
//
// Build:
//
//	make
//	# or:
//	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o libauth.so ./cmd
package main

import (
	_ "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/abi"

	sdk "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go"

	"github.com/dio/jisr"
	_ "github.com/dio/jisr/examples/auth"
)

func init() {
	sdk.RegisterHttpFilterConfigFactories(jisr.WellKnownHttpFilterConfigFactories())
}

func main() {}
