// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package engineconfig

import (
	"context"
	"fmt"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

// engine holds what differs between inference engines.
type engine interface {
	name() string
	// anchorFamily is the gauge whose data points carry the record in the anchor shape.
	anchorFamily() string
	// owns reports whether a scrape comes from this engine.
	owns(rm pmetric.ResourceMetrics) bool
	// configured gives the configured values of the container from its parsed pod
	// spec: a value, or "default" for a flag that the customer did not set.
	configured(c containerInfo) map[string]sourced
	// routes reads the version and effective values from the engine HTTP routes.
	routes(ctx context.Context, t scrapeTarget, ports []string) routeResult
	// scraped reads effective values from the engine metrics of the scrape.
	scraped(rm pmetric.ResourceMetrics) map[string]sourced
	// identity gives the engine version and generation from the effective values
	// and the pod spec; an empty value when no source gives one.
	identity(effective map[string]sourced, c containerInfo) (version, generation sourced)
}

// routeResult holds the values of the HTTP routes. failed means that no port
// gave an HTTP answer, so the read is tried again on the retry interval.
type routeResult struct {
	values map[string]sourced
	failed bool
}

func newEngine(name string, fetcher httpFetcher) (engine, error) {
	switch name {
	case engineVLLM:
		return vllmEngine{fetcher: fetcher}, nil
	case engineSGLang:
		return sglangEngine{fetcher: fetcher}, nil
	}
	return nil, fmt.Errorf("unknown engine %q", name)
}

// vllmConfigured are the vLLM values that the pod args (and the env contract) give.
var vllmConfigured = []string{
	valModel, valServedModelName, valTensorParallelSize, valPipelineParallelSize, valDataParallelSize, valDtype, valQuantization,
	valEnforceEager, valSpeculativeMethod, valNumSpeculativeTokens, valMaxNumSeqs, valMaxNumBatchedTokens,
	valEnableChunkedPrefill, valEnablePrefixCaching, valGPUMemoryUtilization, valKVCacheDtype, valMaxModelLen,
}

type vllmEngine struct{ fetcher httpFetcher }

func (vllmEngine) name() string { return engineVLLM }

func (vllmEngine) anchorFamily() string { return "vllm:num_requests_running" }

func (vllmEngine) owns(rm pmetric.ResourceMetrics) bool { return hasFamily(rm, vllmMetricPrefix) }

// configured applies the source precedence per value: the pod args, then the env contract.
func (vllmEngine) configured(c containerInfo) map[string]sourced {
	out := map[string]sourced{}
	for _, name := range vllmConfigured {
		if v, i, ok := pick(fromArgs(c, name), fromEnv(c, name)); ok && validValue(v) {
			out[name] = sourced{v, []string{srcArgs, srcEnv}[i]}
		}
	}
	return out
}

// routes reads GET /version (the version) and GET /v1/models (the model, the
// served model name and max_model_len).
func (e vllmEngine) routes(ctx context.Context, t scrapeTarget, ports []string) routeResult {
	res := e.fetcher.fetch(ctx, t.scheme, t.host, ports)
	out := routeResult{values: map[string]sourced{}, failed: res.versionState == probeFailed || res.modelsState == probeFailed}
	if res.versionState == probeAvailable {
		out.values[valVersion] = sourced{res.version, srcVersion}
	}
	if res.modelsState == probeAvailable {
		out.values[valModel] = sourced{res.model, srcModels}
		out.values[valServedModelName] = sourced{res.servedModel, srcModels}
		if res.maxModelLen != "" {
			out.values[valMaxModelLen] = sourced{res.maxModelLen, srcModels}
		}
	}
	return out
}

// scraped reads the vllm:cache_config_info labels, and marks V0 when the scrape
// has a family that only the V0 engine emits.
func (vllmEngine) scraped(rm pmetric.ResourceMetrics) map[string]sourced {
	out := map[string]sourced{}
	for k, v := range cacheConfigFromMetrics(rm) {
		out[k] = sourced{v, srcCacheConfig}
	}
	if hasV0Family(rm) {
		out[valGeneration] = sourced{"V0", srcV0Family}
	}
	return out
}

// identity: the version of GET /version, else the startup line. The generation:
// the startup line, or a family that only the V0 engine emits, says it; else
// the version rule (deriveGeneration) with VLLM_USE_V1.
func (vllmEngine) identity(effective map[string]sourced, c containerInfo) (sourced, sourced) {
	version := effective[valVersion]
	if g := effective[valGeneration]; g.value != "" {
		return version, g
	}
	if v := deriveGeneration(version.value, c.UseV1); v != "" {
		return version, sourced{v, srcVersionRule}
	}
	return version, sourced{}
}

type sglangEngine struct{ fetcher httpFetcher }

func (sglangEngine) name() string { return engineSGLang }

func (sglangEngine) anchorFamily() string { return "sglang:num_running_reqs" }

func (sglangEngine) owns(rm pmetric.ResourceMetrics) bool { return hasFamily(rm, "sglang:") }

func (sglangEngine) configured(c containerInfo) map[string]sourced {
	out := map[string]sourced{}
	for _, name := range sglangValues {
		if v, _, ok := pick(fromArgs(c, name)); ok && validValue(v) {
			out[name] = sourced{v, srcArgs}
		}
	}
	return out
}

// routes reads GET /server_info on the scrape port, where SGLang serves both its
// metrics and its API: the version and the allowlisted effective values.
func (e sglangEngine) routes(ctx context.Context, t scrapeTarget, ports []string) routeResult {
	values, state := e.fetcher.serverInfo(ctx, t.scheme, t.host, ports[:min(1, len(ports))])
	out := routeResult{values: map[string]sourced{}, failed: state == probeFailed}
	for k, v := range values {
		out.values[k] = sourced{v, srcServerInfo}
	}
	return out
}

// scraped reads the engine_type label of the anchor data points, for the phase.
func (e sglangEngine) scraped(rm pmetric.ResourceMetrics) map[string]sourced {
	var out map[string]sourced
	forEachMetric(rm, func(_ pmetric.ScopeMetrics, m pmetric.Metric) bool {
		if m.Name() != e.anchorFamily() {
			return true
		}
		forEachDataPoint(m, func(attrs pcommon.Map, _, _ pcommon.Timestamp) {
			if v, ok := attrs.Get(valEngineType); ok && out == nil && validValue(v.AsString()) {
				out = map[string]sourced{valEngineType: {v.AsString(), srcEngineType}}
			}
		})
		return out == nil
	})
	return out
}

// identity: the version of /server_info, else the release in the image tag of
// the container. SGLang has no generation.
func (sglangEngine) identity(effective map[string]sourced, c containerInfo) (sourced, sourced) {
	if v := effective[valVersion]; v.value != "" {
		return v, sourced{}
	}
	if c.Image != "" {
		return sourced{c.Image, srcImageTag}, sourced{}
	}
	return sourced{}, sourced{}
}
