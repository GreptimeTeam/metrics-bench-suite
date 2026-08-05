package http

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
	writev2 "github.com/prometheus/prometheus/prompb/io/prometheus/write/v2"
)

const (
	contentTypeV1            = "application/x-protobuf"
	contentTypeV2            = contentTypeV1 + ";proto=io.prometheus.write.v2.Request"
	remoteWriteVersionHeader = "X-Prometheus-Remote-Write-Version"
	remoteWriteVersionV1     = "0.1.0"
	remoteWriteVersionV2     = "2.0.0"
	samplesWrittenHeader     = "X-Prometheus-Remote-Write-Samples-Written"
	userAgent                = "metrics-bench-suite"
)

// Requester is the struct for the requester
type Requester struct {
	URL    string
	Client *http.Client
	Header http.Header
}

// SendStats describes a completed remote-write request.
type SendStats struct {
	PayloadBytes int
}

// PreparedWriteRequest is a marshaled and compressed v1 remote-write payload.
// It lets callers apply admission control without serializing a request twice.
type PreparedWriteRequest struct {
	compressedData []byte
}

// PayloadBytes returns the encoded on-the-wire payload size.
func (p PreparedWriteRequest) PayloadBytes() int {
	return len(p.compressedData)
}

// NewRequester creates a new requester
func NewRequester(url string) *Requester {
	return &Requester{
		URL:    url,
		Client: &http.Client{},
		Header: make(http.Header),
	}
}

// SetHeader sets a header on every request sent by this requester.
func (r *Requester) SetHeader(key, value string) {
	r.Header.Set(key, value)
}

// Send sends a write request to the remote write endpoint
func (r *Requester) Send(writeRequest prompb.WriteRequest) error {
	return r.SendContext(context.Background(), writeRequest)
}

// SendContext sends a write request with cancellation.
func (r *Requester) SendContext(ctx context.Context, writeRequest prompb.WriteRequest) error {
	protobufData, err := writeRequest.Marshal()
	if err != nil {
		return err
	}

	_, err = r.send(ctx, protobufData, contentTypeV1, remoteWriteVersionV1)
	return err
}

// SendWithStats sends a v1 remote-write request and returns the compressed payload size.
func (r *Requester) SendWithStats(writeRequest prompb.WriteRequest) (SendStats, error) {
	prepared, err := PrepareWriteRequest(writeRequest)
	if err != nil {
		return SendStats{}, err
	}
	stats := SendStats{PayloadBytes: prepared.PayloadBytes()}
	err = r.SendPrepared(context.Background(), prepared)
	return stats, err
}

// PrepareWriteRequest marshals and compresses a v1 remote-write request once.
func PrepareWriteRequest(writeRequest prompb.WriteRequest) (PreparedWriteRequest, error) {
	protobufData, err := writeRequest.Marshal()
	if err != nil {
		return PreparedWriteRequest{}, err
	}
	return PreparedWriteRequest{compressedData: snappy.Encode(nil, protobufData)}, nil
}

// SendPrepared sends a payload returned by PrepareWriteRequest with cancellation.
func (r *Requester) SendPrepared(ctx context.Context, prepared PreparedWriteRequest) error {
	_, err := r.sendCompressed(ctx, prepared.compressedData, contentTypeV1, remoteWriteVersionV1)
	return err
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

	headers, err := r.send(ctx, protobufData, contentTypeV2, remoteWriteVersionV2)
	if err != nil {
		return err
	}

	expectedSamples := uint64(0)
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

func (r *Requester) send(ctx context.Context, protobufData []byte, contentType, remoteWriteVersion string) (http.Header, error) {
	return r.sendCompressed(ctx, snappy.Encode(nil, protobufData), contentType, remoteWriteVersion)
}

func (r *Requester) sendCompressed(ctx context.Context, compressedData []byte, contentType, remoteWriteVersion string) (http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL, bytes.NewBuffer(compressedData))
	if err != nil {
		return nil, fmt.Errorf("failed to create HTTP request: %v", err)
	}

	for key, values := range r.Header {
		for _, value := range values {
			req.Header.Add(key, value)
		}
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set(remoteWriteVersionHeader, remoteWriteVersion)

	resp, err := r.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if !(resp.StatusCode >= 200 && resp.StatusCode < 300) {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("failed to read response body: %v", err)
		}
		return nil, fmt.Errorf("failed to send HTTP request: %v, body: %v", resp.Status, string(body))
	}

	return resp.Header, nil
}
