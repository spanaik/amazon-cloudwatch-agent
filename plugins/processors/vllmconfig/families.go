// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package vllmconfig

import "strings"

// The attribution map: each vllm:* family gets only the engine values that
// explain it (design note "Attribution of vLLM Engine Configuration", section 4,
// plus the V0-era names). Names are without the "vllm:" prefix. Counters match
// with and without "_total"; histograms come without "_bucket".

const vllmMetricPrefix = "vllm:"

var (
	prefillValues = []string{valTensorParallelSize, valPipelineParallelSize, valDtype, valQuantization,
		valEnablePrefixCaching, valEnableChunkedPrefill, valMaxNumBatchedTokens}
	decodeValues = []string{valTensorParallelSize, valPipelineParallelSize, valDtype, valQuantization,
		valEnforceEager, valSpeculativeMethod, valNumSpeculativeTokens, valEnableChunkedPrefill}
	requestValues = []string{valTensorParallelSize, valPipelineParallelSize, valDtype, valQuantization,
		valEnforceEager, valSpeculativeMethod}
	queueValues = []string{valMaxNumSeqs, valMaxNumBatchedTokens, valGPUMemoryUtilization, valKVCacheDtype,
		valMaxModelLen, valTensorParallelSize, valPipelineParallelSize}
	throughputValues = []string{valTensorParallelSize, valPipelineParallelSize, valDataParallelSize, valDtype,
		valQuantization, valEnforceEager, valSpeculativeMethod, valMaxNumBatchedTokens}
	kvCacheValues = []string{valKVCacheDtype, valGPUMemoryUtilization, valMaxModelLen,
		valTensorParallelSize, valPipelineParallelSize, valEnablePrefixCaching}
	prefixCacheValues = []string{valEnablePrefixCaching}
	specDecodeValues  = []string{valSpeculativeMethod, valNumSpeculativeTokens}
	requestSizeValues = []string{valMaxModelLen}
)

var familyValues = map[string][]string{}

// familyPatterns match families by prefix and suffix, after the exact names.
var familyPatterns = []struct {
	prefix, suffix string
	values         []string
}{
	{"kv_block_", "_seconds", kvCacheValues},
	{"gpu_prefix_cache_", "", prefixCacheValues},
	{"external_prefix_cache_", "", prefixCacheValues},
	{"spec_decode_", "", specDecodeValues},
}

func init() {
	add := func(values []string, names ...string) {
		for _, n := range names {
			familyValues[n] = values
		}
	}
	add(prefillValues, "time_to_first_token_seconds", "request_prefill_time_seconds")
	add(decodeValues, "inter_token_latency_seconds", "time_per_output_token_seconds",
		"request_time_per_output_token_seconds", "request_decode_time_seconds")
	add(requestValues, "e2e_request_latency_seconds", "request_inference_time_seconds")
	add(queueValues, "request_queue_time_seconds", "time_in_queue_requests", "num_requests_running",
		"num_requests_waiting", "num_requests_waiting_by_reason", "num_requests_swapped", "num_preemptions",
		"request_num_preemptions", "admission_rejections")
	// iteration_tokens_total is a histogram whose base name ends in _total.
	add(throughputValues, "prompt_tokens", "generation_tokens", "prompt_tokens_by_source",
		"iteration_tokens_total", "tokens", "avg_prompt_throughput_toks_per_s", "avg_generation_throughput_toks_per_s")
	add(kvCacheValues, "kv_cache_usage_perc", "gpu_cache_usage_perc", "cpu_cache_usage_perc")
	add(prefixCacheValues, "prefix_cache_queries", "prefix_cache_hits", "cpu_prefix_cache_hit_rate",
		"gpu_prefix_cache_hit_rate", "prompt_tokens_cached", "prompt_tokens_cached_by_source")
	add(requestSizeValues, "request_prompt_tokens", "request_generation_tokens", "request_max_num_generation_tokens",
		"request_params_max_tokens", "request_prefill_kv_computed_tokens", "request_success")
}

// valuesForMetric returns the engine values that the metric family gets, or nil.
// cache_config_info, lora_requests_info, model_forward_time_*, request_params_n,
// http_*, process_* and non-vLLM metrics get none.
func valuesForMetric(name string) []string {
	base, ok := strings.CutPrefix(name, vllmMetricPrefix)
	if !ok {
		return nil
	}
	if v, ok := familyValues[base]; ok {
		return v
	}
	trimmed, hadTotal := strings.CutSuffix(base, "_total")
	if hadTotal {
		if v, ok := familyValues[trimmed]; ok {
			return v
		}
	}
	// _created series of a counter or histogram are not the family itself.
	if strings.HasSuffix(trimmed, "_created") {
		return nil
	}
	for _, p := range familyPatterns {
		if strings.HasPrefix(trimmed, p.prefix) && strings.HasSuffix(trimmed, p.suffix) && len(trimmed) > len(p.prefix)+len(p.suffix) {
			return p.values
		}
	}
	return nil
}
