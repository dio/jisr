// Package main builds the spa example as a standalone Envoy dynamic module.
//
// Build (requires ui/dist to exist first):
//
//	make           # runs vite build then go build
//	make build-so  # skip vite, rebuild .so only
package main

import (
	sdk "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go"
	_ "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/abi"

	"github.com/dio/jisr"
	_ "github.com/dio/jisr/examples/spa" // registers spa + api-backend via init()
)

func init() {
	sdk.RegisterHttpFilterConfigFactories(jisr.WellKnownHttpFilterConfigFactories())
}

func main() {}
