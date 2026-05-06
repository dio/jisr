module github.com/dio/jisr/examples/hello

go 1.26.2

require (
	github.com/dio/jisr v0.2.1
	github.com/envoyproxy/envoy/source/extensions/dynamic_modules v0.0.0-20260423231439-f1dd21b16c24
)

replace github.com/dio/jisr => ../..
