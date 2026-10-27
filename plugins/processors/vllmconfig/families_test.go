// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package vllmconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValuesForMetric(t *testing.T) {
	tests := []struct {
		name string
		want []string
	}{
		{"vllm:time_to_first_token_seconds", prefillValues},
		{"vllm:request_prefill_time_seconds", prefillValues},
		{"vllm:inter_token_latency_seconds", decodeValues},
		{"vllm:time_per_output_token_seconds", decodeValues},
		{"vllm:request_decode_time_seconds", decodeValues},
		{"vllm:e2e_request_latency_seconds", requestValues},
		{"vllm:request_queue_time_seconds", queueValues},
		{"vllm:time_in_queue_requests", queueValues},
		{"vllm:num_requests_swapped", queueValues},
		{"vllm:num_preemptions_total", queueValues},
		{"vllm:num_preemptions", queueValues},
		{"vllm:prompt_tokens_total", throughputValues},
		{"vllm:prompt_tokens", throughputValues},
		{"vllm:tokens_total", throughputValues},
		{"vllm:avg_generation_throughput_toks_per_s", throughputValues},
		// a histogram whose base name ends in _total
		{"vllm:iteration_tokens_total", throughputValues},
		// prompt_tokens must not take prompt_tokens_cached
		{"vllm:prompt_tokens_cached_total", prefixCacheValues},
		{"vllm:prompt_tokens_cached_by_source_total", prefixCacheValues},
		{"vllm:prompt_tokens_by_source_total", throughputValues},
		{"vllm:kv_cache_usage_perc", kvCacheValues},
		{"vllm:gpu_cache_usage_perc", kvCacheValues},
		{"vllm:cpu_cache_usage_perc", kvCacheValues},
		{"vllm:kv_block_lifetime_seconds", kvCacheValues},
		{"vllm:prefix_cache_queries_total", prefixCacheValues},
		{"vllm:gpu_prefix_cache_hits_total", prefixCacheValues},
		{"vllm:gpu_prefix_cache_hit_rate", prefixCacheValues},
		{"vllm:cpu_prefix_cache_hit_rate", prefixCacheValues},
		{"vllm:external_prefix_cache_queries_total", prefixCacheValues},
		{"vllm:spec_decode_num_draft_tokens_total", specDecodeValues},
		{"vllm:spec_decode_efficiency", specDecodeValues},
		{"vllm:request_prompt_tokens", requestSizeValues},
		{"vllm:request_params_max_tokens", requestSizeValues},
		{"vllm:request_success_total", requestSizeValues},
		// no values
		{"vllm:cache_config_info", nil},
		{"vllm:lora_requests_info", nil},
		{"vllm:model_forward_time_milliseconds", nil},
		{"vllm:request_params_n", nil},
		{"vllm:engine_sleep_state", nil},
		{"vllm:prompt_tokens_created", nil},
		{"vllm:spec_decode_num_drafts_created", nil},
		{"vllm:mm_cache_queries_total", nil},
		{"vllm:estimated_flops_per_gpu_total", nil},
		{"http_requests_total", nil},
		{"process_cpu_seconds_total", nil},
		{"python_gc_collections_total", nil},
		{"time_to_first_token_seconds", nil},
		{"solstice_tokens_generated_total", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, valuesForMetric(tt.name))
		})
	}
}
