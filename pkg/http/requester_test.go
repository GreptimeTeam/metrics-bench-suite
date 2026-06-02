package http

import (
	"io"
	nethttp "net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/prometheus/prompb"
)

func TestRequesterSendIncludesConfiguredHeader(t *testing.T) {
	var gotAuthorization string
	server := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, r *nethttp.Request) {
		gotAuthorization = r.Header.Get("Authorization")
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
	if gotAuthorization != expected {
		t.Fatalf("expected authorization header %q, got %q", expected, gotAuthorization)
	}
}
