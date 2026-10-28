// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package engineconfig

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

type fakeFetcher struct {
	mu        sync.Mutex
	res       httpResult
	info      map[string]string
	infoState probeState
	calls     atomic.Int32
	delay     time.Duration
	hosts     []string
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

func (f *fakeFetcher) serverInfo(context.Context, string, string, []string) (map[string]string, probeState) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.info, f.infoState
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
		version: "0.11.2", versionState: probeAvailable, model: "Qwen/Qwen2.5-0.5B-Instruct",
		servedModel: "qwen-small", maxModelLen: "2048", modelsState: probeAvailable,
	}
	httpNone = httpResult{versionState: probeUnavailable, modelsState: probeUnavailable}
)

func enginePod(uid, engine string, spec containerSpec) podView {
	ci := containerInfo{Name: engine, Ports: []string{"8000"}, Engine: true}
	if res := parseContainerArgs(spec, engine); res.found {
		ci.Args, ci.Phase, ci.Group = res.fields, res.phase, res.group
	}
	ci.Image = imageVersion(spec.Image)
	if engine == engineVLLM {
		ci.UseV1 = useV1Env(spec.Env, false)
	}
	return podView{uid: types.UID(uid), name: engine + "-0", namespace: "default",
		restarts: map[string]int32{}, info: podInfo{Containers: []containerInfo{ci}}}
}

func vllmServePod(uid string, args ...string) podView {
	return enginePod(uid, engineVLLM, containerSpec{Image: "vllm/vllm-openai:v0.11.2", Args: args})
}

type metricSpec struct {
	name  string
	kind  pmetric.MetricType
	attrs map[string]string
	ts    pcommon.Timestamp
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
	sm := rm.ScopeMetrics().AppendEmpty()
	sm.Scope().SetName("github.com/open-telemetry/opentelemetry-collector-contrib/receiver/prometheusreceiver")
	ms := sm.Metrics()
	for _, s := range metrics {
		m := ms.AppendEmpty()
		m.SetName(s.name)
		var attrs pcommon.Map
		switch s.kind {
		case pmetric.MetricTypeHistogram:
			dp := m.SetEmptyHistogram().DataPoints().AppendEmpty()
			dp.SetTimestamp(s.ts)
			attrs = dp.Attributes()
		case pmetric.MetricTypeSum:
			dp := m.SetEmptySum().DataPoints().AppendEmpty()
			dp.SetTimestamp(s.ts)
			attrs = dp.Attributes()
		default:
			dp := m.SetEmptyGauge().DataPoints().AppendEmpty()
			dp.SetTimestamp(s.ts)
			attrs = dp.Attributes()
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

// findMetric returns the metric of the first resource by name.
func findMetric(md pmetric.Metrics, name string) (pmetric.Metric, bool) {
	var out pmetric.Metric
	found := false
	forEachMetric(md.ResourceMetrics().At(0), func(_ pmetric.ScopeMetrics, m pmetric.Metric) bool {
		if m.Name() == name {
			out, found = m, true
		}
		return !found
	})
	return out, found
}

// dpAttrs returns the data point attributes of the first data point of the metric.
func dpAttrs(t *testing.T, md pmetric.Metrics, name string) map[string]any {
	t.Helper()
	m, ok := findMetric(md, name)
	require.True(t, ok, "metric %s not found", name)
	var out map[string]any
	forEachDataPoint(m, func(a pcommon.Map, _, _ pcommon.Timestamp) {
		if out == nil {
			out = a.AsRaw()
		}
	})
	return out
}

// withPrefix returns the attributes whose key starts with prefix, without the prefix.
func withPrefix(m map[string]any, prefix string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if rest, ok := strings.CutPrefix(k, prefix); ok {
			out[rest] = v
		}
	}
	return out
}

func newTestProcessor(t *testing.T, fetcher httpFetcher, lookup podLookup, opts ...func(*Config)) *engineConfigProcessor {
	return newTestProcessorWithLogger(t, zap.NewNop(), fetcher, lookup, opts...)
}

func newTestProcessorWithLogger(t *testing.T, logger *zap.Logger, fetcher httpFetcher, lookup podLookup, opts ...func(*Config)) *engineConfigProcessor {
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	cfg.Engine = engineVLLM
	for _, o := range opts {
		o(cfg)
	}
	require.NoError(t, cfg.Validate())
	p, err := newEngineConfigProcessor(cfg, logger)
	require.NoError(t, err)
	p.eng, err = newEngine(cfg.Engine, fetcher)
	require.NoError(t, err)
	p.resolver = newResolver(cfg, p.eng, logger)
	p.pods = lookup
	t.Cleanup(func() { _ = p.shutdown(context.Background()) })
	return p
}

func process(t *testing.T, p *engineConfigProcessor, md pmetric.Metrics) pmetric.Metrics {
	t.Helper()
	out, err := p.processMetrics(context.Background(), md)
	require.NoError(t, err)
	return out
}

func TestAnchorRecordAndPrecedence(t *testing.T) {
	pod := vllmServePod("uid-1", "--model", "org/args-model", "-tp", "2", "--max-model-len", "4096",
		"--gpu-memory-utilization", "0.95", "--enforce-eager", "--max-num-seqs", "64")
	fetcher := &fakeFetcher{res: httpAll}
	p := newTestProcessor(t, fetcher, &fakeLookup{pods: map[string]podView{"10.0.0.5": pod}})

	md := scrape("10.0.0.5", "8000",
		histogram("vllm:time_to_first_token_seconds"),
		gauge("vllm:num_requests_running", map[string]string{"vllm.config.max_num_seqs": "customer"}),
		gauge("vllm:cache_config_info", map[string]string{
			"cache_dtype": "auto", "enable_prefix_caching": "True", "gpu_memory_utilization": "0.9",
			"num_gpu_blocks": "682"}),
		counter("http_requests_total"),
	)
	out := process(t, p, md)

	anchor := dpAttrs(t, out, "vllm:num_requests_running")
	assert.Equal(t, "vllm", anchor[keyEngineName])
	assert.Equal(t, "0.11.2", anchor[keyEngineVersion])
	assert.Equal(t, "V1", anchor[keyEngineGeneration])
	// no KV transfer config: the flags say unified; no group
	assert.Equal(t, phaseUnified, anchor[keyEngineRole])
	assert.NotContains(t, anchor, keyEngineGroupRole)
	// the served model name of /v1/models
	assert.Equal(t, "qwen-small", anchor[keyRequestModel])
	// no workload by default: the chart sets it on the resource
	assert.NotContains(t, anchor, keyWorkloadType)
	assert.NotContains(t, anchor, keyWorkloadName)

	assert.Equal(t, map[string]any{
		valModel: "org/args-model", valServedModelName: "default", valTensorParallelSize: "2", valPipelineParallelSize: "default",
		valDataParallelSize: "default", valDtype: "default", valQuantization: "default", valEnforceEager: "true",
		valSpeculativeMethod: "default", valNumSpeculativeTokens: "default",
		// an attribute that is already present is not overwritten
		valMaxNumSeqs:          "customer",
		valMaxNumBatchedTokens: "default", valEnableChunkedPrefill: "default", valEnablePrefixCaching: "default",
		valGPUMemoryUtilization: "0.95", valKVCacheDtype: "default", valMaxModelLen: "4096",
	}, withPrefix(anchor, "vllm.config."))
	// effective values come from the engine; an unknown one is absent, never "default"
	assert.Equal(t, map[string]any{
		valModel: "Qwen/Qwen2.5-0.5B-Instruct", valServedModelName: "qwen-small", valMaxModelLen: "2048",
		valKVCacheDtype: "auto", valEnablePrefixCaching: "true", valGPUMemoryUtilization: "0.9", valNumGPUBlocks: "682",
	}, withPrefix(anchor, "vllm.effective."))

	// no other family and not the resource changes
	for _, name := range []string{"vllm:time_to_first_token_seconds", "vllm:cache_config_info", "http_requests_total"} {
		assert.Empty(t, withPrefix(dpAttrs(t, out, name), "vllm."), name)
		assert.NotContains(t, dpAttrs(t, out, name), keyEngineName, name)
	}
	assert.Equal(t, 5, out.ResourceMetrics().At(0).Resource().Attributes().Len())
	_, hasInfo := findMetric(out, infoMetricName)
	assert.False(t, hasInfo)
	assert.Equal(t, int32(1), fetcher.calls.Load())
}

func TestInfoShape(t *testing.T) {
	pod := vllmServePod("uid-i", "org/m")
	p := newTestProcessor(t, &fakeFetcher{res: httpAll}, &fakeLookup{pods: map[string]podView{"10.0.0.5": pod}},
		func(c *Config) { c.Shapes = []string{shapeAnchor, shapeInfo} })
	running := gauge("vllm:num_requests_running", nil)
	running.ts = 2000
	ttft := histogram("vllm:time_to_first_token_seconds")
	ttft.ts = 3000
	out := process(t, p, scrape("10.0.0.5", "8000", ttft, running))

	m, ok := findMetric(out, infoMetricName)
	require.True(t, ok)
	require.Equal(t, pmetric.MetricTypeGauge, m.Type())
	require.Equal(t, 1, m.Gauge().DataPoints().Len())
	dp := m.Gauge().DataPoints().At(0)
	assert.Equal(t, 1.0, dp.DoubleValue())
	// the timestamp of the anchor data point, not of the newest one
	assert.Equal(t, pcommon.Timestamp(2000), dp.Timestamp())
	// the same record as the anchor, without the anchor's own labels
	want := dpAttrs(t, out, "vllm:num_requests_running")
	delete(want, "model_name")
	assert.Equal(t, want, dp.Attributes().AsRaw())
	assert.Equal(t, 1, out.ResourceMetrics().At(0).ScopeMetrics().Len())

	// without the anchor family: the newest data point gives the timestamp
	p2 := newTestProcessor(t, &fakeFetcher{res: httpAll}, &fakeLookup{pods: map[string]podView{"10.0.0.5": pod}},
		func(c *Config) { c.Shapes = []string{shapeInfo} })
	out = process(t, p2, scrape("10.0.0.5", "8000", ttft, counter("vllm:prompt_tokens_total")))
	m, ok = findMetric(out, infoMetricName)
	require.True(t, ok)
	assert.Equal(t, pcommon.Timestamp(3000), m.Gauge().DataPoints().At(0).Timestamp())
	assert.Equal(t, "0.11.2", m.Gauge().DataPoints().At(0).Attributes().AsRaw()[keyEngineVersion])

	// a second pass does not add a second Info metric
	out = process(t, p2, out)
	count := 0
	forEachMetric(out.ResourceMetrics().At(0), func(_ pmetric.ScopeMetrics, m pmetric.Metric) bool {
		if m.Name() == infoMetricName {
			count++
		}
		return true
	})
	assert.Equal(t, 1, count)
}

func TestPerFamilyShapeAndResourceIdentity(t *testing.T) {
	pod := vllmServePod("uid-f", "org/m", "-tp", "2")
	p := newTestProcessor(t, &fakeFetcher{res: httpAll}, &fakeLookup{pods: map[string]podView{"10.0.0.5": pod}},
		func(c *Config) { c.Shapes, c.ResourceIdentity = []string{shapePerFamily}, true })
	out := process(t, p, scrape("10.0.0.5", "8000",
		histogram("vllm:time_to_first_token_seconds"), gauge("vllm:num_requests_running", nil),
		counter("vllm:prompt_tokens_cached_total"), histogram("vllm:request_params_n")))

	res := out.ResourceMetrics().At(0).Resource().Attributes().AsRaw()
	assert.Equal(t, "vllm", res[keyEngineName])
	assert.Equal(t, "0.11.2", res[keyEngineVersion])
	assert.Equal(t, "V1", res[keyEngineGeneration])
	assert.Equal(t, phaseUnified, res[keyEngineRole])
	assert.Equal(t, "qwen-small", res[keyRequestModel])

	assert.Equal(t, map[string]any{
		valTensorParallelSize: "2", valPipelineParallelSize: "default", valDtype: "default",
		valQuantization: "default", valEnablePrefixCaching: "default", valEnableChunkedPrefill: "default",
		valMaxNumBatchedTokens: "default",
	}, withPrefix(dpAttrs(t, out, "vllm:time_to_first_token_seconds"), "vllm.config."))
	assert.Equal(t, map[string]any{valMaxModelLen: "2048"},
		withPrefix(dpAttrs(t, out, "vllm:num_requests_running"), "vllm.effective."))
	assert.Equal(t, map[string]any{valEnablePrefixCaching: "default"},
		withPrefix(dpAttrs(t, out, "vllm:prompt_tokens_cached_total"), "vllm.config."))
	assert.Empty(t, withPrefix(dpAttrs(t, out, "vllm:request_params_n"), "vllm."))
	// per_family does not write the record keys on data points
	assert.NotContains(t, dpAttrs(t, out, "vllm:num_requests_running"), keyEngineName)
}

func TestWorkloadServingAndRole(t *testing.T) {
	pod := vllmServePod("uid-w", "org/m", "--served-model-name", "llama", "llama-alias", "--kv-transfer-config",
		`{"kv_connector":"NixlConnector","kv_role":"kv_consumer"}`, "--nnodes", "2", "--node-rank", "0")
	pod.owners = []owner{{Kind: "ReplicaSet", Name: "llama-decode-7d9f8b6c5d"}}
	pod.labels = map[string]string{podTemplateHashLabel: "7d9f8b6c5d", "serving.kserve.io/inferenceservice": "llama",
		"llm-d.ai/role": "prefill"}
	lookup := &fakeLookup{pods: map[string]podView{"10.0.0.5": pod}}

	out := process(t, newTestProcessor(t, &fakeFetcher{res: httpNone}, lookup),
		scrape("10.0.0.5", "8000", gauge("vllm:num_requests_running", nil)))
	anchor := dpAttrs(t, out, "vllm:num_requests_running")
	// the flag beats the label
	assert.Equal(t, phaseDecode, anchor[keyEngineRole])
	assert.Equal(t, groupLeader, anchor[keyEngineGroupRole])
	assert.Equal(t, "serving.kserve.io/InferenceService", anchor[keyServingKind])
	assert.Equal(t, "llama", anchor[keyServingName])
	assert.NotContains(t, anchor, keyWorkloadName)
	// without /v1/models the configured served model name (the first one) gives gen_ai.request.model
	assert.Equal(t, "llama", anchor[keyRequestModel])
	assert.Equal(t, "llama", anchor["vllm.config.served_model_name"])

	out = process(t, newTestProcessor(t, &fakeFetcher{res: httpNone}, lookup, func(c *Config) { c.EmitWorkload = true }),
		scrape("10.0.0.5", "8000", gauge("vllm:num_requests_running", nil)))
	anchor = dpAttrs(t, out, "vllm:num_requests_running")
	assert.Equal(t, "Deployment", anchor[keyWorkloadType])
	assert.Equal(t, "llama-decode", anchor[keyWorkloadName])
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
	require.False(t, parseContainerArgs(spec, engineVLLM).found)
	ci.Env = readEnvContract(envMap, spec.Env, false)
	ci.UseV1 = useV1Env(spec.Env, false)
	pod := podView{uid: "uid-2", info: podInfo{Containers: []containerInfo{ci}}}

	p := newTestProcessor(t, &fakeFetcher{res: httpNone}, &fakeLookup{pods: map[string]podView{"10.0.0.6": pod}},
		func(c *Config) { c.EnvMap = envMap })
	out := process(t, p, scrape("10.0.0.6", "8082",
		gauge("vllm:num_requests_running", nil),
		gauge("vllm:num_requests_swapped", nil),
		gauge("vllm:cache_config_info", map[string]string{
			"cache_dtype": "auto", "enable_prefix_caching": "False", "gpu_memory_utilization": "0.85"}),
	))
	anchor := dpAttrs(t, out, "vllm:num_requests_running")
	assert.NotContains(t, anchor, keyEngineVersion)
	assert.Equal(t, "V0", anchor[keyEngineGeneration])
	// no command line: nothing decides the phase
	assert.Equal(t, phaseUnified, anchor[keyEngineRole])
	assert.Equal(t, "/models/tinyllama", anchor[keyRequestModel])
	// the env contract gives configured values; a mapped env name that the pod
	// does not set is "default"; unmapped values with no command line are absent
	assert.Equal(t, map[string]any{
		valModel: "/models/tinyllama", valMaxModelLen: "2048", valGPUMemoryUtilization: "0.8",
		valTensorParallelSize: "default",
	}, withPrefix(anchor, "vllm.config."))
	assert.Equal(t, map[string]any{
		valKVCacheDtype: "auto", valEnablePrefixCaching: "false", valGPUMemoryUtilization: "0.85",
	}, withPrefix(anchor, "vllm.effective."))
}

func TestDefaultVersusAbsent(t *testing.T) {
	// an unknown shell expansion may set any flag after it: such values are left out
	spec := containerSpec{Command: []string{"sh", "-c", "vllm serve org/m --dtype half $EXTRA_ARGS"}}
	ci := containerInfo{Name: "vllm", Ports: []string{"8000"}, Args: parseContainerArgs(spec, engineVLLM).fields}
	pod := podView{uid: "uid-4", info: podInfo{Containers: []containerInfo{ci}}}
	p := newTestProcessor(t, &fakeFetcher{res: httpNone}, &fakeLookup{pods: map[string]podView{"10.0.0.8": pod}})
	out := process(t, p, scrape("10.0.0.8", "8000", gauge("vllm:num_requests_running", nil)))
	assert.Equal(t, map[string]any{valModel: "org/m"}, withPrefix(dpAttrs(t, out, "vllm:num_requests_running"), "vllm.config."))

	// without an unknown expansion, a flag that is not set is "default"
	pod2 := vllmServePod("uid-5", "org/m")
	p2 := newTestProcessor(t, &fakeFetcher{res: httpNone}, &fakeLookup{pods: map[string]podView{"10.0.0.9": pod2}})
	out = process(t, p2, scrape("10.0.0.9", "8000", gauge("vllm:num_requests_running", nil)))
	assert.Equal(t, "default", dpAttrs(t, out, "vllm:num_requests_running")["vllm.config.dtype"])
	assert.Empty(t, withPrefix(dpAttrs(t, out, "vllm:num_requests_running"), "vllm.effective."))
}

func TestRestartInvalidatesAndSingleFlight(t *testing.T) {
	pod := vllmServePod("uid-6", "m")
	fetcher := &fakeFetcher{res: httpAll, delay: 50 * time.Millisecond}
	lookup := &fakeLookup{pods: map[string]podView{"10.0.0.10": pod}}
	p := newTestProcessor(t, fetcher, lookup)
	version := func(md pmetric.Metrics) any { return dpAttrs(t, md, "vllm:num_requests_running")[keyEngineVersion] }

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// every concurrent first batch already carries the values
			assert.Equal(t, "0.11.2", version(process(t, p, scrape("10.0.0.10", "8000", gauge("vllm:num_requests_running", nil)))))
		}()
	}
	wg.Wait()
	assert.Equal(t, int32(1), fetcher.calls.Load())

	process(t, p, scrape("10.0.0.10", "8000", gauge("vllm:num_requests_running", nil)))
	assert.Equal(t, int32(1), fetcher.calls.Load(), "cached")

	// a restart of another container does not count
	pod.restarts = map[string]int32{"sidecar": 3}
	lookup.mu.Lock()
	lookup.pods["10.0.0.10"] = pod
	lookup.mu.Unlock()
	process(t, p, scrape("10.0.0.10", "8000", gauge("vllm:num_requests_running", nil)))
	assert.Equal(t, int32(1), fetcher.calls.Load())

	pod.restarts = map[string]int32{"vllm": 1}
	lookup.mu.Lock()
	lookup.pods["10.0.0.10"] = pod
	lookup.mu.Unlock()
	fetcher.set(httpResult{version: "0.12.0", versionState: probeAvailable, modelsState: probeUnavailable})
	out := process(t, p, scrape("10.0.0.10", "8000", gauge("vllm:num_requests_running", nil)))
	assert.Equal(t, int32(2), fetcher.calls.Load(), "a restart reads again")
	assert.Equal(t, "0.12.0", version(out))
}

func TestRefreshKeepsLastGoodAndRetries(t *testing.T) {
	pod := vllmServePod("uid-7", "m")
	fetcher := &fakeFetcher{res: httpAll}
	p := newTestProcessor(t, fetcher, &fakeLookup{pods: map[string]podView{"10.0.0.11": pod}},
		func(c *Config) { c.RetryInterval = 2 * time.Minute })
	now := time.Unix(1_800_000_000, 0)
	var clockMu sync.Mutex
	p.resolver.now = func() time.Time { clockMu.Lock(); defer clockMu.Unlock(); return now }
	advance := func(d time.Duration) { clockMu.Lock(); now = now.Add(d); clockMu.Unlock() }
	run := func() map[string]any {
		out := process(t, p, scrape("10.0.0.11", "8000", gauge("vllm:num_requests_running", nil)))
		p.resolver.wg.Wait() // let a background refresh finish
		return dpAttrs(t, out, "vllm:num_requests_running")
	}
	run()
	assert.Equal(t, int32(1), fetcher.calls.Load())

	// the refresh fails: the last good values stay
	fetcher.set(httpResult{versionState: probeFailed, modelsState: probeFailed})
	advance(10 * time.Minute)
	run()
	assert.Equal(t, int32(2), fetcher.calls.Load())
	assert.Equal(t, "0.11.2", run()[keyEngineVersion])

	// retried at most once per retry_interval
	advance(time.Minute)
	run()
	assert.Equal(t, int32(2), fetcher.calls.Load())
	advance(61 * time.Second)
	run()
	assert.Equal(t, int32(3), fetcher.calls.Load())
}

func TestMissingAnchorLoggedOncePerPod(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	pod := vllmServePod("uid-8", "m")
	p := newTestProcessorWithLogger(t, zap.New(core), &fakeFetcher{res: httpAll},
		&fakeLookup{pods: map[string]podView{"10.0.0.12": pod}})
	for i := 0; i < 3; i++ {
		out := process(t, p, scrape("10.0.0.12", "8000", histogram("vllm:time_to_first_token_seconds")))
		assert.NotContains(t, dpAttrs(t, out, "vllm:time_to_first_token_seconds"), keyEngineName)
	}
	assert.Equal(t, 1, logs.FilterMessageSnippet("anchor family is not in the scrape").Len())
}

func TestSGLangRecord(t *testing.T) {
	spec := containerSpec{Command: []string{"python3", "-m", "sglang.launch_server"}, Image: "lmsysorg/sglang:v0.5.21",
		Args: []string{"--model-path", "Qwen/Qwen3-8B", "--tp", "2", "--disaggregation-mode", "decode",
			"--mem-fraction-static", "0.85", "--context-length", "32k", "--disable-radix-cache"}}
	pod := enginePod("uid-s", engineSGLang, spec)
	pod.labels = map[string]string{"ray.io/cluster": "sgl-ray"}
	// the effective mode of /server_info beats the flag
	fetcher := &fakeFetcher{infoState: probeAvailable, info: map[string]string{
		sgVersion: "0.5.22", sgModelPath: "Qwen/Qwen3-8B", "tp_size": "2", "mem_fraction_static": "0.85",
		"quantization": noneValue, "disaggregation_mode": "prefill"}}
	p := newTestProcessor(t, fetcher, &fakeLookup{pods: map[string]podView{"10.0.0.20": pod}},
		func(c *Config) { c.Engine, c.Shapes = engineSGLang, []string{shapeAnchor, shapeInfo} })

	out := process(t, p, scrape("10.0.0.20", "30000", gauge("sglang:num_running_reqs", nil),
		gauge("sglang:token_usage", nil), gauge("vllm:num_requests_running", nil)))
	anchor := dpAttrs(t, out, "sglang:num_running_reqs")
	assert.Equal(t, "sglang", anchor[keyEngineName])
	assert.Equal(t, "0.5.22", anchor[keyEngineVersion])
	assert.NotContains(t, anchor, keyEngineGeneration)
	assert.Equal(t, phasePrefill, anchor[keyEngineRole])
	// no group role from Ray labels for SGLang
	assert.NotContains(t, anchor, keyEngineGroupRole)
	assert.Equal(t, "ray.io/RayCluster", anchor[keyServingKind])
	assert.Equal(t, "Qwen/Qwen3-8B", anchor[keyRequestModel])
	config := withPrefix(anchor, "sglang.config.")
	assert.Equal(t, "2", config["tp_size"])
	assert.Equal(t, "32000", config["context_length"])
	assert.Equal(t, "true", config["disable_radix_cache"])
	assert.Equal(t, "default", config["dtype"])
	assert.Len(t, config, len(sglangValues)-1)
	assert.Equal(t, map[string]any{sgModelPath: "Qwen/Qwen3-8B", "tp_size": "2", "mem_fraction_static": "0.85",
		"quantization": "none", "disaggregation_mode": "prefill"}, withPrefix(anchor, "sglang.effective."))
	_, ok := findMetric(out, infoMetricName)
	assert.True(t, ok)
	// a vLLM family in the same scrape is not the SGLang anchor
	assert.NotContains(t, dpAttrs(t, out, "vllm:num_requests_running"), keyEngineName)

	// a scrape without sglang:* families is not SGLang's
	md := scrape("10.0.0.20", "8000", gauge("vllm:num_requests_running", nil))
	_, ok = findMetric(process(t, p, md), infoMetricName)
	assert.False(t, ok)
}

// TestSGLangVersionAndEngineType: without /server_info, the image tag gives the
// version, and the engine_type label of the anchor (from v0.5.7) gives the phase
// when the flags cannot tell.
func TestSGLangVersionAndEngineType(t *testing.T) {
	spec := containerSpec{Image: "lmsysorg/sglang:v0.5.22-cu130",
		Command: []string{"sh", "-c", "python3 -m sglang.launch_server $EXTRA_ARGS --model-path m"}}
	pod := enginePod("uid-t", engineSGLang, spec)
	pod.labels = map[string]string{"llm-d.ai/role": "decode"}
	p := newTestProcessor(t, &fakeFetcher{infoState: probeUnavailable}, &fakeLookup{pods: map[string]podView{"10.0.0.21": pod}},
		func(c *Config) { c.Engine = engineSGLang })
	out := process(t, p, scrape("10.0.0.21", "30000",
		gauge("sglang:num_running_reqs", map[string]string{"engine_type": "prefill", "tp_rank": "0"})))
	anchor := dpAttrs(t, out, "sglang:num_running_reqs")
	assert.Equal(t, "0.5.22", anchor[keyEngineVersion])
	assert.Equal(t, phasePrefill, anchor[keyEngineRole])
	assert.Equal(t, "m", anchor[keyRequestModel])
	assert.NotContains(t, anchor, "sglang.effective.engine_type")
	assert.NotContains(t, anchor, "sglang.effective.version")

	// before v0.5.7 the label was always unified: the platform label decides
	spec.Image = "lmsysorg/sglang:v0.5.6"
	pod = enginePod("uid-u", engineSGLang, spec)
	pod.labels = map[string]string{"llm-d.ai/role": "decode"}
	p = newTestProcessor(t, &fakeFetcher{infoState: probeUnavailable}, &fakeLookup{pods: map[string]podView{"10.0.0.22": pod}},
		func(c *Config) { c.Engine = engineSGLang })
	out = process(t, p, scrape("10.0.0.22", "30000",
		gauge("sglang:num_running_reqs", map[string]string{"engine_type": "unified"})))
	assert.Equal(t, phaseDecode, dpAttrs(t, out, "sglang:num_running_reqs")[keyEngineRole])
}

func TestStartupLineFallback(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "default_vllm-0_uid-l", "vllm")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	line := "2026-10-15T12:43:00.1Z stdout F INFO 05-02 09:10:11 [llm_engine.py:241] Initializing a V0 LLM engine " +
		"(v0.8.5) with config: model='Qwen/Qwen2.5-7B-Instruct', dtype=torch.float16, max_seq_len=32768, " +
		"tensor_parallel_size=1, quantization=awq, enforce_eager=True, kv_cache_dtype=auto, enable_prefix_caching=False, \n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "0.log"), []byte(line), 0o600))

	pod := vllmServePod("uid-l", "Qwen/Qwen2.5-7B-Instruct")
	p := newTestProcessor(t, &fakeFetcher{res: httpResult{version: "0.8.5", versionState: probeAvailable}},
		&fakeLookup{pods: map[string]podView{"10.0.0.13": pod}},
		func(c *Config) { c.LogFallback, c.LogRoot = true, root })
	out := process(t, p, scrape("10.0.0.13", "8000", gauge("vllm:num_requests_running", nil)))
	anchor := dpAttrs(t, out, "vllm:num_requests_running")
	// the version rule says V1 for 0.8.5; the startup line says V0
	assert.Equal(t, "V0", anchor[keyEngineGeneration])
	assert.Equal(t, "0.8.5", anchor[keyEngineVersion])
	assert.Equal(t, map[string]any{
		valModel: "Qwen/Qwen2.5-7B-Instruct", valDtype: "float16", valMaxModelLen: "32768",
		valTensorParallelSize: "1", valQuantization: "awq", valEnforceEager: "true", valKVCacheDtype: "auto",
		valEnablePrefixCaching: "false",
	}, withPrefix(anchor, "vllm.effective."))
}

func TestPassThroughWithoutKubernetes(t *testing.T) {
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	cfg.Engine, cfg.NodeName = engineVLLM, "node-a"
	p, err := newEngineConfigProcessor(cfg, zap.NewNop())
	require.NoError(t, err)
	p.newClient = func() (kubernetes.Interface, error) {
		return nil, errors.New("unable to load in-cluster configuration")
	}
	require.NoError(t, p.start(context.Background(), componenttest.NewNopHost()))
	defer func() { require.NoError(t, p.shutdown(context.Background())) }()

	md := scrape("10.0.0.5", "8000", gauge("vllm:num_requests_running", nil))
	want := pmetric.NewMetrics()
	md.CopyTo(want)
	assert.Equal(t, want, process(t, p, md))

	// the default client outside a cluster fails the same way
	_, err = inClusterClient()
	assert.Error(t, err)
}

func TestPassThroughWithoutNodeName(t *testing.T) {
	t.Setenv(nodeNameEnv, "")
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	cfg.Engine = engineSGLang
	p, err := newEngineConfigProcessor(cfg, zap.NewNop())
	require.NoError(t, err)
	called := false
	p.newClient = func() (kubernetes.Interface, error) { called = true; return nil, errors.New("x") }
	require.NoError(t, p.start(context.Background(), componenttest.NewNopHost()))
	defer func() { require.NoError(t, p.shutdown(context.Background())) }()
	assert.False(t, called)
	md := scrape("10.0.0.5", "8000", gauge("sglang:num_running_reqs", nil))
	assert.NotContains(t, dpAttrs(t, process(t, p, md), "sglang:num_running_reqs"), keyEngineName)
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

func TestDeriveGeneration(t *testing.T) {
	assert.Equal(t, "V1", deriveGeneration("0.11.0", "0"))
	assert.Equal(t, "V1", deriveGeneration("0.8.5", ""))
	assert.Equal(t, "V0", deriveGeneration("0.8.5", "0"))
	assert.Equal(t, "V0", deriveGeneration("0.6.6.post1", ""))
	assert.Equal(t, "V1", deriveGeneration("0.7.3", "1"))
	assert.Equal(t, "", deriveGeneration("", ""))
	assert.Equal(t, "", deriveGeneration("0.9.0", "?"))
}
