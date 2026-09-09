package http

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
	writev2 "github.com/prometheus/prometheus/prompb/io/prometheus/write/v2"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
)

const (
	// ProtocolPrometheus sends Prometheus remote write requests.
	ProtocolPrometheus = "prometheus"
	// ProtocolOTLP sends OTLP Metrics requests over HTTP/protobuf.
	ProtocolOTLP = "otlp"

	contentTypeV1            = "application/x-protobuf"
	contentTypeV2            = contentTypeV1 + ";proto=io.prometheus.write.v2.Request"
	remoteWriteVersionHeader = "X-Prometheus-Remote-Write-Version"
	remoteWriteVersionV1     = "0.1.0"
	remoteWriteVersionV2     = "2.0.0"
	samplesWrittenHeader     = "X-Prometheus-Remote-Write-Samples-Written"
	userAgent                = "metrics-bench-suite"
)

// PartialSuccessError reports data points rejected by an OTLP receiver.
type PartialSuccessError struct {
	RejectedDataPoints uint64
	Message            string
}

func (e *PartialSuccessError) Error() string {
	return fmt.Sprintf("OTLP partial success: rejected data points: %d, message: %s", e.RejectedDataPoints, e.Message)
}

// Requester sends metrics using a selected HTTP protocol.
type Requester struct {
	URL      string
	Client   *http.Client
	Header   http.Header
	protocol string
}

// NewRequester creates a Prometheus remote-write requester.
func NewRequester(url string) *Requester {
	r, _ := NewRequesterForProtocol(url, ProtocolPrometheus)
	return r
}

// NewRequesterForProtocol creates a requester for the selected metrics protocol.
func NewRequesterForProtocol(url, protocol string) (*Requester, error) {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	if protocol != ProtocolPrometheus && protocol != ProtocolOTLP {
		return nil, fmt.Errorf("unsupported metrics protocol %q (supported: %s, %s)", protocol, ProtocolPrometheus, ProtocolOTLP)
	}

	return &Requester{
		URL:      url,
		Client:   &http.Client{},
		Header:   make(http.Header),
		protocol: protocol,
	}, nil
}

// SetHeader sets a header on every request sent by this requester.
func (r *Requester) SetHeader(key, value string) {
	r.Header.Set(key, value)
}

// Send sends a metrics write request using the configured protocol.
func (r *Requester) Send(writeRequest prompb.WriteRequest) error {
	return r.SendContext(context.Background(), writeRequest)
}

// SendContext sends a metrics write request with cancellation.
func (r *Requester) SendContext(ctx context.Context, writeRequest prompb.WriteRequest) error {
	if r.metricsProtocol() == ProtocolOTLP {
		body, err := marshalOTLP(writeRequest.Timeseries)
		if err != nil {
			return err
		}
		_, responseBody, err := r.send(ctx, body, map[string]string{"Content-Type": contentTypeV1})
		if err != nil {
			return err
		}
		return checkOTLPResponse(bytes.NewReader(responseBody))
	}

	body, err := marshalPrometheus(writeRequest)
	if err != nil {
		return err
	}
	_, _, err = r.send(ctx, body, prometheusHeaders())
	return err
}

// SendTimeSeries encodes and sends time series using the configured protocol.
func (r *Requester) SendTimeSeries(timeSeries []prompb.TimeSeries) error {
	return r.SendTimeSeriesContext(context.Background(), timeSeries)
}

// SendTimeSeriesContext encodes and sends time series with cancellation.
func (r *Requester) SendTimeSeriesContext(ctx context.Context, timeSeries []prompb.TimeSeries) error {
	return r.SendContext(ctx, prompb.WriteRequest{Timeseries: timeSeries})
}

// SendV2 sends a Prometheus remote write 2.0 request.
func (r *Requester) SendV2(writeRequest writev2.Request) error {
	return r.SendV2Context(context.Background(), writeRequest)
}

// SendV2Context sends a Prometheus remote write 2.0 request with cancellation.
func (r *Requester) SendV2Context(ctx context.Context, writeRequest writev2.Request) error {
	protobufData, err := writeRequest.OptimizedMarshal(nil)
	if err != nil {
		return err
	}

	headers, _, err := r.send(ctx, snappy.Encode(nil, protobufData), map[string]string{
		"Content-Type":           contentTypeV2,
		"Content-Encoding":       "snappy",
		remoteWriteVersionHeader: remoteWriteVersionV2,
	})
	if err != nil {
		return err
	}

	var expectedSamples uint64
	for i := range writeRequest.Timeseries {
		expectedSamples += uint64(len(writeRequest.Timeseries[i].Samples))
	}
	value := headers.Get(samplesWrittenHeader)
	if value == "" {
		return fmt.Errorf("remote write v2 response missing %s", samplesWrittenHeader)
	}
	writtenSamples, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid %s response header: %w", samplesWrittenHeader, err)
	}
	if writtenSamples != expectedSamples {
		return fmt.Errorf("remote write v2 wrote %d of %d samples", writtenSamples, expectedSamples)
	}
	return nil
}

func (r *Requester) metricsProtocol() string {
	if r.protocol == "" {
		return ProtocolPrometheus
	}
	return r.protocol
}

func prometheusHeaders() map[string]string {
	return map[string]string{
		"Content-Type":           contentTypeV1,
		"Content-Encoding":       "snappy",
		remoteWriteVersionHeader: remoteWriteVersionV1,
	}
}

func marshalPrometheus(writeRequest prompb.WriteRequest) ([]byte, error) {
	protobufData, err := writeRequest.Marshal()
	if err != nil {
		return nil, fmt.Errorf("failed to marshal Prometheus request: %w", err)
	}
	return snappy.Encode(nil, protobufData), nil
}

func marshalOTLP(timeSeries []prompb.TimeSeries) ([]byte, error) {
	metricsByName := make(map[string]*metricspb.Metric)
	metrics := make([]*metricspb.Metric, 0)
	for _, series := range timeSeries {
		name, attributes, err := otlpMetricIdentity(series.Labels)
		if err != nil {
			return nil, err
		}
		metric, ok := metricsByName[name]
		if !ok {
			metric = newOTLPGauge(name)
			metricsByName[name] = metric
			metrics = append(metrics, metric)
		}
		if err := appendOTLPDataPoints(metric.GetGauge(), attributes, series.Samples); err != nil {
			return nil, err
		}
	}
	return marshalOTLPRequest(metrics)
}

func otlpMetricIdentity(labels []prompb.Label) (string, []*commonpb.KeyValue, error) {
	name := ""
	attributes := make([]*commonpb.KeyValue, 0, len(labels))
	for _, label := range labels {
		if label.Name == "__name__" {
			name = label.Value
			continue
		}
		attributes = append(attributes, &commonpb.KeyValue{
			Key:   label.Name,
			Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: label.Value}},
		})
	}
	if name == "" {
		return "", nil, fmt.Errorf("time series is missing the __name__ label")
	}
	return name, attributes, nil
}

func newOTLPGauge(name string) *metricspb.Metric {
	return &metricspb.Metric{
		Name: name,
		Data: &metricspb.Metric_Gauge{Gauge: &metricspb.Gauge{}},
	}
}

func appendOTLPDataPoints(gauge *metricspb.Gauge, attributes []*commonpb.KeyValue, samples []prompb.Sample) error {
	for _, sample := range samples {
		if sample.Timestamp < 0 {
			return fmt.Errorf("OTLP does not support negative Unix timestamps: %d", sample.Timestamp)
		}
		if uint64(sample.Timestamp) > ^uint64(0)/1_000_000 {
			return fmt.Errorf("timestamp overflows OTLP nanoseconds: %d", sample.Timestamp)
		}
		gauge.DataPoints = append(gauge.DataPoints, &metricspb.NumberDataPoint{
			Attributes:   attributes,
			TimeUnixNano: uint64(sample.Timestamp) * 1_000_000,
			Value:        &metricspb.NumberDataPoint_AsDouble{AsDouble: sample.Value},
		})
	}
	return nil
}

func marshalOTLPRequest(metrics []*metricspb.Metric) ([]byte, error) {
	resourceMetrics := &metricspb.ResourceMetrics{
		ScopeMetrics: []*metricspb.ScopeMetrics{{Metrics: metrics}},
	}
	resourceMetricsData, err := proto.Marshal(resourceMetrics)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal OTLP request: %w", err)
	}

	// Encode field 1 directly to keep this HTTP-only client free of gRPC dependencies.
	body := protowire.AppendTag(nil, 1, protowire.BytesType)
	return protowire.AppendBytes(body, resourceMetricsData), nil
}

func (r *Requester) send(ctx context.Context, body []byte, headers map[string]string) (http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create HTTP request: %w", err)
	}
	for key, values := range r.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to read response body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, nil, fmt.Errorf("failed to send HTTP request: %s, body: %s", resp.Status, string(responseBody))
	}
	return resp.Header, responseBody, nil
}

func checkOTLPResponse(reader io.Reader) error {
	body, err := io.ReadAll(reader)
	if err != nil {
		return fmt.Errorf("failed to read OTLP response body: %w", err)
	}
	if len(body) == 0 {
		return nil
	}

	partialSuccess, err := protobufBytesField(body, 1)
	if err != nil || partialSuccess == nil {
		return err
	}
	rejected, err := protobufVarintField(partialSuccess, 1)
	if err != nil {
		return err
	}
	message, err := protobufBytesField(partialSuccess, 2)
	if err != nil {
		return err
	}
	if rejected > 0 {
		return &PartialSuccessError{RejectedDataPoints: rejected, Message: string(message)}
	}
	if len(message) > 0 {
		log.Printf("OTLP response warning: %s", message)
	}
	return nil
}

func protobufBytesField(message []byte, wanted protowire.Number) ([]byte, error) {
	field, err := protobufField(message, wanted, protowire.BytesType)
	if err != nil || field == nil {
		return nil, err
	}
	value, _ := protowire.ConsumeBytes(field)
	return value, nil
}

func protobufVarintField(message []byte, wanted protowire.Number) (uint64, error) {
	field, err := protobufField(message, wanted, protowire.VarintType)
	if err != nil || field == nil {
		return 0, err
	}
	value, _ := protowire.ConsumeVarint(field)
	return value, nil
}

func protobufField(message []byte, wanted protowire.Number, wantedType protowire.Type) ([]byte, error) {
	for len(message) > 0 {
		number, wireType, tagLength := protowire.ConsumeTag(message)
		if tagLength < 0 {
			return nil, fmt.Errorf("invalid protobuf response tag")
		}
		valueLength := protowire.ConsumeFieldValue(number, wireType, message[tagLength:])
		if valueLength < 0 {
			return nil, fmt.Errorf("invalid protobuf response field %d", number)
		}
		if number == wanted && wireType == wantedType {
			return message[tagLength : tagLength+valueLength], nil
		}
		message = message[tagLength+valueLength:]
	}
	return nil, nil
}
