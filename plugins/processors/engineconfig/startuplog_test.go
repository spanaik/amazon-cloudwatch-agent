// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package engineconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// placeLog writes content as <root>/<namespace>_<pod>_<uid>/<container>/<name>.
func placeLog(t *testing.T, root, namespace, pod, uid, container, name string, content []byte) string {
	dir := filepath.Join(root, namespace+"_"+pod+"_"+uid, container)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, content, 0o600))
	return path
}

func values(m map[string]sourced) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v.value
	}
	return out
}

// Startup lines built from the format string of each release in the vLLM clone,
// with the log prefix of vllm/logger.py _FORMAT of the same release.
const (
	// v0.6.3: vllm/engine/llm_engine.py L237-292 (LLMEngine.__init__, the field
	// list with no comma after chunked_prefill_enabled), logger.py L19-20;
	// speculative_config is SpeculativeConfig.__repr__ of vllm/config.py L1516-1522.
	startupLineV063 = "INFO 10-01 08:00:00 llm_engine.py:237] Initializing an LLM engine (v0.6.3) with config: " +
		"model='mistralai/Mistral-7B-Instruct-v0.3', " +
		"speculative_config=SpeculativeConfig(draft_model='[ngram]', num_spec_tokens=5), " +
		"tokenizer='mistralai/Mistral-7B-Instruct-v0.3', skip_tokenizer_init=False, tokenizer_mode=auto, revision=None, " +
		"override_neuron_config=None, rope_scaling=None, rope_theta=None, tokenizer_revision=None, " +
		"trust_remote_code=False, dtype=torch.bfloat16, max_seq_len=32768, download_dir=None, " +
		"load_format=LoadFormat.AUTO, tensor_parallel_size=2, pipeline_parallel_size=1, " +
		"disable_custom_all_reduce=False, quantization=None, enforce_eager=False, kv_cache_dtype=auto, " +
		"quantization_param_path=None, device_config=cuda, " +
		"decoding_config=DecodingConfig(guided_decoding_backend='outlines'), " +
		"observability_config=ObservabilityConfig(otlp_traces_endpoint=None, collect_model_forward_time=False, " +
		"collect_model_execute_time=False), seed=0, served_model_name=mistral-7b, use_v2_block_manager=True, " +
		"num_scheduler_steps=1, chunked_prefill_enabled=False multi_step_stream_outputs=True, " +
		"enable_prefix_caching=False, use_async_output_proc=True, use_cached_outputs=False, mm_processor_kwargs=None)"
	// v0.8.5 V0: vllm/engine/llm_engine.py L240-246 with VllmConfig.__str__ of
	// vllm/config.py L4051-4085 (note the double space before device_config),
	// logger.py L22-24; speculative_config is vllm/config.py L2607-2611.
	startupLineV085 = "INFO 05-02 09:10:11 [llm_engine.py:240] Initializing a V0 LLM engine (v0.8.5) with config: " +
		"model='Qwen/Qwen2.5-7B-Instruct-AWQ', " +
		"speculative_config=SpeculativeConfig(method='draft_model', model='Qwen/Qwen2.5-0.5B-Instruct', num_spec_tokens=4), " +
		"tokenizer='Qwen/Qwen2.5-7B-Instruct-AWQ', skip_tokenizer_init=False, tokenizer_mode=auto, revision=None, " +
		"override_neuron_config=None, tokenizer_revision=None, trust_remote_code=False, dtype=torch.float16, " +
		"max_seq_len=32768, download_dir=None, load_format=LoadFormat.AUTO, tensor_parallel_size=1, " +
		"pipeline_parallel_size=1, disable_custom_all_reduce=False, quantization=awq, enforce_eager=True, " +
		"kv_cache_dtype=auto,  device_config=cuda, " +
		"decoding_config=DecodingConfig(guided_decoding_backend='xgrammar', reasoning_backend=None), " +
		"observability_config=ObservabilityConfig(show_hidden_metrics=False, otlp_traces_endpoint=None, " +
		"collect_model_forward_time=False, collect_model_execute_time=False), seed=None, served_model_name=qwen-7b, " +
		"num_scheduler_steps=1, multi_step_stream_outputs=True, enable_prefix_caching=False, " +
		"chunked_prefill_enabled=False, use_async_output_proc=True, disable_mm_preprocessor_cache=False, " +
		"mm_processor_kwargs=None, pooler_config=None, " +
		`compilation_config={"splitting_ops":[],"compile_sizes":[],"cudagraph_capture_sizes":[8,4,2,1],"max_capture_size":8}, ` +
		"use_cached_outputs=False, "
	// v0.32.0 V1: vllm/v1/engine/core.py L131-136 with VllmConfig.__str__ of
	// vllm/config/vllm.py L2703-2738, logger.py L28-32; speculative_config is
	// vllm/config/speculative.py L1990-2004.
	startupLineV0320 = "(EngineCore pid=211) INFO 10-15 12:41:46 [core.py:132] Initializing a V1 LLM engine (v0.32.0) with config: " +
		"model='meta-llama/Llama-3.1-70B-Instruct', " +
		"speculative_config=SpeculativeConfig(method='eagle3', model='yuhuili/EAGLE3-LLaMA3.3-Instruct-70B', num_spec_tokens=3), " +
		"tokenizer='meta-llama/Llama-3.1-70B-Instruct', skip_tokenizer_init=False, tokenizer_mode=auto, revision=None, " +
		"tokenizer_revision=None, trust_remote_code=False, dtype=torch.bfloat16, max_seq_len=131072, " +
		"download_dir=None, load_format=auto, tensor_parallel_size=4, pipeline_parallel_size=2, " +
		"data_parallel_size=1, decode_context_parallel_size=1, dcp_comm_backend=ag_rs, " +
		"disable_custom_all_reduce=False, quantization=fp8, " +
		"quantization_config={'activation_scheme': 'dynamic', 'quant_method': 'fp8'}, enforce_eager=False, " +
		"aux_output_config=AuxOutputConfig(enabled=False), kv_cache_dtype=fp8_e4m3, device_config=cuda, " +
		"structured_outputs_config=StructuredOutputsConfig(backend='auto', reasoning_parser=''), " +
		"observability_config=ObservabilityConfig(show_hidden_metrics_for_version=None), seed=0, " +
		"served_model_name=llama-70b, enable_prefix_caching=True, enable_chunked_prefill=True, pooler_config=None, " +
		"compilation_config={'mode': <CompilationMode.VLLM_COMPILE: 3>, 'cudagraph_capture_sizes': [1, 2, 4, 8]}, " +
		"kernel_config=KernelConfig()"
)

// TestStartupLineFixtures reads the real v0.28.0 and v0.29.0 lines (CRI files
// captured from a cluster, named like /var/log/containers/<pod>_<namespace>_
// <container>-<id>.log) and the lines built from the format of v0.6.3, v0.8.5
// and v0.32.0, from the /var/log/pods layout.
func TestStartupLineFixtures(t *testing.T) {
	type fixture struct {
		namespace, container string
		content              []byte
		want                 map[string]string
	}
	real := map[string]map[string]string{
		"vllm-v0285-5c4b": {
			valVersion: "0.28.0", valGeneration: "V1", valModel: "google/gemma-4-26B-A4B-it",
			valServedModelName: "gemma-4-26b-a4b-it", valDtype: "bfloat16", valMaxModelLen: "131072",
			valTensorParallelSize: "1", valPipelineParallelSize: "1", valDataParallelSize: "1",
			valQuantization: "none", valEnforceEager: "false", valKVCacheDtype: "auto",
			valEnablePrefixCaching: "true", valEnableChunkedPrefill: "true", valSpeculativeMethod: "mtp",
			valNumSpeculativeTokens: "4",
		},
		"vllm-v0290-6a1e": {
			valVersion: "0.29.0", valGeneration: "V1", valModel: "/data_nvm2/qwen3.8/model",
			valServedModelName: "qwen3.6", valDtype: "bfloat16", valMaxModelLen: "524288",
			valTensorParallelSize: "2", valPipelineParallelSize: "1", valDataParallelSize: "1",
			valQuantization: "compressed-tensors", valEnforceEager: "false", valKVCacheDtype: "fp8",
			valEnablePrefixCaching: "true", valEnableChunkedPrefill: "true", valSpeculativeMethod: "mtp",
			valNumSpeculativeTokens: "3",
		},
	}
	fixtures := map[string]fixture{
		// "Initializing an LLM engine" is V0; v0.6.3 prints no data parallel size
		"vllm-v063": {"legacy", "server", []byte("2026-10-15T12:44:00.1Z stdout F " + startupLineV063 + "\n"), map[string]string{
			valVersion: "0.6.3", valGeneration: "V0", valModel: "mistralai/Mistral-7B-Instruct-v0.3",
			valServedModelName: "mistral-7b", valDtype: "bfloat16", valMaxModelLen: "32768",
			valTensorParallelSize: "2", valPipelineParallelSize: "1", valQuantization: "none",
			valEnforceEager: "false", valKVCacheDtype: "auto", valEnablePrefixCaching: "false",
			valEnableChunkedPrefill: "false", valSpeculativeMethod: "ngram", valNumSpeculativeTokens: "5",
		}},
		"vllm-v085": {"legacy", "vllm", []byte("2026-10-15T12:43:00.1Z stdout F " + startupLineV085 + "\n"), map[string]string{
			valVersion: "0.8.5", valGeneration: "V0", valModel: "Qwen/Qwen2.5-7B-Instruct-AWQ",
			valServedModelName: "qwen-7b", valDtype: "float16", valMaxModelLen: "32768",
			valTensorParallelSize: "1", valPipelineParallelSize: "1", valQuantization: "awq",
			valEnforceEager: "true", valKVCacheDtype: "auto", valEnablePrefixCaching: "false",
			valEnableChunkedPrefill: "false", valSpeculativeMethod: "draft_model", valNumSpeculativeTokens: "4",
		}},
		"vllm-v0320": {"inference", "vllm", []byte("2026-10-15T12:40:01.1Z stdout F " + startupLineV0320 + "\n"), map[string]string{
			valVersion: "0.32.0", valGeneration: "V1", valModel: "meta-llama/Llama-3.1-70B-Instruct",
			valServedModelName: "llama-70b", valDtype: "bfloat16", valMaxModelLen: "131072",
			valTensorParallelSize: "4", valPipelineParallelSize: "2", valDataParallelSize: "1",
			valQuantization: "fp8", valEnforceEager: "false", valKVCacheDtype: "fp8_e4m3",
			valEnablePrefixCaching: "true", valEnableChunkedPrefill: "true", valSpeculativeMethod: "eagle3",
			valNumSpeculativeTokens: "3",
		}},
	}
	files, err := filepath.Glob("testdata/startuplog/*.log")
	require.NoError(t, err)
	require.Len(t, files, len(real))
	for _, f := range files {
		// <pod>_<namespace>_<container>-<64 hex id>.log
		name := strings.TrimSuffix(filepath.Base(f), ".log")
		parts := strings.SplitN(name, "_", 3)
		require.Len(t, parts, 3, f)
		content, err := os.ReadFile(f)
		require.NoError(t, err)
		fixtures[parts[0]] = fixture{parts[1], parts[2][:len(parts[2])-65], content, real[parts[0]]}
	}
	root := t.TempDir()
	for pod, fx := range fixtures {
		t.Run(pod, func(t *testing.T) {
			require.NotNil(t, fx.want)
			placeLog(t, root, fx.namespace, pod, "uid-"+pod, fx.container, "0.log", fx.content)
			got, done := startupLog{root: root, limit: 1 << 20}.read(fx.namespace, pod, "uid-"+pod, fx.container, 0)
			assert.True(t, done)
			assert.Equal(t, fx.want, values(got))
			for _, v := range got {
				assert.Equal(t, srcStartupLine, v.source)
			}
		})
	}
}

func TestStartupLinePartialAndLimit(t *testing.T) {
	line := "Initializing a V1 LLM engine (v0.29.0) with config: model='org/m', speculative_config=None, " +
		"dtype=torch.bfloat16, max_seq_len=2048, tensor_parallel_size=1, quantization=None, enforce_eager=True, "
	partial := "2026-10-15T12:42:00.1Z stdout P (EngineCore pid=1) INFO " + line[:40] + "\n" +
		"2026-10-15T12:42:00.2Z stderr F a warning on the other stream\n" +
		"2026-10-15T12:42:00.3Z stdout P " + line[40:90] + "\n" +
		"2026-10-15T12:42:00.4Z stdout F " + line[90:] + "kv_cache_dtype=auto, served_model_name=m,\n"
	root := t.TempDir()
	placeLog(t, root, "ns", "p", "u", "vllm", "0.log", []byte(partial))
	got, done := startupLog{root: root, limit: 1 << 20}.read("ns", "p", "u", "vllm", 0)
	assert.True(t, done)
	assert.Equal(t, map[string]string{
		valVersion: "0.29.0", valGeneration: "V1", valModel: "org/m", valSpeculativeMethod: "none",
		valNumSpeculativeTokens: "none",
		valDtype:                "bfloat16", valMaxModelLen: "2048", valTensorParallelSize: "1", valQuantization: "none",
		valEnforceEager: "true", valKVCacheDtype: "auto", valServedModelName: "m",
	}, values(got))

	// the line is not there yet and the file is below the limit: try again later
	noise := "2026-10-15T12:42:00.1Z stdout F INFO loading weights\n"
	placeLog(t, root, "ns", "q", "u", "vllm", "0.log", []byte(noise))
	got, done = startupLog{root: root, limit: 1 << 20}.read("ns", "q", "u", "vllm", 0)
	assert.Nil(t, got)
	assert.False(t, done)
	// the line is still being written (no newline yet)
	placeLog(t, root, "ns", "q", "u", "vllm", "0.log", []byte(noise+"2026-10-15T12:42:00.2Z stdout F "+line))
	got, done = startupLog{root: root, limit: 1 << 20}.read("ns", "q", "u", "vllm", 0)
	assert.Nil(t, got)
	assert.False(t, done)

	// the line is after the limit: give up for this container start
	long := strings.Repeat(noise, 100) + "2026-10-15T12:42:00.2Z stdout F " + line + "\n"
	placeLog(t, root, "ns", "r", "u", "vllm", "0.log", []byte(long))
	got, done = startupLog{root: root, limit: int64(len(noise) * 50)}.read("ns", "r", "u", "vllm", 0)
	assert.Nil(t, got)
	assert.True(t, done)
	got, done = startupLog{root: root, limit: int64(len(long))}.read("ns", "r", "u", "vllm", 0)
	assert.True(t, done)
	assert.Equal(t, "0.29.0", got[valVersion].value)
}

func TestStartupLineSpeculativeForms(t *testing.T) {
	tests := []struct {
		name, config string
		want         map[string]string
	}{
		{"none", "speculative_config=None", map[string]string{valSpeculativeMethod: "none", valNumSpeculativeTokens: "none"}},
		{"v0.8.3+ with method first", "speculative_config=SpeculativeConfig(method='mtp', model='org/m-mtp', num_spec_tokens=4)",
			map[string]string{valSpeculativeMethod: "mtp", valNumSpeculativeTokens: "4"}},
		{"v0.8.3+ n-gram without a model", "speculative_config=SpeculativeConfig(method='ngram', model=None, num_spec_tokens=None)",
			map[string]string{valSpeculativeMethod: "ngram", valNumSpeculativeTokens: "none"}},
		{"method later in the list", "speculative_config=SpeculativeConfig(model='org/d', num_spec_tokens=2, method='eagle3')",
			map[string]string{valSpeculativeMethod: "eagle3", valNumSpeculativeTokens: "2"}},
		{"a quote in the model name", `speculative_config=SpeculativeConfig(method='draft_model', model="org/it's, (x)", num_spec_tokens=3)`,
			map[string]string{valSpeculativeMethod: "draft_model", valNumSpeculativeTokens: "3"}},
		{"method None with a model: the name rule", "speculative_config=SpeculativeConfig(method=None, model='yuhuili/EAGLE-LLaMA3-Instruct-8B', num_spec_tokens=5)",
			map[string]string{valSpeculativeMethod: "eagle", valNumSpeculativeTokens: "5"}},
		{"up to v0.8.2: n-gram", "speculative_config=SpeculativeConfig(draft_model='[ngram]', num_spec_tokens=5)",
			map[string]string{valSpeculativeMethod: "ngram", valNumSpeculativeTokens: "5"}},
		{"up to v0.8.2: a medusa head", "speculative_config=SpeculativeConfig(draft_model='abhigoyal/vllm-medusa-llama-68m-random', num_spec_tokens=3)",
			map[string]string{valSpeculativeMethod: "medusa", valNumSpeculativeTokens: "3"}},
		{"up to v0.8.2: a draft model", "speculative_config=SpeculativeConfig(draft_model='facebook/opt-125m', num_spec_tokens=5)",
			map[string]string{valSpeculativeMethod: "draft_model", valNumSpeculativeTokens: "5"}},
		{"not a value", "speculative_config=SpeculativeConfig(method='a b', num_spec_tokens=x)", map[string]string{}},
		{"no speculative_config", "tokenizer='m'", map[string]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line := "Initializing a V1 LLM engine (v0.29.0) with config: model='org/m', " + tt.config + ", tokenizer='org/m', "
			got := values(parseStartupLine([]byte(line)))
			delete(got, valVersion)
			delete(got, valGeneration)
			delete(got, valModel)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestStartupLineServedModelName(t *testing.T) {
	for config, want := range map[string]string{
		", served_model_name=llama-70b, ":     "llama-70b",
		", served_model_name=['a', 'b'], ":    "a",
		`, served_model_name=["a's"], `:       "a's",
		", served_model_name=[], ":            "",
		", served_model_name=None, ":          "none",
		", served_model_name=org/m:latest, ":  "org/m:latest",
		", served_model_name=last-field\n":    "",
		", tokenizer='m', seed=0, pooler=x, ": "",
	} {
		got := parseStartupLine([]byte("Initializing a V1 LLM engine (v0.29.0) with config: model='m'" + config))
		assert.Equal(t, want, got[valServedModelName].value, config)
	}
}

func TestStartupLineFileChoice(t *testing.T) {
	root := t.TempDir()
	line := func(v string) []byte {
		return []byte("2026-10-15T12:42:00.1Z stdout F Initializing a V1 LLM engine (v" + v + ") with config: model='m', \n")
	}
	placeLog(t, root, "ns", "p", "u", "vllm", "2.log", line("0.29.0"))
	old := placeLog(t, root, "ns", "p", "u", "vllm", "1.log", line("0.28.0"))
	require.NoError(t, os.Chtimes(old, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)))
	read := func(restarts int32) string {
		got, _ := startupLog{root: root, limit: 1 << 20}.read("ns", "p", "u", "vllm", restarts)
		return got[valVersion].value
	}
	assert.Equal(t, "0.28.0", read(1))
	// no <restartCount>.log: the newest *.log of the folder
	assert.Equal(t, "0.29.0", read(5))

	// no folder: try again later; a path part that is not a name: never
	got, done := startupLog{root: root, limit: 1 << 20}.read("ns", "other", "u", "vllm", 0)
	assert.Nil(t, got)
	assert.False(t, done)
	for _, bad := range []string{"..", "../ns", "a/b", ""} {
		got, done = startupLog{root: root, limit: 1 << 20}.read(bad, "p", "u", "vllm", 0)
		assert.Nil(t, got, bad)
		assert.True(t, done, bad)
	}
}
