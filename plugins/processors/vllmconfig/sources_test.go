// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package vllmconfig

import (
	"context"
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
	model, maxLen, ok := parseModelsBody([]byte(openAIModels))
	assert.True(t, ok)
	assert.Equal(t, "Qwen/Qwen2.5-0.5B-Instruct", model)
	assert.Equal(t, "2048", maxLen)

	// a LoRA adapter (with a parent) listed first, and no root: the base model by id
	model, maxLen, ok = parseModelsBody([]byte(`{"object":"list","data":[` +
		`{"id":"sql-lora","root":"/adapters/sql","parent":"base"},{"id":"base","max_model_len":8192}]}`))
	assert.True(t, ok)
	assert.Equal(t, "base", model)
	assert.Equal(t, "8192", maxLen)

	for _, b := range []string{`{"models":["tinyllama"]}`, `{"object":"list","data":[]}`, `{"object":"model","data":[{"id":"x"}]}`,
		`{"error":"Unauthorized"}`, promText, `[]`} {
		_, _, ok := parseModelsBody([]byte(b))
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
	assert.Equal(t, map[string]string{
		valKVCacheDtype: "fp8", valEnablePrefixCaching: "false", valGPUMemoryUtilization: "0.85",
	}, cacheConfigFromMetrics(rm))
}
