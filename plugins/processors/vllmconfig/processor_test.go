// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package vllmconfig

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component/componenttest"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

type fakeFetcher struct {
	mu    sync.Mutex
	res   httpResult
	calls atomic.Int32
	delay time.Duration
	hosts []string
}

func (f *fakeFetcher) fetch(ctx context.Context, _, host string, _ []string) httpResult {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hosts = append(f.hosts, host)
	return f.res
}

func (f *fakeFetcher) set(res httpResult) {
	f.mu.Lock()
	f.res = res
	f.mu.Unlock()
}

type fakeLookup struct {
	mu   sync.Mutex
	pods map[string]podView // by host
}

func (l *fakeLookup) lookup(t scrapeTarget) (podView, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.pods[t.host]
	return p, ok
}

var (
	httpAll = httpResult{
		version: "0.11.2", versionState: probeAvailable,
		model: "Qwen/Qwen2.5-0.5B-Instruct", maxModelLen: "2048", modelsState: probeAvailable,
	}
	httpNone = httpResult{versionState: probeUnavailable, modelsState: probeUnavailable}
)

func vllmServePod(uid string, args ...string) podView {
	spec := containerSpec{Image: "vllm/vllm-openai:v0.11.2", Args: args}
	ci := containerInfo{Name: "vllm", Ports: []string{"8000"}, VLLM: true}
	if res := parseContainerArgs(spec); res.found {
		ci.Args = res.fields
	}
	ci.UseV1 = useV1Env(nil, false)
	return podView{uid: types.UID(uid), name: "vllm-0", namespace: "default", info: podInfo{Containers: []containerInfo{ci}}}
}

type metricSpec struct {
	name  string
	kind  pmetric.MetricType
	attrs map[string]string
}

func scrape(host, port string, metrics ...metricSpec) pmetric.Metrics {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	res := rm.Resource().Attributes()
	res.PutStr("service.name", "vllm")
	res.PutStr(attrServerAddress, host)
	res.PutStr(attrServerPort, port)
	res.PutStr(attrURLScheme, "http")
	res.PutStr(attrServiceInstanceID, host+":"+port)
	ms := rm.ScopeMetrics().AppendEmpty().Metrics()
	for _, s := range metrics {
		m := ms.AppendEmpty()
		m.SetName(s.name)
		var attrs pcommon.Map
		switch s.kind {
		case pmetric.MetricTypeHistogram:
			attrs = m.SetEmptyHistogram().DataPoints().AppendEmpty().Attributes()
		case pmetric.MetricTypeSum:
			attrs = m.SetEmptySum().DataPoints().AppendEmpty().Attributes()
		default:
			attrs = m.SetEmptyGauge().DataPoints().AppendEmpty().Attributes()
		}
		attrs.PutStr("model_name", "served")
		for k, v := range s.attrs {
			attrs.PutStr(k, v)
		}
	}
	return md
}

func gauge(name string, attrs map[string]string) metricSpec {
	return metricSpec{name: name, kind: pmetric.MetricTypeGauge, attrs: attrs}
}

func histogram(name string) metricSpec {
	return metricSpec{name: name, kind: pmetric.MetricTypeHistogram}
}

func counter(name string) metricSpec { return metricSpec{name: name, kind: pmetric.MetricTypeSum} }

// dpAttrs returns the data point attributes of the first data point of the metric.
func dpAttrs(t *testing.T, md pmetric.Metrics, name string) map[string]any {
	t.Helper()
	ms := md.ResourceMetrics().At(0).ScopeMetrics().At(0).Metrics()
	for i := 0; i < ms.Len(); i++ {
		m := ms.At(i)
		if m.Name() != name {
			continue
		}
		var out map[string]any
		forEachDataPoint(m, func(a pcommon.Map) {
			if out == nil {
				out = a.AsRaw()
			}
		})
		return out
	}
	t.Fatalf("metric %s not found", name)
	return nil
}

func configAttrs(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if len(k) > len(attrPrefix) && k[:len(attrPrefix)] == attrPrefix {
			out[k[len(attrPrefix):]] = v
		}
	}
	return out
}

func newTestProcessor(t *testing.T, fetcher httpFetcher, lookup podLookup) *vllmConfigProcessor {
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	p := newVLLMConfigProcessor(cfg, zap.NewNop())
	p.fetcher = fetcher
	p.resolver = newResolver(cfg, fetcher, zap.NewNop())
	p.pods = lookup
	t.Cleanup(func() { _ = p.shutdown(context.Background()) })
	return p
}

func process(t *testing.T, p *vllmConfigProcessor, md pmetric.Metrics) pmetric.Metrics {
	t.Helper()
	out, err := p.processMetrics(context.Background(), md)
	require.NoError(t, err)
	return out
}

func TestAttachFamiliesAndPrecedence(t *testing.T) {
	pod := vllmServePod("uid-1", "--model", "org/args-model", "-tp", "2", "--max-model-len", "4096",
		"--gpu-memory-utilization", "0.95", "--enforce-eager", "--max-num-seqs", "64")
	fetcher := &fakeFetcher{res: httpAll}
	p := newTestProcessor(t, fetcher, &fakeLookup{pods: map[string]podView{"10.0.0.5": pod}})

	md := scrape("10.0.0.5", "8000",
		histogram("vllm:time_to_first_token_seconds"),
		gauge("vllm:num_requests_running", nil),
		gauge("vllm:kv_cache_usage_perc", nil),
		counter("vllm:prompt_tokens_cached_total"),
		gauge("vllm:cache_config_info", map[string]string{
			"cache_dtype": "auto", "enable_prefix_caching": "True", "gpu_memory_utilization": "0.9"}),
		counter("http_requests_total"),
		histogram("vllm:request_params_n"),
		gauge("vllm:num_requests_waiting", map[string]string{"vllm.config.max_num_seqs": "customer"}),
	)
	out := process(t, p, md)

	res := out.ResourceMetrics().At(0).Resource().Attributes().AsRaw()
	assert.Equal(t, "0.11.2", res["vllm.config.version"])
	// the HTTP API wins over the args for the model
	assert.Equal(t, "Qwen/Qwen2.5-0.5B-Instruct", res["vllm.config.model"])
	assert.Equal(t, "V1", res["vllm.config.engine"])

	assert.Equal(t, map[string]any{
		valTensorParallelSize: "2", valPipelineParallelSize: "default", valDtype: "default",
		valQuantization: "default", valEnablePrefixCaching: "true", valEnableChunkedPrefill: "default",
		valMaxNumBatchedTokens: "default",
	}, configAttrs(dpAttrs(t, out, "vllm:time_to_first_token_seconds")))

	// cache_config_info of the same scrape wins over the args for gpu_memory_utilization;
	// /v1/models wins over the args for max_model_len
	assert.Equal(t, map[string]any{
		valMaxNumSeqs: "64", valMaxNumBatchedTokens: "default", valGPUMemoryUtilization: "0.9",
		valKVCacheDtype: "auto", valMaxModelLen: "2048", valTensorParallelSize: "2", valPipelineParallelSize: "default",
	}, configAttrs(dpAttrs(t, out, "vllm:num_requests_running")))
	assert.Equal(t, map[string]any{valEnablePrefixCaching: "true"},
		configAttrs(dpAttrs(t, out, "vllm:prompt_tokens_cached_total")))
	assert.Len(t, configAttrs(dpAttrs(t, out, "vllm:kv_cache_usage_perc")), 6)

	// no data point attributes outside the mapped families
	assert.Empty(t, configAttrs(dpAttrs(t, out, "vllm:cache_config_info")))
	assert.Empty(t, configAttrs(dpAttrs(t, out, "http_requests_total")))
	assert.Empty(t, configAttrs(dpAttrs(t, out, "vllm:request_params_n")))
	// an attribute that is already present is not overwritten
	assert.Equal(t, "customer", dpAttrs(t, out, "vllm:num_requests_waiting")["vllm.config.max_num_seqs"])
	assert.Equal(t, int32(1), fetcher.calls.Load())
}

// The target cluster: vLLM 0.6.6.post1 AsyncLLMEngine in a custom server that
// reads its own env vars. No /version, a non-OpenAI /v1/models, V0 families.
func TestEnvContractAndV0(t *testing.T) {
	envMap := map[string]string{
		"MODEL_PATH": valModel, "MAX_MODEL_LEN": valMaxModelLen,
		"GPU_MEMORY_UTILIZATION": valGPUMemoryUtilization, "TENSOR_PARALLEL_SIZE": valTensorParallelSize,
	}
	spec := containerSpec{
		Image:   "123456789012.dkr.ecr.us-west-2.amazonaws.com/gpu-test-app:v3",
		Command: []string{"python", "/app/serve.py"},
		Env: []envVar{{Name: "MODEL_PATH", Value: "/models/tinyllama"}, {Name: "MAX_MODEL_LEN", Value: "2048"},
			{Name: "GPU_MEMORY_UTILIZATION", Value: "0.80"}, {Name: "HF_TOKEN", FromSource: true}},
	}
	ci := containerInfo{Name: "kserve-container", Ports: []string{"8080", "8082"}}
	require.False(t, parseContainerArgs(spec).found)
	ci.Env = readEnvContract(envMap, spec.Env, false)
	ci.UseV1 = useV1Env(spec.Env, false)
	pod := podView{uid: "uid-2", info: podInfo{Containers: []containerInfo{ci}}}

	fetcher := &fakeFetcher{res: httpNone}
	p := newTestProcessor(t, fetcher, &fakeLookup{pods: map[string]podView{"10.0.0.6": pod}})
	md := scrape("10.0.0.6", "8082",
		histogram("vllm:time_to_first_token_seconds"),
		gauge("vllm:num_requests_swapped", nil),
		gauge("vllm:gpu_cache_usage_perc", nil),
		gauge("vllm:cache_config_info", map[string]string{
			"cache_dtype": "auto", "enable_prefix_caching": "False", "gpu_memory_utilization": "0.85"}),
	)
	out := process(t, p, md)
	res := out.ResourceMetrics().At(0).Resource().Attributes().AsRaw()
	assert.Nil(t, res["vllm.config.version"])
	assert.Equal(t, "/models/tinyllama", res["vllm.config.model"])
	assert.Equal(t, "V0", res["vllm.config.engine"])
	// effective cache_config_info beats the env value; the env gives the others;
	// a mapped env name that the pod does not set is "default"; unmapped values with
	// no command line are left out
	assert.Equal(t, map[string]any{
		valKVCacheDtype: "auto", valGPUMemoryUtilization: "0.85", valMaxModelLen: "2048",
		valTensorParallelSize: "default", valEnablePrefixCaching: "false",
	}, configAttrs(dpAttrs(t, out, "vllm:gpu_cache_usage_perc")))
	assert.Equal(t, map[string]any{valTensorParallelSize: "default", valEnablePrefixCaching: "false"},
		configAttrs(dpAttrs(t, out, "vllm:time_to_first_token_seconds")))
}

func TestCacheConfigRememberedPerPod(t *testing.T) {
	pod := vllmServePod("uid-3", "m")
	p := newTestProcessor(t, &fakeFetcher{res: httpNone}, &fakeLookup{pods: map[string]podView{"10.0.0.7": pod}})
	process(t, p, scrape("10.0.0.7", "8000", gauge("vllm:cache_config_info", map[string]string{"cache_dtype": "fp8"})))
	out := process(t, p, scrape("10.0.0.7", "8000", gauge("vllm:kv_cache_usage_perc", nil)))
	assert.Equal(t, "fp8", dpAttrs(t, out, "vllm:kv_cache_usage_perc")["vllm.config.kv_cache_dtype"])
}

func TestDefaultVersusAbsent(t *testing.T) {
	// an unknown shell expansion may set any flag after it: such values are left out
	spec := containerSpec{Command: []string{"sh", "-c", "vllm serve org/m --dtype half $EXTRA_ARGS"}}
	ci := containerInfo{Name: "vllm", Ports: []string{"8000"}, Args: parseContainerArgs(spec).fields}
	pod := podView{uid: "uid-4", info: podInfo{Containers: []containerInfo{ci}}}
	p := newTestProcessor(t, &fakeFetcher{res: httpNone}, &fakeLookup{pods: map[string]podView{"10.0.0.8": pod}})
	out := process(t, p, scrape("10.0.0.8", "8000", histogram("vllm:time_to_first_token_seconds")))
	assert.Empty(t, configAttrs(dpAttrs(t, out, "vllm:time_to_first_token_seconds")))
	assert.Equal(t, "org/m", out.ResourceMetrics().At(0).Resource().Attributes().AsRaw()["vllm.config.model"])

	// without an unknown expansion, a flag that is not set is "default"
	pod2 := vllmServePod("uid-5", "org/m")
	p2 := newTestProcessor(t, &fakeFetcher{res: httpNone}, &fakeLookup{pods: map[string]podView{"10.0.0.9": pod2}})
	out = process(t, p2, scrape("10.0.0.9", "8000", histogram("vllm:time_to_first_token_seconds")))
	assert.Equal(t, "default", dpAttrs(t, out, "vllm:time_to_first_token_seconds")["vllm.config.dtype"])
}

func TestRestartInvalidatesAndSingleFlight(t *testing.T) {
	pod := vllmServePod("uid-6", "m")
	fetcher := &fakeFetcher{res: httpAll, delay: 50 * time.Millisecond}
	lookup := &fakeLookup{pods: map[string]podView{"10.0.0.10": pod}}
	p := newTestProcessor(t, fetcher, lookup)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out := process(t, p, scrape("10.0.0.10", "8000", histogram("vllm:e2e_request_latency_seconds")))
			// every concurrent first batch already carries the values
			assert.Equal(t, "0.11.2", out.ResourceMetrics().At(0).Resource().Attributes().AsRaw()["vllm.config.version"])
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), fetcher.calls.Load())

	process(t, p, scrape("10.0.0.10", "8000", histogram("vllm:e2e_request_latency_seconds")))
	assert.Equal(t, int32(1), fetcher.calls.Load(), "cached")

	pod.restarts = 1
	lookup.mu.Lock()
	lookup.pods["10.0.0.10"] = pod
	lookup.mu.Unlock()
	fetcher.set(httpResult{version: "0.12.0", versionState: probeAvailable, modelsState: probeUnavailable})
	out := process(t, p, scrape("10.0.0.10", "8000", histogram("vllm:e2e_request_latency_seconds")))
	assert.Equal(t, int32(2), fetcher.calls.Load(), "a restart reads again")
	assert.Equal(t, "0.12.0", out.ResourceMetrics().At(0).Resource().Attributes().AsRaw()["vllm.config.version"])
}

func TestRefreshKeepsLastGoodAndRetries(t *testing.T) {
	pod := vllmServePod("uid-7", "m")
	fetcher := &fakeFetcher{res: httpAll}
	p := newTestProcessor(t, fetcher, &fakeLookup{pods: map[string]podView{"10.0.0.11": pod}})
	now := time.Unix(1_800_000_000, 0)
	var clockMu sync.Mutex
	p.resolver.now = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	advance := func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }
	run := func() pcommon.Map {
		out := process(t, p, scrape("10.0.0.11", "8000", gauge("vllm:num_requests_running", nil)))
		p.resolver.wg.Wait() // let a background refresh finish
		return out.ResourceMetrics().At(0).Resource().Attributes()
	}
	run()
	assert.Equal(t, int32(1), fetcher.calls.Load())

	// the refresh fails: the last good values stay
	fetcher.set(httpResult{versionState: probeFailed, modelsState: probeFailed})
	advance(10 * time.Minute)
	run()
	assert.Equal(t, int32(2), fetcher.calls.Load())
	v, _ := run().Get("vllm.config.version")
	assert.Equal(t, "0.11.2", v.AsString())

	// retried at most once per minute
	advance(30 * time.Second)
	run()
	assert.Equal(t, int32(2), fetcher.calls.Load())
	advance(31 * time.Second)
	run()
	assert.Equal(t, int32(3), fetcher.calls.Load())
}

func TestPassThroughWithoutKubernetes(t *testing.T) {
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	cfg.NodeName = "node-a"
	p := newVLLMConfigProcessor(cfg, zap.NewNop())
	p.newClient = func() (kubernetes.Interface, error) {
		return nil, errors.New("unable to load in-cluster configuration")
	}
	require.NoError(t, p.start(context.Background(), componenttest.NewNopHost()))
	defer func() { require.NoError(t, p.shutdown(context.Background())) }()

	md := scrape("10.0.0.5", "8000", histogram("vllm:time_to_first_token_seconds"))
	want := pmetric.NewMetrics()
	md.CopyTo(want)
	out := process(t, p, md)
	assert.Equal(t, want, out)

	// the default client outside a cluster fails the same way
	_, err := inClusterClient()
	assert.Error(t, err)
}

func TestPassThroughWithoutNodeName(t *testing.T) {
	t.Setenv(nodeNameEnv, "")
	p := newVLLMConfigProcessor(NewFactory().CreateDefaultConfig().(*Config), zap.NewNop())
	called := false
	p.newClient = func() (kubernetes.Interface, error) { called = true; return nil, errors.New("x") }
	require.NoError(t, p.start(context.Background(), componenttest.NewNopHost()))
	defer func() { require.NoError(t, p.shutdown(context.Background())) }()
	assert.False(t, called)
	md := scrape("10.0.0.5", "8000", histogram("vllm:time_to_first_token_seconds"))
	assert.Equal(t, 0, len(configAttrs(dpAttrs(t, process(t, p, md), "vllm:time_to_first_token_seconds"))))
}

func TestTargetFromResource(t *testing.T) {
	attrs := pcommon.NewMap()
	attrs.PutStr(attrServiceInstanceID, "127.0.0.1:8000")
	attrs.PutStr(attrServerPort, "8000")
	tg := targetFromResource(attrs)
	assert.Equal(t, scrapeTarget{scheme: "http", host: "127.0.0.1", port: "8000"}, tg)

	attrs.PutStr(attrServerAddress, "10.1.2.3")
	attrs.PutStr(attrURLScheme, "https")
	attrs.PutStr(attrPodUID, "u")
	attrs.PutStr(attrContainerName, "c")
	tg = targetFromResource(attrs)
	assert.Equal(t, "10.1.2.3", tg.host)
	assert.Equal(t, "https", tg.scheme)
	assert.Equal(t, "u", tg.podUID)
	assert.Equal(t, "c", tg.container)
}

func TestDeriveEngine(t *testing.T) {
	assert.Equal(t, "V1", deriveEngine("0.11.0", "0", false))
	assert.Equal(t, "V1", deriveEngine("0.8.5", "", false))
	assert.Equal(t, "V0", deriveEngine("0.8.5", "0", false))
	assert.Equal(t, "V0", deriveEngine("0.6.6.post1", "", false))
	assert.Equal(t, "V1", deriveEngine("0.7.3", "1", false))
	assert.Equal(t, "V0", deriveEngine("", "", true))
	assert.Equal(t, "", deriveEngine("", "", false))
	assert.Equal(t, "", deriveEngine("0.9.0", "?", false))
}
