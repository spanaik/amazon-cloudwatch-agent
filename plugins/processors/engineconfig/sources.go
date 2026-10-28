// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package engineconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

const (
	// maxBodyBytes bounds the body read from /version and /v1/models.
	maxBodyBytes = 64 << 10
	// maxServerInfoBytes bounds the body of the SGLang /server_info, which holds
	// every server argument and the scheduler state.
	maxServerInfoBytes = 1 << 20
	// maxProbePorts bounds the ports tried per endpoint: the scrape port first.
	maxProbePorts = 3

	cacheConfigInfoMetric = "vllm:cache_config_info"
)

// Value sources, kept in memory for the debug log of a record.
const (
	srcArgs        = "pod args"
	srcEnv         = "env_map"
	srcVersion     = "GET /version"
	srcModels      = "GET /v1/models"
	srcServerInfo  = "GET /server_info"
	srcCacheConfig = cacheConfigInfoMetric
	srcV0Family    = "V0-only family"
	srcStartupLine = "startup log line"
	srcVersionRule = "version rule"
	srcImageTag    = "image tag"
	srcOwner       = "owner references"
)

// sourced is a value and the source that gave it.
type sourced struct {
	value  string
	source string
}

type probeState int

const (
	// probeUnavailable: a server answered, but not with the expected JSON shape.
	probeUnavailable probeState = iota
	probeAvailable
	// probeFailed: no port gave an HTTP answer (connection error or timeout).
	probeFailed
)

type httpResult struct {
	version      string
	versionState probeState
	model        string // root, else id
	servedModel  string // id: the served model name
	maxModelLen  string
	modelsState  probeState
}

// httpFetcher reads the engine HTTP routes; tests replace it.
type httpFetcher interface {
	fetch(ctx context.Context, scheme, host string, ports []string) httpResult
	serverInfo(ctx context.Context, scheme, host string, ports []string) (map[string]string, probeState)
}

// apiFetcher reads the engine HTTP routes. It sends only GET requests without
// credentials, does not follow redirects and does not use a proxy.
type apiFetcher struct {
	client *http.Client
}

func newAPIFetcher() *apiFetcher {
	transport := &http.Transport{
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: 2 * time.Second}).DialContext,
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 16 << 10,
		TLSHandshakeTimeout:    2 * time.Second,
	}
	return &apiFetcher{client: &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

func (f *apiFetcher) fetch(ctx context.Context, scheme, host string, ports []string) httpResult {
	if scheme != "https" {
		scheme = "http"
	}
	var res httpResult
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		res.versionState = f.probe(ctx, scheme, host, ports, []string{"/version"}, maxBodyBytes, func(body []byte) bool {
			v, ok := parseVersionBody(body)
			if ok {
				res.version = v
			}
			return ok
		})
	}()
	res.modelsState = f.probe(ctx, scheme, host, ports, []string{"/v1/models"}, maxBodyBytes, func(body []byte) bool {
		model, served, maxLen, ok := parseModelsBody(body)
		if ok {
			res.model, res.servedModel, res.maxModelLen = model, served, maxLen
		}
		return ok
	})
	wg.Wait()
	return res
}

// serverInfo reads the SGLang GET /server_info, then /get_server_info on a 404,
// and keeps only the allowlisted keys. The body carries secrets (api_key,
// admin_api_key, launch_command), so it is never logged or kept.
func (f *apiFetcher) serverInfo(ctx context.Context, scheme, host string, ports []string) (map[string]string, probeState) {
	if scheme != "https" {
		scheme = "http"
	}
	var values map[string]string
	state := f.probe(ctx, scheme, host, ports, []string{"/server_info", "/get_server_info"}, maxServerInfoBytes,
		func(body []byte) bool {
			values = parseServerInfo(body)
			return values != nil
		})
	return values, state
}

// probe tries the ports in order until one gives a body that accept takes. On a
// port, the next path is tried only after a 404. A 401 means that the route needs
// a key, so it is not available.
func (f *apiFetcher) probe(ctx context.Context, scheme, host string, ports, paths []string, limit int64, accept func([]byte) bool) probeState {
	answered := false
	for i, port := range ports {
		// share the remaining time among the remaining ports
		timeout := defaultTimeout
		if deadline, ok := ctx.Deadline(); ok {
			timeout = time.Until(deadline) / time.Duration(len(ports)-i)
		}
		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		for _, path := range paths {
			body, status, err := f.get(reqCtx, scheme+"://"+net.JoinHostPort(host, port)+path, limit)
			if err != nil {
				answered = answered || errors.Is(err, errBodyTooLarge)
				break
			}
			answered = true
			if status == http.StatusOK && accept(body) {
				cancel()
				return probeAvailable
			}
			if status != http.StatusNotFound {
				break
			}
		}
		cancel()
	}
	if answered {
		return probeUnavailable
	}
	return probeFailed
}

var errBodyTooLarge = errors.New("response body too large")

func (f *apiFetcher) get(ctx context.Context, url string, limit int64) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "cloudwatch-agent-engineconfig")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if int64(len(body)) > limit {
		return nil, resp.StatusCode, errBodyTooLarge
	}
	return body, resp.StatusCode, nil
}

// decodeObject decodes a JSON object strictly: one value, an object, nothing after it.
func decodeObject(body []byte) (map[string]json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	var obj map[string]json.RawMessage
	if err := dec.Decode(&obj); err != nil || obj == nil {
		return nil, false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return obj, true
}

// parseVersionBody accepts {"version":"0.11.0"}. A Prometheus text body, which a
// prometheus_client server sends on every path, is not available.
func parseVersionBody(body []byte) (string, bool) {
	obj, ok := decodeObject(body)
	if !ok {
		return "", false
	}
	raw, ok := obj["version"]
	if !ok {
		return "", false
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", false
	}
	if len(v) > 64 || !versionRE.MatchString(v) {
		return "", false
	}
	return v, true
}

// parseModelsBody accepts the OpenAI shape {"object":"list","data":[{"id":...,
// "root":...,"max_model_len":...}]}. It takes the first entry that is not a LoRA
// adapter (no parent). The model is root, else id; the served model name is id.
func parseModelsBody(body []byte) (string, string, string, bool) {
	obj, ok := decodeObject(body)
	if !ok {
		return "", "", "", false
	}
	if raw, has := obj["object"]; has {
		var o string
		if json.Unmarshal(raw, &o) != nil || o != "list" {
			return "", "", "", false
		}
	}
	raw, has := obj["data"]
	if !has {
		return "", "", "", false
	}
	var data []map[string]json.RawMessage
	if json.Unmarshal(raw, &data) != nil || len(data) == 0 {
		return "", "", "", false
	}
	entry := data[0]
	for _, e := range data {
		if p, has := e["parent"]; !has || string(p) == "null" {
			entry = e
			break
		}
	}
	str := func(key string) string {
		var s string
		if r, has := entry[key]; has && json.Unmarshal(r, &s) == nil {
			return s
		}
		return ""
	}
	id, root := str("id"), str("root")
	if id == "" {
		return "", "", "", false
	}
	model := id
	if root != "" {
		model = root
	}
	if !validValue(model) || !validValue(id) {
		return "", "", "", false
	}
	maxModelLen := ""
	if r, has := entry["max_model_len"]; has {
		var n float64
		if json.Unmarshal(r, &n) == nil && n > 0 && n == math.Trunc(n) && n < 1e15 {
			maxModelLen = strconv.FormatInt(int64(n), 10)
		}
	}
	return model, id, maxModelLen, true
}

// parseServerInfo keeps the allowlisted keys of a /server_info object. It first
// drops the denied keys at the top level and in every internal_states entry; a
// key that the top level lacks is taken from the first internal_states entry.
// A JSON null is "none"; objects and lists are left out.
func parseServerInfo(body []byte) map[string]string {
	obj, ok := decodeObject(body)
	if !ok {
		return nil
	}
	dropDenied(obj)
	var states []map[string]json.RawMessage
	if raw, has := obj["internal_states"]; has && json.Unmarshal(raw, &states) == nil {
		for _, st := range states {
			dropDenied(st)
		}
	}
	out := map[string]string{}
	for _, name := range sglangValues {
		raw, has := obj[name]
		if !has && len(states) > 0 {
			raw, has = states[0][name]
		}
		if !has {
			continue
		}
		var v any
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		s, known := jsonScalar(v)
		switch {
		case v == nil:
			s, known = noneValue, true
		case name == sgVersion:
			known = known && versionRE.MatchString(s)
		}
		if known && validValue(s) {
			out[name] = s
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func dropDenied(obj map[string]json.RawMessage) {
	for k := range obj {
		if sglangDenied(k) {
			delete(obj, k)
		}
	}
}

// cacheConfigFromMetrics reads the vllm:cache_config_info labels of the scrape:
// cache_dtype (as kv_cache_dtype), enable_prefix_caching, gpu_memory_utilization
// and num_gpu_blocks. It returns nil if the family is not in the scrape.
func cacheConfigFromMetrics(rm pmetric.ResourceMetrics) map[string]string {
	sms := rm.ScopeMetrics()
	for i := 0; i < sms.Len(); i++ {
		ms := sms.At(i).Metrics()
		for j := 0; j < ms.Len(); j++ {
			m := ms.At(j)
			if m.Name() != cacheConfigInfoMetric {
				continue
			}
			var attrs pcommon.Map
			switch m.Type() {
			case pmetric.MetricTypeGauge:
				if m.Gauge().DataPoints().Len() == 0 {
					continue
				}
				attrs = m.Gauge().DataPoints().At(0).Attributes()
			case pmetric.MetricTypeSum:
				if m.Sum().DataPoints().Len() == 0 {
					continue
				}
				attrs = m.Sum().DataPoints().At(0).Attributes()
			default:
				continue
			}
			out := map[string]string{}
			if v, ok := attrs.Get("cache_dtype"); ok && validValue(v.AsString()) {
				out[valKVCacheDtype] = v.AsString()
			}
			if v, ok := attrs.Get("enable_prefix_caching"); ok {
				if b, _, ok := convBool(v.AsString()); ok && b != "" {
					out[valEnablePrefixCaching] = b
				}
			}
			if v, ok := attrs.Get("gpu_memory_utilization"); ok {
				if f, _, ok := convFloat(v.AsString()); ok {
					out[valGPUMemoryUtilization] = f
				}
			}
			if v, ok := attrs.Get("num_gpu_blocks"); ok {
				if n, _, ok := convInt(v.AsString()); ok {
					out[valNumGPUBlocks] = n
				}
			}
			return out
		}
	}
	return nil
}

// hasFamily reports whether the scrape has a family whose name starts with prefix.
func hasFamily(rm pmetric.ResourceMetrics, prefix string) bool {
	found := false
	forEachMetric(rm, func(_ pmetric.ScopeMetrics, m pmetric.Metric) bool {
		found = strings.HasPrefix(m.Name(), prefix)
		return !found
	})
	return found
}

// hasV0Family reports whether the scrape has a family that only the vLLM V0
// engine emits.
func hasV0Family(rm pmetric.ResourceMetrics) bool {
	found := false
	forEachMetric(rm, func(_ pmetric.ScopeMetrics, m pmetric.Metric) bool {
		found = v0OnlyFamilies[m.Name()]
		return !found
	})
	return found
}
