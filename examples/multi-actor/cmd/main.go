// Package main builds the multi-actor example as a standalone Envoy dynamic module.
//
// Build:
//
//	CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o libmulti-actor.so ./cmd
package main

import (
	sdk "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go"
	_ "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/abi"

	"github.com/dio/jisr"
	_ "github.com/dio/jisr/examples/multi-actor"
)

func init() {
	sdk.RegisterHttpFilterConfigFactories(jisr.WellKnownHttpFilterConfigFactories())
}

func main() {}
