package dataset_test

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"golang.org/x/exp/rand"
	"gopkg.in/yaml.v3"
	"metrics-bench-suite/pkg/dataset"
	"metrics-bench-suite/pkg/samples"
)

// Every bundled metric must have an explicit semantic category. In particular,
// recording rules and running pod/container counts are not raw counters.
var curatedMetricGroups = map[string][]string{
	"boolean": {
		"kube_namespace_status_phase",
		"kube_node_status_condition",
		"kube_persistentvolumeclaim_status_phase",
		"kube_pod_container_status_last_terminated_reason",
	},
	"bytes": {
		"cluster:namespace:pod_memory:active:kube_pod_container_resource_limits",
		"cluster:namespace:pod_memory:active:kube_pod_container_resource_requests",
		"container_fs_limit_bytes",
		"container_fs_usage_bytes",
		"container_memory_cache",
		"container_memory_rss",
		"container_memory_swap",
		"container_memory_working_set_bytes",
		"kubelet_volume_stats_available_bytes",
		"kubelet_volume_stats_capacity_bytes",
		"kubelet_volume_stats_used_bytes",
		"namespace_memory:kube_pod_container_resource_limits:sum",
		"namespace_memory:kube_pod_container_resource_requests:sum",
		"node_memory_MemAvailable_bytes:sum",
		"node_memory_MemTotal_bytes",
		"node_namespace_pod_container:container_memory_cache",
		"node_namespace_pod_container:container_memory_rss",
		"node_namespace_pod_container:container_memory_swap",
		"node_namespace_pod_container:container_memory_working_set_bytes",
		"process_resident_memory_bytes",
	},
	"counter": {
		"container_cpu_cfs_periods_total",
		"container_cpu_cfs_throttled_periods_total",
		"container_cpu_usage_seconds_total",
		"container_fs_reads_bytes_total",
		"container_fs_reads_total",
		"container_fs_writes_bytes_total",
		"container_fs_writes_total",
		"container_network_receive_bytes_total",
		"container_network_receive_packets_dropped_total",
		"container_network_receive_packets_total",
		"container_network_transmit_bytes_total",
		"container_network_transmit_packets_dropped_total",
		"container_network_transmit_packets_total",
		"coredns_cache_hits_total",
		"coredns_cache_misses_total",
		"coredns_dns_do_requests_total",
		"coredns_dns_request_count_total",
		"coredns_dns_request_do_count_total",
		"coredns_dns_request_duration_seconds_bucket",
		"coredns_dns_request_size_bytes_bucket",
		"coredns_dns_request_type_count_total",
		"coredns_dns_requests_total",
		"coredns_dns_response_rcode_count_total",
		"coredns_dns_response_size_bytes_bucket",
		"coredns_dns_responses_total",
		"kubelet_cgroup_manager_duration_seconds_bucket",
		"kubelet_cgroup_manager_duration_seconds_count",
		"kubelet_pleg_relist_duration_seconds_bucket",
		"kubelet_pleg_relist_duration_seconds_count",
		"kubelet_pleg_relist_interval_seconds_bucket",
		"kubelet_pod_start_duration_seconds_bucket",
		"kubelet_pod_start_duration_seconds_count",
		"kubelet_pod_worker_duration_seconds_bucket",
		"kubelet_pod_worker_duration_seconds_count",
		"kubelet_runtime_operations_duration_seconds_bucket",
		"kubelet_runtime_operations_errors_total",
		"kubelet_runtime_operations_total",
		"kubeproxy_network_programming_duration_seconds_bucket",
		"kubeproxy_network_programming_duration_seconds_count",
		"kubeproxy_sync_proxy_rules_duration_seconds_bucket",
		"kubeproxy_sync_proxy_rules_duration_seconds_count",
		"node_netstat_TcpExt_TCPSynRetrans",
		"node_netstat_Tcp_OutSegs",
		"node_netstat_Tcp_RetransSegs",
		"process_cpu_seconds_total",
		"rest_client_request_duration_seconds_bucket",
		"rest_client_requests_total",
		"storage_operation_duration_seconds_bucket",
		"storage_operation_duration_seconds_count",
		"storage_operation_errors_total",
		"workqueue_adds_total",
		"workqueue_queue_duration_seconds_bucket",
	},
	"cpu_rate": {
		"node_namespace_pod_container:container_cpu_usage_seconds_total:sum_irate",
		"node_namespace_pod_container:container_cpu_usage_seconds_total:sum_rate5m",
	},
	"gauge": {
		"cluster:namespace:pod_cpu:active:kube_pod_container_resource_limits",
		"cluster:namespace:pod_cpu:active:kube_pod_container_resource_requests",
		"kube_node_status_allocatable",
		"kube_node_status_capacity",
		"kube_pod_container_resource_limits",
		"kube_pod_container_resource_requests",
		"kube_resourcequota",
		"namespace_cpu:kube_pod_container_resource_limits:sum",
		"namespace_cpu:kube_pod_container_resource_requests:sum",
	},
	"info": {
		"container",
		"kube_node_info",
		"kube_persistentvolumeclaim_info",
		"kube_pod_info",
		"kube_pod_owner",
		"kubelet_node_name",
		"namespace_workload_pod:kube_pod_owner:relabel",
		"up",
	},
	"integer": {
		"coredns_cache_entries",
		"coredns_cache_size",
		"go_goroutines",
		"kubelet_running_container_count",
		"kubelet_running_containers",
		"kubelet_running_pod_count",
		"kubelet_running_pods",
		"volume_manager_total_volumes",
		"workqueue_depth",
	},
	"latency": {
		"cluster_quantile:apiserver_request_slo_duration_seconds:histogram_quantile",
	},
	"rate": {
		"code_resource:apiserver_request_total:rate5m",
	},
	"ratio": {
		"apiserver_request:availability30d",
		"cluster:node_cpu:ratio_rate5m",
	},
	"zero": {
		"kubelet_node_config_error",
	},
}

func curatedCategories(t *testing.T) map[string]string {
	t.Helper()
	categories := map[string]string{}
	for category, names := range curatedMetricGroups {
		for _, name := range names {
			if _, exists := categories[name]; exists {
				t.Fatalf("duplicate classification for %s", name)
			}
			categories[name] = category
		}
	}
	if len(categories) != 109 {
		t.Fatalf("classified %d metrics, expected 109", len(categories))
	}
	return categories
}

func readCuratedConfig(t *testing.T, path string) samples.Config {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config samples.Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	return config
}

func checkCuratedValues(t *testing.T, name, category string, dist samples.Distribution) {
	t.Helper()
	expectedType := "uniform"
	switch category {
	case "counter":
		expectedType = "mono_inc"
	case "info", "zero":
		expectedType = "constant_float"
	case "boolean", "integer":
		expectedType = "random_int"
	}
	if dist.Type != expectedType {
		t.Fatalf("%s requires %s, got %s", category, expectedType, dist.Type)
	}
	upper := map[string]float64{"boolean": 2, "integer": 1001, "ratio": 1, "cpu_rate": 16, "rate": 1000, "latency": 5, "bytes": 1073741824, "gauge": 1000}[category]
	if category == "counter" && dist.UpperBound != nil {
		t.Fatal("counter must not wrap")
	}
	lower := 0.0
	if name == "namespace_cpu:kube_pod_container_resource_requests:sum" {
		lower = 1
	}
	if upper > 0 && (dist.LowerBound == nil || *dist.LowerBound != lower || dist.UpperBound == nil || *dist.UpperBound != upper) {
		t.Fatalf("%s must use [0, %g)", category, upper)
	}
	// A private fixed seed keeps behavior checks deterministic. Sample beyond the
	// former 100-sample wrap point and require gauges to move in both directions.
	generator := dist.FieldGeneratorWithRandom(rand.New(rand.NewSource(42)))
	previous, rose, fell := 0.0, false, false
	for i := 0; i < 256; i++ {
		value := generator.Next()
		if math.IsNaN(value) || math.IsInf(value, 0) {
			t.Fatalf("nonfinite value: %v", value)
		}
		switch category {
		case "counter":
			step := 10
			if name == "container_cpu_cfs_throttled_periods_total" {
				step = 1
			}
			if value != float64(i*step) {
				t.Fatalf("counter sample %d: %g", i, value)
			}
		case "info":
			if value != 1 {
				t.Fatalf("info/healthy target value: %g", value)
			}
		case "zero":
			if value != 0 {
				t.Fatalf("config error value: %g", value)
			}
		default:
			if value < lower || value >= upper {
				t.Fatalf("%s outside [0, %g): %g", category, upper, value)
			}
			if (category == "boolean" || category == "integer") && value != math.Trunc(value) {
				t.Fatalf("nonintegral value: %g", value)
			}
		}
		if i > 0 {
			rose = rose || value > previous
			fell = fell || value < previous
		}
		previous = value
	}
	if upper > 0 && (!rose || !fell) {
		t.Fatal("gauge did not move in both directions")
	}
}

func TestCuratedProfiles(t *testing.T) {
	categories := curatedCategories(t)
	reference := map[string]samples.Distribution{}
	for _, profile := range []struct {
		name, source string
		series       int64
	}{
		{"k8s-small", "debug_samples_20", 20205},
		{"k8s-medium", "debug_samples_400", 406610},
		{"k8s-large", "samples_1750", 1747610},
	} {
		t.Run(profile.name, func(t *testing.T) {
			root := filepath.Join("..", "..", "profiles", profile.name)
			inspection, err := dataset.Inspect(root)
			if err != nil {
				t.Fatal(err)
			}
			if !inspection.Valid || inspection.BaseSeries != profile.series || len(inspection.Metrics) != len(categories) {
				t.Fatalf("invalid profile inventory: %+v", inspection)
			}
			// Exercise the live loader's parser as well as strict offline inspection.
			live, err := samples.WalkAndParseConfigWithMaxFileCount(root, math.MaxUint64)
			if err != nil {
				t.Fatal(err)
			}
			if len(live) != len(categories) {
				t.Fatalf("live parser read %d configs", len(live))
			}
			for _, metric := range live {
				t.Run(metric.Name, func(t *testing.T) {
					category, ok := categories[metric.Name]
					if !ok {
						t.Fatalf("unclassified metric %s", metric.Name)
					}
					dist := metric.Config.Fields[0].Dist
					checkCuratedValues(t, metric.Name, category, dist)
					if first, ok := reference[metric.Name]; ok {
						if !reflect.DeepEqual(first, dist) {
							t.Fatal("value definition differs between profile sizes")
						}
					} else {
						reference[metric.Name] = dist
					}
					// Dashboard inputs intentionally correct a few legacy label domains.
					if metric.Name == "node_namespace_pod_container:container_cpu_usage_seconds_total:sum_rate5m" || metric.Name == "namespace_workload_pod:kube_pod_owner:relabel" || metric.Name == "kube_pod_info" || metric.Name == "container_cpu_cfs_throttled_periods_total" {
						return
					}
					original := readCuratedConfig(t, filepath.Join("..", "..", "configs", profile.source, metric.Name+".yaml"))
					current := readCuratedConfig(t, filepath.Join(root, metric.Name+".yaml"))
					// Only the value distribution may differ from the historical source.
					current.Fields[0].Dist = original.Fields[0].Dist
					if !reflect.DeepEqual(original, current) {
						t.Fatal("labels, timing, or field identity changed")
					}
				})
			}
		})
	}
}

func TestCuratedConstantsOffline(t *testing.T) {
	configRoot := t.TempDir()
	expected := map[string]float64{}
	for name, category := range curatedCategories(t) {
		if category != "info" && category != "zero" {
			continue
		}
		config := readCuratedConfig(t, filepath.Join("..", "..", "profiles", "k8s-small", name+".yaml"))
		expected[name] = config.Fields[0].Dist.Value.(float64)
		// Keep actual profile fields while reducing the fixture to one series each.
		config.Tags = nil
		data, err := yaml.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(configRoot, name+".yaml"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	output := t.TempDir()
	_, err := dataset.Generate(context.Background(), configRoot, output, dataset.Options{Start: start, End: start.Add(3 * time.Second), IntervalMillis: 1000, MaxSamples: 10, Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dataset.Verify(context.Background(), output); err != nil {
		t.Fatal(err)
	}
	decoded := decode(t, output, 10)
	if len(decoded) != len(expected) {
		t.Fatalf("got %d constant series", len(decoded))
	}
	for labels, points := range decoded {
		var pairs []struct {
			Name  string
			Value string
		}
		if err := json.Unmarshal([]byte(labels), &pairs); err != nil {
			t.Fatal(err)
		}
		name := ""
		for _, pair := range pairs {
			if pair.Name == "__name__" {
				name = pair.Value
			}
		}
		want, ok := expected[name]
		if !ok || len(points) != 3 {
			t.Fatalf("unexpected constant series: %s", labels)
		}
		for _, point := range points {
			if point.Value != want {
				t.Fatalf("%s: got %g, want %g", name, point.Value, want)
			}
		}
	}
}

// The dashboard workload requires correlated label domains, unlike the legacy
// Cartesian profiles. Validate every scale, including scales not loaded locally.
func TestDashboardProfileRelationships(t *testing.T) {
	for _, profile := range []string{"k8s-small", "k8s-medium", "k8s-large"} {
		t.Run(profile, func(t *testing.T) {
			read := func(name string) samples.Config {
				return readCuratedConfig(t, filepath.Join("..", "..", "profiles", profile, name+".yaml"))
			}
			cpu := read("node_namespace_pod_container:container_cpu_usage_seconds_total:sum_rate5m")
			oldCPU := read("node_namespace_pod_container:container_cpu_usage_seconds_total:sum_irate")
			if !reflect.DeepEqual(cpu, oldCPU) {
				t.Fatal("CPU recording dimensions changed")
			}
			owner := read("namespace_workload_pod:kube_pod_owner:relabel")
			for _, tag := range owner.Tags {
				if tag.Name != "namespace" && tag.Name != "pod" && tag.Dist.Len() != 1 {
					t.Fatalf("owner dimension %s would duplicate a join key", tag.Name)
				}
				if tag.Name == "workload" && tag.Dist.Value != "workload-0" {
					t.Fatal("unexpected workload")
				}
				if tag.Name == "workload_type" && tag.Dist.Value != "deployment" {
					t.Fatal("unexpected workload type")
				}
			}
			found := false
			for _, tag := range read("kube_pod_info").Tags {
				if tag.Name == "host_network" {
					found = tag.Dist.Type == "constant_string" && tag.Dist.Value == "false"
				}
			}
			if !found {
				t.Fatal("host_network=false selector would be empty")
			}
			throttled, periods := read("container_cpu_cfs_throttled_periods_total"), read("container_cpu_cfs_periods_total")
			if !reflect.DeepEqual(throttled.Tags, periods.Tags) {
				t.Fatal("throttling label domains differ")
			}
			if *throttled.Fields[0].Dist.Step != 1 || *periods.Fields[0].Dist.Step != 10 {
				t.Fatal("throttling ratio must be 0.1")
			}
			requests := read("namespace_cpu:kube_pod_container_resource_requests:sum")
			if *requests.Fields[0].Dist.LowerBound <= 0 {
				t.Fatal("CPU requests must stay positive")
			}
			for _, metric := range []string{"container_memory_rss", "container_network_receive_bytes_total", "container_cpu_cfs_periods_total"} {
				values := map[string]interface{}{}
				for _, tag := range read(metric).Tags {
					values[tag.Name] = tag.Dist.Value
				}
				if values["job"] != "kubelet" || values["metrics_path"] != "/metrics/cadvisor" {
					t.Fatalf("%s does not match cAdvisor selector", metric)
				}
			}
		})
	}
}
