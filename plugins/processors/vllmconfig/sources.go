// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package vllmconfig

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
	// maxProbePorts bounds the ports tried per endpoint: the scrape port first.
	maxProbePorts = 3

	cacheConfigInfoMetric = "vllm:cache_config_info"
)

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
	model        string
	maxModelLen  string
	modelsState  probeState
}

type httpFetcher interface {
	fetch(ctx context.Context, scheme, host string, ports []string) httpResult
}

// apiFetcher reads the vLLM HTTP API. It sends only GET requests without
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
		res.versionState = f.probe(ctx, scheme, host, ports, "/version", func(body []byte) bool {
			v, ok := parseVersionBody(body)
			if ok {
				res.version = v
			}
			return ok
		})
	}()
	res.modelsState = f.probe(ctx, scheme, host, ports, "/v1/models", func(body []byte) bool {
		model, maxLen, ok := parseModelsBody(body)
		if ok {
			res.model, res.maxModelLen = model, maxLen
		}
		return ok
	})
	wg.Wait()
	return res
}

// probe tries the ports in order until one gives a body that accept takes.
func (f *apiFetcher) probe(ctx context.Context, scheme, host string, ports []string, path string, accept func([]byte) bool) probeState {
	answered := false
	for i, port := range ports {
		// share the remaining time among the remaining ports
		timeout := defaultTimeout
		if deadline, ok := ctx.Deadline(); ok {
			timeout = time.Until(deadline) / time.Duration(len(ports)-i)
		}
		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		body, status, err := f.get(reqCtx, scheme+"://"+net.JoinHostPort(host, port)+path)
		cancel()
		if err != nil {
			if errors.Is(err, errBodyTooLarge) {
				answered = true
			}
			continue
		}
		answered = true
		if status == http.StatusOK && accept(body) {
			return probeAvailable
		}
	}
	if answered {
		return probeUnavailable
	}
	return probeFailed
}

var errBodyTooLarge = errors.New("response body too large")

func (f *apiFetcher) get(ctx context.Context, url string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "cloudwatch-agent-vllmconfig")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if len(body) > maxBodyBytes {
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
// adapter (no parent). The model is root, else id.
func parseModelsBody(body []byte) (string, string, bool) {
	obj, ok := decodeObject(body)
	if !ok {
		return "", "", false
	}
	if raw, has := obj["object"]; has {
		var o string
		if json.Unmarshal(raw, &o) != nil || o != "list" {
			return "", "", false
		}
	}
	raw, has := obj["data"]
	if !has {
		return "", "", false
	}
	var data []map[string]json.RawMessage
	if json.Unmarshal(raw, &data) != nil || len(data) == 0 {
		return "", "", false
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
		return "", "", false
	}
	model := id
	if root != "" {
		model = root
	}
	if !validValue(model) {
		return "", "", false
	}
	maxModelLen := ""
	if r, has := entry["max_model_len"]; has {
		var n float64
		if json.Unmarshal(r, &n) == nil && n > 0 && n == math.Trunc(n) && n < 1e15 {
			maxModelLen = strconv.FormatInt(int64(n), 10)
		}
	}
	return model, maxModelLen, true
}

// cacheConfigFromMetrics reads the vllm:cache_config_info labels of the scrape:
// cache_dtype, enable_prefix_caching and gpu_memory_utilization. It returns nil if
// the family is not in the scrape.
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
			return out
		}
	}
	return nil
}

// hasVLLMMetrics reports whether the scrape has a vllm:* family, and whether it has
// a family that only the V0 engine emits.
func hasVLLMMetrics(rm pmetric.ResourceMetrics) (bool, bool) {
	vllm, v0 := false, false
	sms := rm.ScopeMetrics()
	for i := 0; i < sms.Len(); i++ {
		ms := sms.At(i).Metrics()
		for j := 0; j < ms.Len(); j++ {
			name := ms.At(j).Name()
			if strings.HasPrefix(name, vllmMetricPrefix) {
				vllm = true
				if v0OnlyFamilies[name] {
					v0 = true
				}
			}
		}
	}
	return vllm, v0
}
