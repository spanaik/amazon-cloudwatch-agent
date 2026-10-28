// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package engineconfig

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

const (
	openAIModels = `{"object":"list","data":[{"id":"Qwen/Qwen2.5-0.5B-Instruct","object":"model","created":1792776187,` +
		`"owned_by":"vllm","root":"Qwen/Qwen2.5-0.5B-Instruct","parent":null,"max_model_len":2048,"permission":[]}]}`
	promText = "# HELP python_gc_objects_collected_total Objects collected during gc\n" +
		"# TYPE python_gc_objects_collected_total counter\npython_gc_objects_collected_total{generation=\"0\"} 4.78e+08\n"
)

type recordedRequest struct {
	method, path string
	header       http.Header
}

// fakeServer serves fixed responses per path and records what it receives.
type fakeServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []recordedRequest
}

func newFakeServer(t *testing.T, routes map[string]func(http.ResponseWriter)) *fakeServer {
	fs := &fakeServer{}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fs.mu.Lock()
		fs.requests = append(fs.requests, recordedRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone()})
		fs.mu.Unlock()
		if h, ok := routes[r.URL.Path]; ok {
			h(w)
			return
		}
		if h, ok := routes["*"]; ok {
			h(w)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(fs.Close)
	return fs
}

func (fs *fakeServer) reqs() []recordedRequest {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return append([]recordedRequest(nil), fs.requests...)
}

func (fs *fakeServer) hostPort(t *testing.T) (string, string) {
	u, err := url.Parse(fs.URL)
	require.NoError(t, err)
	h, p, err := net.SplitHostPort(u.Host)
	require.NoError(t, err)
	return h, p
}

func body(status int, b string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(b))
	}
}

func fetchWith(t *testing.T, servers ...*fakeServer) httpResult {
	var host string
	var ports []string
	for _, s := range servers {
		h, p := s.hostPort(t)
		host = h
		ports = append(ports, p)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return newAPIFetcher().fetch(ctx, "http", host, ports)
}

func TestFetchOpenAIShape(t *testing.T) {
	s := newFakeServer(t, map[string]func(http.ResponseWriter){
		"/version":   body(200, `{"version":"0.11.2"}`),
		"/v1/models": body(200, openAIModels),
	})
	res := fetchWith(t, s)
	assert.Equal(t, probeAvailable, res.versionState)
	assert.Equal(t, "0.11.2", res.version)
	assert.Equal(t, probeAvailable, res.modelsState)
	assert.Equal(t, "Qwen/Qwen2.5-0.5B-Instruct", res.model)
	assert.Equal(t, "2048", res.maxModelLen)
	for _, r := range s.reqs() {
		assert.Equal(t, http.MethodGet, r.method)
		assert.Empty(t, r.header.Get("Authorization"))
		assert.Empty(t, r.header.Get("Cookie"))
	}
}

func TestFetchAPIKey401(t *testing.T) {
	s := newFakeServer(t, map[string]func(http.ResponseWriter){
		"/version":   body(200, `{"version":"0.29.0"}`),
		"/v1/models": body(401, `{"error":"Unauthorized"}`),
	})
	res := fetchWith(t, s)
	assert.Equal(t, "0.29.0", res.version)
	assert.Equal(t, probeUnavailable, res.modelsState)
	assert.Empty(t, res.model)
}

func TestFetch404(t *testing.T) {
	s := newFakeServer(t, nil)
	res := fetchWith(t, s)
	assert.Equal(t, probeUnavailable, res.versionState)
	assert.Equal(t, probeUnavailable, res.modelsState)
}

// The target cluster: a prometheus_client server on the scrape port answers every
// path with Prometheus text; the KServe port answers {"models":[...]}.
func TestFetchPrometheusTextAndCustomModels(t *testing.T) {
	metricsPort := newFakeServer(t, map[string]func(http.ResponseWriter){"*": body(200, promText)})
	kserve := newFakeServer(t, map[string]func(http.ResponseWriter){
		"/v1/models": body(200, `{"models":["tinyllama"]}`),
	})
	res := fetchWith(t, metricsPort, kserve)
	assert.Equal(t, probeUnavailable, res.versionState)
	assert.Equal(t, probeUnavailable, res.modelsState)
	// the scrape port is tried first, then the other port
	assert.Len(t, metricsPort.reqs(), 2)
	assert.Len(t, kserve.reqs(), 2)
}

func TestFetchSecondPort(t *testing.T) {
	metricsPort := newFakeServer(t, map[string]func(http.ResponseWriter){"*": body(200, promText)})
	api := newFakeServer(t, map[string]func(http.ResponseWriter){
		"/version":   body(200, `{"version":"0.6.6.post1"}`),
		"/v1/models": body(200, openAIModels),
	})
	res := fetchWith(t, metricsPort, api)
	assert.Equal(t, "0.6.6.post1", res.version)
	assert.Equal(t, "Qwen/Qwen2.5-0.5B-Instruct", res.model)
}

func TestFetchDoesNotFollowRedirects(t *testing.T) {
	target := newFakeServer(t, map[string]func(http.ResponseWriter){"/version": body(200, `{"version":"0.11.0"}`)})
	redirect := newFakeServer(t, map[string]func(http.ResponseWriter){
		"/version": func(w http.ResponseWriter) {
			w.Header().Set("Location", target.URL+"/version")
			w.WriteHeader(http.StatusFound)
		},
	})
	res := fetchWith(t, redirect)
	assert.Equal(t, probeUnavailable, res.versionState)
	assert.Empty(t, target.reqs())
}

func TestFetchBodyLimit(t *testing.T) {
	big := `{"version":"0.11.0","pad":"` + strings.Repeat("x", maxBodyBytes) + `"}`
	s := newFakeServer(t, map[string]func(http.ResponseWriter){"/version": body(200, big)})
	res := fetchWith(t, s)
	assert.Equal(t, probeUnavailable, res.versionState)
}

func TestFetchUnreachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	_, port, _ := net.SplitHostPort(l.Addr().String())
	require.NoError(t, l.Close())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res := newAPIFetcher().fetch(ctx, "http", "127.0.0.1", []string{port})
	assert.Equal(t, probeFailed, res.versionState)
	assert.Equal(t, probeFailed, res.modelsState)
}

func TestParseVersionBody(t *testing.T) {
	for body, want := range map[string]string{
		`{"version":"0.11.0"}`:                 "0.11.0",
		`{"version":"0.6.6.post1"}`:            "0.6.6.post1",
		`{"version":"0.10.1.dev12+g1a2b3c4"}`:  "0.10.1.dev12+g1a2b3c4",
		`{"version":"latest"}`:                 "",
		`{"version":0.11}`:                     "",
		`["0.11.0"]`:                           "",
		`{"version":"0.11.0"} trailing`:        "",
		`{"other":"0.11.0"}`:                   "",
		promText:                               "",
		`{"version":"0.11.0\n# TYPE x gauge"}`: "",
	} {
		got, ok := parseVersionBody([]byte(body))
		assert.Equal(t, want, got, body)
		assert.Equal(t, want != "", ok, body)
	}
}

func TestParseModelsBody(t *testing.T) {
	model, served, maxLen, ok := parseModelsBody([]byte(openAIModels))
	assert.True(t, ok)
	assert.Equal(t, "Qwen/Qwen2.5-0.5B-Instruct", model)
	assert.Equal(t, "Qwen/Qwen2.5-0.5B-Instruct", served)
	assert.Equal(t, "2048", maxLen)

	// a LoRA adapter (with a parent) listed first, and no root: the base model by id
	model, served, maxLen, ok = parseModelsBody([]byte(`{"object":"list","data":[` +
		`{"id":"sql-lora","root":"/adapters/sql","parent":"base"},{"id":"base","max_model_len":8192}]}`))
	assert.True(t, ok)
	assert.Equal(t, "base", model)
	assert.Equal(t, "base", served)
	// a served model name: id is the name, root the model
	model, served, _, _ = parseModelsBody([]byte(`{"object":"list","data":[{"id":"llama-70b","root":"/models/llama"}]}`))
	assert.Equal(t, "/models/llama", model)
	assert.Equal(t, "llama-70b", served)
	assert.Equal(t, "8192", maxLen)

	for _, b := range []string{`{"models":["tinyllama"]}`, `{"object":"list","data":[]}`, `{"object":"model","data":[{"id":"x"}]}`,
		`{"error":"Unauthorized"}`, promText, `[]`} {
		_, _, _, ok := parseModelsBody([]byte(b))
		assert.False(t, ok, b)
	}
}

func TestCacheConfigFromMetrics(t *testing.T) {
	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	assert.Nil(t, cacheConfigFromMetrics(rm))
	m := rm.ScopeMetrics().AppendEmpty().Metrics().AppendEmpty()
	m.SetName("vllm:cache_config_info")
	dp := m.SetEmptyGauge().DataPoints().AppendEmpty()
	dp.Attributes().PutStr("cache_dtype", "fp8")
	dp.Attributes().PutStr("enable_prefix_caching", "False")
	dp.Attributes().PutStr("gpu_memory_utilization", "0.85")
	dp.Attributes().PutStr("block_size", "16")
	dp.Attributes().PutStr("num_gpu_blocks", "682")
	assert.Equal(t, map[string]string{
		valKVCacheDtype: "fp8", valEnablePrefixCaching: "false", valGPUMemoryUtilization: "0.85", valNumGPUBlocks: "682",
	}, cacheConfigFromMetrics(rm))
	dp.Attributes().PutStr("num_gpu_blocks", "None")
	assert.NotContains(t, cacheConfigFromMetrics(rm), valNumGPUBlocks)
}

// serverInfoBody is an SGLang /server_info body with the secrets that it carries.
var serverInfoBody = `{"model_path":"Qwen/Qwen3-8B","served_model_name":"qwen3","tp_size":2,"pp_size":1,"dp_size":1,` +
	`"dtype":"auto","quantization":null,"kv_cache_dtype":"auto","mem_fraction_static":0.871,` +
	`"chunked_prefill_size":8192,"max_prefill_tokens":16384,"max_running_requests":null,"context_length":40960,` +
	`"schedule_policy":"fcfs","disable_radix_cache":false,"disable_cuda_graph":false,"speculative_algorithm":null,` +
	`"attention_backend":"fa3","disaggregation_mode":"null","api_key":"` + secretValue + `",` +
	`"admin_api_key":"admin-` + secretValue + `","launch_command":"sglang serve --api-key ` + secretValue + `",` +
	`"lora_paths":["/adapters/` + secretValue + `"],"internal_states":[{"pad":"` + strings.Repeat("x", 100<<10) + `"}],` +
	`"version":"0.5.22","frontend":"python"}`

// deniedFields are the deny list of the research note (section 7), with one
// concrete key per pattern.
var deniedFields = []string{
	"api_key", "admin_api_key", "ssl_keyfile_password", "ssl_keyfile", "ssl_certfile", "ssl_ca_certs",
	"launch_command", "watermark_key", "watermark_key_b", "watermark_config", "hicache_storage_backend_extra_config",
	"model_loader_extra_config", "modelexpress_config", "kv_events_config", "sidecar_args", "tool_server",
	"otlp_traces_endpoint", "lora_paths", "json_model_override_args",
	"remote_instance_weight_loader_seed_instance_ip", "remote_instance_weight_loader_backend", "env_vars",
}

func TestServerInfoDenyList(t *testing.T) {
	for _, k := range deniedFields {
		assert.True(t, sglangDenied(k), k)
	}
	for _, k := range sglangValues {
		assert.False(t, sglangDenied(k), "an allowlisted key is denied: %s", k)
	}
	// every denied field holds a secret, at the top level and in each
	// internal_states entry; an allowlisted key that the top level lacks comes
	// from the first entry
	entry := func(extra string) string {
		var b strings.Builder
		b.WriteString("{" + extra)
		for _, k := range deniedFields {
			b.WriteString(`"` + k + `":"` + secretValue + `-` + k + `",`)
		}
		b.WriteString(`"world_size":2}`)
		return b.String()
	}
	body := entry(`"version":"0.5.22","tp_size":2,`)
	body = body[:len(body)-1] + `,"internal_states":[` + entry(`"max_running_requests":48,"tp_size":4,`) + "," +
		entry(`"max_running_requests":96,`) + "]}"
	values := parseServerInfo([]byte(body))
	assert.Equal(t, map[string]string{sgVersion: "0.5.22", "tp_size": "2", "max_running_requests": "48"}, values)
	for k, v := range values {
		assert.NotContains(t, k+v, secretValue)
	}
}

func serverInfoWith(t *testing.T, routes map[string]func(http.ResponseWriter)) (map[string]string, probeState, *fakeServer) {
	s := newFakeServer(t, routes)
	h, p := s.hostPort(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	values, state := newAPIFetcher().serverInfo(ctx, "http", h, []string{p})
	return values, state, s
}

func TestServerInfoAllowlistAndSecrets(t *testing.T) {
	values, state, s := serverInfoWith(t, map[string]func(http.ResponseWriter){"/server_info": body(200, serverInfoBody)})
	assert.Equal(t, probeAvailable, state)
	assert.Equal(t, map[string]string{
		sgVersion: "0.5.22", sgModelPath: "Qwen/Qwen3-8B", "served_model_name": "qwen3", "tp_size": "2",
		"pp_size": "1", "dp_size": "1", "dtype": "auto", "quantization": "none", "kv_cache_dtype": "auto",
		"mem_fraction_static": "0.871", "chunked_prefill_size": "8192", "max_prefill_tokens": "16384",
		"max_running_requests": "none", "context_length": "40960", "schedule_policy": "fcfs",
		"disable_radix_cache": "false", "disable_cuda_graph": "false", "speculative_algorithm": "none",
		"attention_backend": "fa3", "disaggregation_mode": "null",
	}, values)
	for k, v := range values {
		assert.NotContains(t, k+v, secretValue)
	}
	require.Len(t, s.reqs(), 1)
	assert.Empty(t, s.reqs()[0].header.Get("Authorization"))
}

func TestServerInfoFallbackAnd401(t *testing.T) {
	values, state, s := serverInfoWith(t, map[string]func(http.ResponseWriter){
		"/get_server_info": body(200, `{"version":"0.4.6","tp_size":1}`)})
	assert.Equal(t, probeAvailable, state)
	assert.Equal(t, map[string]string{sgVersion: "0.4.6", "tp_size": "1"}, values)
	assert.Len(t, s.reqs(), 2)

	// a key is needed: not available, and /get_server_info is not tried
	values, state, s = serverInfoWith(t, map[string]func(http.ResponseWriter){
		"/server_info": body(401, `{"error":"Unauthorized"}`), "/get_server_info": body(200, `{"version":"0.5.22"}`)})
	assert.Equal(t, probeUnavailable, state)
	assert.Nil(t, values)
	assert.Len(t, s.reqs(), 1)

	_, state, _ = serverInfoWith(t, map[string]func(http.ResponseWriter){"*": body(200, promText)})
	assert.Equal(t, probeUnavailable, state)
	_, state, _ = serverInfoWith(t, map[string]func(http.ResponseWriter){
		"/server_info": body(200, `{"version":"0.5.22","pad":"`+strings.Repeat("x", maxServerInfoBytes)+`"}`)})
	assert.Equal(t, probeUnavailable, state)
}

// TestSGLangSecretsNeverReachRecordOrLog runs the processor against a fake SGLang
// server with debug logs on: no secret of /server_info reaches an attribute or a log.
func TestSGLangSecretsNeverReachRecordOrLog(t *testing.T) {
	s := newFakeServer(t, map[string]func(http.ResponseWriter){"/server_info": body(200, serverInfoBody)})
	host, port := s.hostPort(t)
	core, logs := observer.New(zapcore.DebugLevel)
	pod := enginePod("uid-sec", engineSGLang, containerSpec{
		Command: []string{"sglang", "serve", "Qwen/Qwen3-8B", "--api-key", secretValue}})
	p := newTestProcessorWithLogger(t, zap.New(core), newAPIFetcher(), &fakeLookup{pods: map[string]podView{host: pod}},
		func(c *Config) {
			c.Engine, c.Shapes, c.ResourceIdentity = engineSGLang, []string{shapeAnchor, shapeInfo}, true
		})

	out := process(t, p, scrape(host, port, gauge("sglang:num_running_reqs", nil)))
	anchor := dpAttrs(t, out, "sglang:num_running_reqs")
	assert.Equal(t, "0.5.22", anchor[keyEngineVersion])
	assert.Equal(t, "fa3", anchor["sglang.effective.attention_backend"])

	raw, err := (&pmetric.JSONMarshaler{}).MarshalMetrics(out)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), secretValue)
	require.NotZero(t, logs.Len())
	for _, e := range logs.All() {
		assert.NotContains(t, e.Message, secretValue)
		for k, v := range e.ContextMap() {
			assert.NotContains(t, k+fmt.Sprint(v), secretValue)
		}
	}
}
