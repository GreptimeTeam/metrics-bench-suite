package loader

import (
	stdhttp "net/http"
	"net/http/httptest"
	"testing"

	benchhttp "metrics-bench-suite/pkg/http"

	"github.com/prometheus/prometheus/prompb"
	"github.com/stretchr/testify/assert"
)

func TestScaleMetrics(t *testing.T) {
	tsSet := []prompb.TimeSeries{
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "metric"},
				{Name: "host", Value: "host"},
			},
			Samples: []prompb.Sample{
				{Value: 1, Timestamp: 1},
			},
		},
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "total"},
				{Name: "host", Value: "host"},
			},
			Samples: []prompb.Sample{
				{Value: 1, Timestamp: 1},
			},
		},
	}

	expected := []prompb.TimeSeries{
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "metric"},
				{Name: "host", Value: "host"},
			},
			Samples: []prompb.Sample{
				{Value: 1, Timestamp: 1},
			},
		},
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "total"},
				{Name: "host", Value: "host"},
			},
			Samples: []prompb.Sample{
				{Value: 1, Timestamp: 1},
			},
		},
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "metric_1"},
				{Name: "host", Value: "host"},
			},
			Samples: []prompb.Sample{
				{Value: 1, Timestamp: 1},
			},
		},
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "total_1"},
				{Name: "host", Value: "host"},
			},
			Samples: []prompb.Sample{
				{Value: 1, Timestamp: 1},
			},
		},
	}
	scaled := ScaleMetrics(tsSet, 2)
	assert.Equal(t, len(scaled), 2*len(tsSet))
	assert.Equal(t, expected, scaled)

}

func TestScaleLabels(t *testing.T) {
	tsSet := []prompb.TimeSeries{
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "metric"},
				{Name: "host", Value: "host"},
			},
			Samples: []prompb.Sample{
				{Value: 1, Timestamp: 1},
			},
		},
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "metric"},
				{Name: "job", Value: "job"},
			},
			Samples: []prompb.Sample{
				{Value: 1, Timestamp: 1},
			},
		},
	}

	expected := []prompb.TimeSeries{
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "metric"},
				{Name: "host", Value: "host"},
			},
			Samples: []prompb.Sample{
				{Value: 1, Timestamp: 1},
			},
		},
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "metric"},
				{Name: "job", Value: "job"},
			},
			Samples: []prompb.Sample{
				{Value: 1, Timestamp: 1},
			},
		},
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "metric"},
				{Name: "hosta", Value: "host"},
			},
			Samples: []prompb.Sample{
				{Value: 1, Timestamp: 1},
			},
		},
		{
			Labels: []prompb.Label{
				{Name: "__name__", Value: "metric"},
				{Name: "joba", Value: "job"},
			},
			Samples: []prompb.Sample{
				{Value: 1, Timestamp: 1},
			},
		},
	}

	scaled := ScaleLabels(tsSet, 2)
	assert.Equal(t, expected, scaled)
}

func TestProcessReturnsRequestErrors(t *testing.T) {
	server := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, _ *stdhttp.Request) {
		stdhttp.Error(w, "rejected", stdhttp.StatusBadRequest)
	}))
	defer server.Close()

	loader := Loader{
		URL:                  server.URL,
		Protocol:             benchhttp.ProtocolPrometheus,
		TimeseriesPerRequest: 1,
		SampleScale:          1,
	}
	err := loader.process([]prompb.TimeSeries{{
		Labels:  []prompb.Label{{Name: "__name__", Value: "metric"}},
		Samples: []prompb.Sample{{Value: 1, Timestamp: 1}},
	}})
	if err == nil {
		t.Fatal("expected request error")
	}
}
