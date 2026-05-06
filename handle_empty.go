package jisr

import "github.com/envoyproxy/envoy/source/extensions/dynamic_modules/sdk/go/shared"

// EmptyHttpFilterHandle provides no-op implementations of all
// shared.HttpFilterHandle methods. Embed it in test fakes to avoid
// implementing the full interface manually.
type EmptyHttpFilterHandle struct{}

func (EmptyHttpFilterHandle) GetMetadataString(shared.MetadataSourceType, string, string) (shared.UnsafeEnvoyBuffer, bool) {
	return shared.UnsafeEnvoyBuffer{}, false
}
func (EmptyHttpFilterHandle) GetMetadataNumber(shared.MetadataSourceType, string, string) (float64, bool) {
	return 0, false
}
func (EmptyHttpFilterHandle) GetMetadataBool(shared.MetadataSourceType, string, string) (bool, bool) {
	return false, false
}
func (EmptyHttpFilterHandle) SetMetadata(string, string, any) {}
func (EmptyHttpFilterHandle) GetMetadataKeys(shared.MetadataSourceType, string) []shared.UnsafeEnvoyBuffer {
	return nil
}
func (EmptyHttpFilterHandle) GetMetadataNamespaces(shared.MetadataSourceType) []shared.UnsafeEnvoyBuffer {
	return nil
}
func (EmptyHttpFilterHandle) AddMetadataListNumber(string, string, float64) bool { return false }
func (EmptyHttpFilterHandle) AddMetadataListString(string, string, string) bool  { return false }
func (EmptyHttpFilterHandle) AddMetadataListBool(string, string, bool) bool      { return false }
func (EmptyHttpFilterHandle) GetMetadataListSize(shared.MetadataSourceType, string, string) (int, bool) {
	return 0, false
}
func (EmptyHttpFilterHandle) GetMetadataListNumber(shared.MetadataSourceType, string, string, int) (float64, bool) {
	return 0, false
}
func (EmptyHttpFilterHandle) GetMetadataListString(shared.MetadataSourceType, string, string, int) (shared.UnsafeEnvoyBuffer, bool) {
	return shared.UnsafeEnvoyBuffer{}, false
}
func (EmptyHttpFilterHandle) GetMetadataListBool(shared.MetadataSourceType, string, string, int) (bool, bool) {
	return false, false
}
func (EmptyHttpFilterHandle) GetFilterState(string) (shared.UnsafeEnvoyBuffer, bool) {
	return shared.UnsafeEnvoyBuffer{}, false
}
func (EmptyHttpFilterHandle) SetFilterState(string, []byte)                     {}
func (EmptyHttpFilterHandle) SetFilterStateTyped(string, []byte) bool           { return false }
func (EmptyHttpFilterHandle) GetFilterStateTyped(string) (shared.UnsafeEnvoyBuffer, bool) {
	return shared.UnsafeEnvoyBuffer{}, false
}
func (EmptyHttpFilterHandle) GetAttributeString(shared.AttributeID) (shared.UnsafeEnvoyBuffer, bool) {
	return shared.UnsafeEnvoyBuffer{}, false
}
func (EmptyHttpFilterHandle) GetAttributeNumber(shared.AttributeID) (float64, bool) { return 0, false }
func (EmptyHttpFilterHandle) GetAttributeBool(shared.AttributeID) (bool, bool)      { return false, false }
func (EmptyHttpFilterHandle) GetData(string) any                                     { return nil }
func (EmptyHttpFilterHandle) SetData(string, any)                                    {}
func (EmptyHttpFilterHandle) SendLocalResponse(uint32, [][2]string, []byte, string)  {}
func (EmptyHttpFilterHandle) SendResponseHeaders([][2]string, bool)                  {}
func (EmptyHttpFilterHandle) SendResponseData([]byte, bool)                          {}
func (EmptyHttpFilterHandle) SendResponseTrailers([][2]string)                       {}
func (EmptyHttpFilterHandle) AddCustomFlag(string)                                   {}
func (EmptyHttpFilterHandle) ContinueRequest()                                       {}
func (EmptyHttpFilterHandle) ContinueResponse()                                      {}
func (EmptyHttpFilterHandle) ClearRouteCache()                                       {}
func (EmptyHttpFilterHandle) RefreshRouteCluster()                                   {}
func (EmptyHttpFilterHandle) RequestHeaders() shared.HeaderMap                       { return nil }
func (EmptyHttpFilterHandle) BufferedRequestBody() shared.BodyBuffer                 { return nil }
func (EmptyHttpFilterHandle) ReceivedRequestBody() shared.BodyBuffer                 { return nil }
func (EmptyHttpFilterHandle) RequestTrailers() shared.HeaderMap                      { return nil }
func (EmptyHttpFilterHandle) ResponseHeaders() shared.HeaderMap                      { return nil }
func (EmptyHttpFilterHandle) BufferedResponseBody() shared.BodyBuffer                { return nil }
func (EmptyHttpFilterHandle) ReceivedResponseBody() shared.BodyBuffer                { return nil }
func (EmptyHttpFilterHandle) ReceivedBufferedRequestBody() bool                      { return false }
func (EmptyHttpFilterHandle) ReceivedBufferedResponseBody() bool                     { return false }
func (EmptyHttpFilterHandle) ResponseTrailers() shared.HeaderMap                     { return nil }
func (EmptyHttpFilterHandle) GetMostSpecificConfig() any                             { return nil }
func (EmptyHttpFilterHandle) GetScheduler() shared.Scheduler                         { return nil }
func (EmptyHttpFilterHandle) Log(shared.LogLevel, string, ...any)                    {}
func (EmptyHttpFilterHandle) HttpCallout(string, [][2]string, []byte, uint64, shared.HttpCalloutCallback) (shared.HttpCalloutInitResult, uint64) {
	return shared.HttpCalloutInitSuccess, 0
}
func (EmptyHttpFilterHandle) StartHttpStream(string, [][2]string, []byte, bool, uint64, shared.HttpStreamCallback) (shared.HttpCalloutInitResult, uint64) {
	return shared.HttpCalloutInitSuccess, 0
}
func (EmptyHttpFilterHandle) SendHttpStreamData(uint64, []byte, bool) bool    { return false }
func (EmptyHttpFilterHandle) SendHttpStreamTrailers(uint64, [][2]string) bool { return false }
func (EmptyHttpFilterHandle) ResetHttpStream(uint64)                          {}
func (EmptyHttpFilterHandle) SetDownstreamWatermarkCallbacks(shared.DownstreamWatermarkCallbacks) {
}
func (EmptyHttpFilterHandle) ClearDownstreamWatermarkCallbacks() {}
func (EmptyHttpFilterHandle) RecordHistogramValue(shared.MetricID, uint64, ...string) shared.MetricsResult {
	return shared.MetricsSuccess
}
func (EmptyHttpFilterHandle) SetGaugeValue(shared.MetricID, uint64, ...string) shared.MetricsResult {
	return shared.MetricsSuccess
}
func (EmptyHttpFilterHandle) IncrementGaugeValue(shared.MetricID, uint64, ...string) shared.MetricsResult {
	return shared.MetricsSuccess
}
func (EmptyHttpFilterHandle) DecrementGaugeValue(shared.MetricID, uint64, ...string) shared.MetricsResult {
	return shared.MetricsSuccess
}
func (EmptyHttpFilterHandle) IncrementCounterValue(shared.MetricID, uint64, ...string) shared.MetricsResult {
	return shared.MetricsSuccess
}

// EmptyHttpFilterConfigHandle is a no-op implementation of shared.HttpFilterConfigHandle.
// Use in tests to exercise ConfigFunc and configHandleImpl without a live Envoy instance.
type EmptyHttpFilterConfigHandle struct{}

func (EmptyHttpFilterConfigHandle) Log(_ shared.LogLevel, _ string, _ ...any) {}
func (EmptyHttpFilterConfigHandle) DefineCounter(_ string, _ ...string) (shared.MetricID, shared.MetricsResult) {
	return 0, shared.MetricsSuccess
}
func (EmptyHttpFilterConfigHandle) DefineHistogram(_ string, _ ...string) (shared.MetricID, shared.MetricsResult) {
	return 0, shared.MetricsSuccess
}
func (EmptyHttpFilterConfigHandle) DefineGauge(_ string, _ ...string) (shared.MetricID, shared.MetricsResult) {
	return 0, shared.MetricsSuccess
}
func (EmptyHttpFilterConfigHandle) GetScheduler() shared.Scheduler { return nil }
