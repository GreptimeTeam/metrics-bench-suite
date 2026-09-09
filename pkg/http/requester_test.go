package http

import (
	"context"
	"errors"
	"io"
	nethttp "net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
	writev2 "github.com/prometheus/prometheus/prompb/io/prometheus/write/v2"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

func TestRequesterSendIncludesConfiguredHeader(t *testing.T) {
	var gotHeaders nethttp.Header
	server := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		gotHeaders = r.Header.Clone()
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(nethttp.StatusNoContent)
	}))
	defer server.Close()

	requester := NewRequester(server.URL)
	requester.SetHeader("Authorization", "Basic YWxpY2U6c2VjcmV0")
	if err := requester.Send(prompb.WriteRequest{}); err != nil {
		t.Fatalf("send request: %v", err)
	}
	if got := gotHeaders.Get("Authorization"); got != "Basic YWxpY2U6c2VjcmV0" {
		t.Fatalf("unexpected authorization header %q", got)
	}
	if got := gotHeaders.Get("Content-Type"); got != contentTypeV1 {
		t.Fatalf("content type = %q, want %q", got, contentTypeV1)
	}
	if got := gotHeaders.Get(remoteWriteVersionHeader); got != remoteWriteVersionV1 {
		t.Fatalf("remote write version = %q, want %q", got, remoteWriteVersionV1)
	}
	if got := gotHeaders.Get("User-Agent"); got != userAgent {
		t.Fatalf("user agent = %q, want %q", got, userAgent)
	}
}

func TestRequesterSendsPrometheusRemoteWrite(t *testing.T) {
	var body []byte
	server := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(nethttp.StatusNoContent)
	}))
	defer server.Close()

	writeRequest := prompb.WriteRequest{
		Timeseries: testTimeSeries(),
		Metadata: []prompb.MetricMetadata{{
			Type:             prompb.MetricMetadata_COUNTER,
			MetricFamilyName: "cpu_usage",
		}},
	}
	if err := NewRequester(server.URL).Send(writeRequest); err != nil {
		t.Fatalf("send Prometheus request: %v", err)
	}
	decoded, err := snappy.Decode(nil, body)
	if err != nil {
		t.Fatalf("decode Snappy body: %v", err)
	}
	var request prompb.WriteRequest
	if err := request.Unmarshal(decoded); err != nil {
		t.Fatalf("unmarshal Prometheus request: %v", err)
	}
	if len(request.Metadata) != 1 || request.Metadata[0].MetricFamilyName != "cpu_usage" {
		t.Fatalf("unexpected metadata: %v", request.Metadata)
	}
}

func TestRequesterSendV2(t *testing.T) {
	var (
		gotHeaders nethttp.Header
		gotRequest writev2.Request
		decodeErr  error
	)
	server := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		gotHeaders = r.Header.Clone()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			decodeErr = err
		} else if body, decodeErr = snappy.Decode(nil, body); decodeErr == nil {
			decodeErr = gotRequest.Unmarshal(body)
		}
		w.Header().Set(samplesWrittenHeader, "1")
		w.WriteHeader(nethttp.StatusNoContent)
	}))
	defer server.Close()

	request := writev2.Request{
		Symbols: []string{"", "__name__", "up"},
		Timeseries: []writev2.TimeSeries{{
			LabelsRefs: []uint32{1, 2},
			Samples:    []writev2.Sample{{Value: 1, Timestamp: 2}},
		}},
	}
	if err := NewRequester(server.URL).SendV2(request); err != nil {
		t.Fatalf("send v2 request: %v", err)
	}
	if decodeErr != nil {
		t.Fatalf("decode request: %v", decodeErr)
	}
	if !reflect.DeepEqual(gotRequest.Symbols, request.Symbols) || !reflect.DeepEqual(gotRequest.Timeseries, request.Timeseries) {
		t.Fatalf("request = %#v, want %#v", gotRequest, request)
	}
	if got := gotHeaders.Get("Content-Type"); got != contentTypeV2 {
		t.Fatalf("content type = %q, want %q", got, contentTypeV2)
	}
	if got := gotHeaders.Get(remoteWriteVersionHeader); got != remoteWriteVersionV2 {
		t.Fatalf("remote write version = %q, want %q", got, remoteWriteVersionV2)
	}
}

func TestRequesterSendV2RejectsWrittenSampleMismatch(t *testing.T) {
	server := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set(samplesWrittenHeader, "0")
		w.WriteHeader(nethttp.StatusNoContent)
	}))
	defer server.Close()

	request := writev2.Request{
		Symbols:    []string{""},
		Timeseries: []writev2.TimeSeries{{Samples: []writev2.Sample{{Value: 1, Timestamp: 2}}}},
	}
	err := NewRequester(server.URL).SendV2(request)
	if err == nil || !strings.Contains(err.Error(), "wrote 0 of 1 samples") {
		t.Fatalf("expected written sample mismatch, got %v", err)
	}
}

func TestRequesterSendsOTLPMetrics(t *testing.T) {
	var body []byte
	var header nethttp.Header
	server := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		body, _ = io.ReadAll(r.Body)
		header = r.Header.Clone()
		w.WriteHeader(nethttp.StatusOK)
	}))
	defer server.Close()

	requester, err := NewRequesterForProtocol(server.URL, ProtocolOTLP)
	if err != nil {
		t.Fatalf("create OTLP requester: %v", err)
	}
	requester.SetHeader("Authorization", "Basic token")
	if err := requester.SendTimeSeries(testTimeSeries()); err != nil {
		t.Fatalf("send OTLP request: %v", err)
	}
	if got := header.Get("Content-Type"); got != contentTypeV1 {
		t.Fatalf("content type = %q, want %q", got, contentTypeV1)
	}
	if got := header.Get("Content-Encoding"); got != "" {
		t.Fatalf("content encoding = %q, want empty", got)
	}
	if got := header.Get("Authorization"); got != "Basic token" {
		t.Fatalf("authorization = %q, want Basic token", got)
	}

	fieldNumber, wireType, tagLength := protowire.ConsumeTag(body)
	if tagLength < 0 || fieldNumber != 1 || wireType != protowire.BytesType {
		t.Fatalf("invalid ExportMetricsServiceRequest field: number=%d type=%d", fieldNumber, wireType)
	}
	resourceMetricsData, valueLength := protowire.ConsumeBytes(body[tagLength:])
	if valueLength < 0 || tagLength+valueLength != len(body) {
		t.Fatal("invalid ResourceMetrics payload")
	}
	var resourceMetrics metricspb.ResourceMetrics
	if err := proto.Unmarshal(resourceMetricsData, &resourceMetrics); err != nil {
		t.Fatalf("unmarshal ResourceMetrics: %v", err)
	}
	metrics := resourceMetrics.ScopeMetrics[0].Metrics
	if len(metrics) != 1 || metrics[0].Name != "cpu_usage" {
		t.Fatalf("unexpected metrics: %v", metrics)
	}
	points := metrics[0].GetGauge().DataPoints
	if len(points) != 2 || points[0].TimeUnixNano != 1_700_000_000_123_000_000 || points[0].GetAsDouble() != 1.5 {
		t.Fatalf("unexpected data points: %v", points)
	}
	if len(points[0].Attributes) != 1 || points[0].Attributes[0].Key != "host" || points[0].Attributes[0].Value.GetStringValue() != "a" {
		t.Fatalf("unexpected attributes: %v", points[0].Attributes)
	}
}

func TestRequesterSendContextCancelsOTLPRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(nethttp.HandlerFunc(func(_ nethttp.ResponseWriter, _ *nethttp.Request) {
		close(started)
		<-release
	}))
	defer server.Close()
	defer close(release)

	requester, err := NewRequesterForProtocol(server.URL, ProtocolOTLP)
	if err != nil {
		t.Fatalf("create OTLP requester: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- requester.SendContext(ctx, prompb.WriteRequest{}) }()
	<-started
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected canceled request error")
		}
	case <-time.After(time.Second):
		t.Fatal("request did not stop after cancellation")
	}
}

func TestNewRequesterForProtocolRejectsUnknownProtocol(t *testing.T) {
	if _, err := NewRequesterForProtocol("http://localhost", "grpc"); err == nil {
		t.Fatal("expected unsupported protocol error")
	}
}

func TestRequesterHandlesOTLPPartialSuccess(t *testing.T) {
	tests := []struct {
		name     string
		rejected uint64
		wantErr  bool
	}{
		{name: "rejected points", rejected: 2, wantErr: true},
		{name: "warning only", rejected: 0, wantErr: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := otlpPartialSuccessResponse(tt.rejected, "invalid points")
			server := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, _ *nethttp.Request) {
				_, _ = w.Write(response)
			}))
			defer server.Close()

			requester, err := NewRequesterForProtocol(server.URL, ProtocolOTLP)
			if err != nil {
				t.Fatalf("create OTLP requester: %v", err)
			}
			err = requester.SendTimeSeries(testTimeSeries())
			if (err != nil) != tt.wantErr {
				t.Fatalf("SendTimeSeries() error = %v, wantErr %t", err, tt.wantErr)
			}
			if tt.rejected > 0 {
				var partialSuccess *PartialSuccessError
				if !errors.As(err, &partialSuccess) || partialSuccess.RejectedDataPoints != tt.rejected {
					t.Fatalf("unexpected partial success error: %v", err)
				}
			}
		})
	}
}

func otlpPartialSuccessResponse(rejected uint64, message string) []byte {
	partialSuccess := protowire.AppendTag(nil, 1, protowire.VarintType)
	partialSuccess = protowire.AppendVarint(partialSuccess, rejected)
	partialSuccess = protowire.AppendTag(partialSuccess, 2, protowire.BytesType)
	partialSuccess = protowire.AppendString(partialSuccess, message)
	response := protowire.AppendTag(nil, 1, protowire.BytesType)
	return protowire.AppendBytes(response, partialSuccess)
}

func TestMarshalOTLPRejectsInvalidTimeSeries(t *testing.T) {
	tests := []prompb.TimeSeries{
		{Samples: []prompb.Sample{{Timestamp: 1}}},
		{Labels: []prompb.Label{{Name: "__name__", Value: "metric"}}, Samples: []prompb.Sample{{Timestamp: -1}}},
	}
	for _, series := range tests {
		if _, err := marshalOTLP([]prompb.TimeSeries{series}); err == nil {
			t.Fatal("expected invalid time series error")
		}
	}
}

func testTimeSeries() []prompb.TimeSeries {
	return []prompb.TimeSeries{
		{
			Labels:  []prompb.Label{{Name: "__name__", Value: "cpu_usage"}, {Name: "host", Value: "a"}},
			Samples: []prompb.Sample{{Value: 1.5, Timestamp: 1_700_000_000_123}},
		},
		{
			Labels:  []prompb.Label{{Name: "__name__", Value: "cpu_usage"}, {Name: "host", Value: "b"}},
			Samples: []prompb.Sample{{Value: 2.5, Timestamp: 1_700_000_000_124}},
		},
	}
}
