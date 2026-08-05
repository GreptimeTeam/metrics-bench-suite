package http

import (
	"io"
	nethttp "net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
	writev2 "github.com/prometheus/prometheus/prompb/io/prometheus/write/v2"
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

	err := requester.Send(prompb.WriteRequest{})
	if err != nil {
		t.Fatalf("expected send to succeed, got %v", err)
	}

	expected := "Basic YWxpY2U6c2VjcmV0"
	if got := gotHeaders.Get("Authorization"); got != expected {
		t.Fatalf("expected authorization header %q, got %q", expected, got)
	}
	if got := gotHeaders.Get("Content-Type"); got != contentTypeV1 {
		t.Fatalf("expected content type %q, got %q", contentTypeV1, got)
	}
	if got := gotHeaders.Get(remoteWriteVersionHeader); got != remoteWriteVersionV1 {
		t.Fatalf("expected remote write version %q, got %q", remoteWriteVersionV1, got)
	}
	if got := gotHeaders.Get("User-Agent"); got != userAgent {
		t.Fatalf("expected user agent %q, got %q", userAgent, got)
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
		} else {
			body, decodeErr = snappy.Decode(nil, body)
			if decodeErr == nil {
				decodeErr = gotRequest.Unmarshal(body)
			}
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
		t.Fatalf("expected send to succeed, got %v", err)
	}
	if decodeErr != nil {
		t.Fatalf("failed to decode request: %v", decodeErr)
	}
	if !reflect.DeepEqual(gotRequest.Symbols, request.Symbols) || !reflect.DeepEqual(gotRequest.Timeseries, request.Timeseries) {
		t.Fatalf("expected request %#v, got %#v", request, gotRequest)
	}
	if got := gotHeaders.Get("Content-Type"); got != contentTypeV2 {
		t.Fatalf("expected content type %q, got %q", contentTypeV2, got)
	}
	if got := gotHeaders.Get(remoteWriteVersionHeader); got != remoteWriteVersionV2 {
		t.Fatalf("expected remote write version %q, got %q", remoteWriteVersionV2, got)
	}
	if got := gotHeaders.Get("Content-Encoding"); got != "snappy" {
		t.Fatalf("expected snappy content encoding, got %q", got)
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
		Symbols: []string{""},
		Timeseries: []writev2.TimeSeries{{
			Samples: []writev2.Sample{{Value: 1, Timestamp: 2}},
		}},
	}
	err := NewRequester(server.URL).SendV2(request)
	if err == nil || !strings.Contains(err.Error(), "wrote 0 of 1 samples") {
		t.Fatalf("expected written sample mismatch, got %v", err)
	}
}
