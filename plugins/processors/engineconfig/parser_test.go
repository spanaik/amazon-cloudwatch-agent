// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package engineconfig

import (
	"bufio"
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// goldenFields are the values that the pod args give, as in the reference check.
var goldenFields = []string{
	valModel, valTensorParallelSize, valPipelineParallelSize, valDataParallelSize,
	valEnforceEager, valSpeculativeMethod, valNumSpeculativeTokens, valMaxNumSeqs,
	valMaxNumBatchedTokens, valEnableChunkedPrefill, valMaxModelLen, valDtype,
	valQuantization, valKVCacheDtype, valEnablePrefixCaching,
}

type goldenRow struct {
	Pos       string `json:"pos"`
	Permalink string `json:"permalink"`
	Image     string `json:"image"`
	Command   []string
	Args      []string
	Env       []struct {
		Name      string `json:"name"`
		Value     string `json:"value"`
		ValueFrom bool   `json:"valueFrom"`
	}
	EnvFrom  bool              `json:"envFrom"`
	Injected []string          `json:"injected"`
	Truth    map[string]string `json:"truth"`
	Python   map[string]string `json:"python"`
}

func loadGolden(t *testing.T) []goldenRow {
	f, err := os.Open("testdata/parser_golden.jsonl")
	require.NoError(t, err)
	defer f.Close()
	var rows []goldenRow
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var r goldenRow
		require.NoError(t, json.Unmarshal(sc.Bytes(), &r))
		rows = append(rows, r)
	}
	require.NoError(t, sc.Err())
	return rows
}

// statusString gives the reference representation: the value, "default" or "unknown".
func statusString(res argsResult, field string) string {
	if !res.found {
		return "unknown"
	}
	fv := res.fields[field]
	switch fv.Status {
	case statusSet:
		return fv.Value
	case statusDefault:
		return "default"
	}
	return "unknown"
}

func (r goldenRow) spec() containerSpec {
	c := containerSpec{Command: r.Command, Args: r.Args, EnvFrom: r.EnvFrom, Image: r.Image}
	for _, e := range r.Env {
		c.Env = append(c.Env, envVar{Name: e.Name, Value: e.Value, FromSource: e.ValueFrom})
	}
	return c
}

// TestParserGolden compares the parser with the hand-read values of 82 real pod
// specs (truth) and with the Python reference parser.
func TestParserGolden(t *testing.T) {
	rows := loadGolden(t)
	require.Len(t, rows, 82)
	agreeTruth, agreePython, total := 0, 0, 0
	var mismatches []string
	for _, r := range rows {
		res := parseContainerArgs(r.spec(), engineVLLM)
		injected := map[string]bool{}
		for _, f := range r.Injected {
			injected[f] = true
		}
		for _, f := range goldenFields {
			got := statusString(res, f)
			if injected[f] {
				got = "unknown" // set by the operator, not in the pod spec
			}
			total++
			if got == r.Truth[f] {
				agreeTruth++
			} else {
				mismatches = append(mismatches, r.Pos+" "+f+": got "+got+", truth "+r.Truth[f]+", python "+r.Python[f])
			}
			if got == r.Python[f] {
				agreePython++
			}
		}
	}
	sort.Strings(mismatches)
	for _, m := range mismatches {
		t.Log("mismatch:", m)
	}
	t.Logf("agreement with truth.jsonl: %d/%d (%.1f%%); with the Python parser: %d/%d (%.1f%%)",
		agreeTruth, total, 100*float64(agreeTruth)/float64(total),
		agreePython, total, 100*float64(agreePython)/float64(total))
	// The reference parser agrees on 97.3% over 16 values; the misses are pod specs
	// that do not hold the command line.
	assert.GreaterOrEqual(t, float64(agreeTruth)/float64(total), 0.97)
}

func parseArgs(t *testing.T, c containerSpec) map[string]string {
	t.Helper()
	res := parseContainerArgs(c, engineVLLM)
	out := map[string]string{}
	for _, f := range append(goldenFields, valGPUMemoryUtilization) {
		out[f] = statusString(res, f)
	}
	return out
}

func TestParserRules(t *testing.T) {
	tests := []struct {
		name string
		spec containerSpec
		want map[string]string
	}{
		{
			name: "vllm serve positional model, aliases, k=v, underscores, last wins",
			spec: containerSpec{
				Command: []string{"vllm", "serve", "meta-llama/Llama-3.1-8B"},
				Args: []string{"-tp", "4", "-pp=2", "--max_num_seqs", "64", "--max-num-seqs=128",
					"--dtype", "bfloat16", "-q", "awq", "--enforce-eager", "--no-enable-prefix-caching",
					"--enable-chunked-prefill", "--max-model-len", "32k", "--gpu-memory-utilization", "0.90",
					"--kv-cache-dtype", "fp8"},
			},
			want: map[string]string{
				valModel: "meta-llama/Llama-3.1-8B", valTensorParallelSize: "4", valPipelineParallelSize: "2",
				valDataParallelSize: "default", valMaxNumSeqs: "128", valDtype: "bfloat16", valQuantization: "awq",
				valEnforceEager: "true", valEnablePrefixCaching: "false", valEnableChunkedPrefill: "true",
				valMaxModelLen: "32000", valGPUMemoryUtilization: "0.9", valKVCacheDtype: "fp8",
				valSpeculativeMethod: "default", valNumSpeculativeTokens: "default", valMaxNumBatchedTokens: "default",
			},
		},
		{
			name: "vllm serve: the first --model becomes the model tag",
			spec: containerSpec{Command: []string{"vllm", "serve"}, Args: []string{"--model", "first/model", "--model=second/model"}},
			want: map[string]string{valModel: "first/model"},
		},
		{
			name: "api_server: the last --model wins",
			spec: containerSpec{
				Command: []string{"python3", "-m", "vllm.entrypoints.openai.api_server"},
				Args:    []string{"--model", "first/model", "--model=second/model", "--tensor-parallel-size=2"},
			},
			want: map[string]string{valModel: "second/model", valTensorParallelSize: "2", valMaxModelLen: "default"},
		},
		{
			name: "args only on the official image, with -dp and speculative JSON",
			spec: containerSpec{
				Image: "vllm/vllm-openai:v0.11.0",
				Args: []string{"Qwen/Qwen3-8B", "-dp", "2",
					"--speculative-config", `{"method": "eagle3", "num_speculative_tokens": 3, "model": "x/eagle"}`},
			},
			want: map[string]string{
				valModel: "Qwen/Qwen3-8B", valDataParallelSize: "2",
				valSpeculativeMethod: "eagle3", valNumSpeculativeTokens: "3",
			},
		},
		{
			name: "speculative dotted keys",
			spec: containerSpec{
				Image: "vllm/vllm-openai:latest",
				Args:  []string{"m", "--speculative-config.method", "ngram", "--speculative-config.num_speculative_tokens=5"},
			},
			want: map[string]string{valSpeculativeMethod: "ngram", valNumSpeculativeTokens: "5"},
		},
		{
			name: "older --speculative-model and --num-speculative-tokens",
			spec: containerSpec{
				Command: []string{"python", "-m", "vllm.entrypoints.openai.api_server"},
				Args:    []string{"--model", "m", "--speculative-model", "[ngram]", "--num-speculative-tokens", "4"},
			},
			want: map[string]string{valSpeculativeMethod: "ngram", valNumSpeculativeTokens: "4"},
		},
		{
			name: "sh -c script with variables, $(VAR) substitution and redirections",
			spec: containerSpec{
				Command: []string{"/bin/sh", "-c"},
				Args: []string{"set -e\n# start the server\nMODEL=${MODEL_ID:-org/default}\n" +
					"exec vllm serve \"$MODEL\" --tensor-parallel-size $(TP) \\\n  --max-model-len ${MAX_LEN} 2>&1 | tee /tmp/log"},
				Env: []envVar{{Name: "TP", Value: "8"}, {Name: "MAX_LEN", Value: "4096"}},
			},
			want: map[string]string{valModel: "org/default", valTensorParallelSize: "8", valMaxModelLen: "4096"},
		},
		{
			name: "an unknown expansion after a flag makes later-unset values unknown",
			spec: containerSpec{
				Command: []string{"bash", "-c", "vllm serve m --dtype half $EXTRA_ARGS"},
			},
			want: map[string]string{valModel: "m", valDtype: "unknown", valTensorParallelSize: "unknown"},
		},
		{
			name: "valueFrom env makes a flag value unknown",
			spec: containerSpec{
				Command: []string{"vllm", "serve", "m", "--max-num-seqs", "$(SEQS)"},
				Env:     []envVar{{Name: "SEQS", FromSource: true}},
			},
			want: map[string]string{valMaxNumSeqs: "unknown", valTensorParallelSize: "default"},
		},
		{
			name: "no vLLM command line: every value unknown",
			spec: containerSpec{Command: []string{"python", "serve.py"}, Image: "example/gpu-test-app:1"},
			want: map[string]string{valModel: "unknown", valTensorParallelSize: "unknown"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseArgs(t, tt.spec)
			for k, v := range tt.want {
				assert.Equal(t, v, got[k], k)
			}
		})
	}
}

func TestParserSGLang(t *testing.T) {
	tests := []struct {
		name  string
		spec  containerSpec
		want  map[string]string
		phase string
		group string
	}{
		{
			name: "sglang serve with a positional model, aliases, prefixes, store_true and human ints",
			spec: containerSpec{
				Command: []string{"sglang", "serve", "Qwen/Qwen3-8B"},
				Args: []string{"--tensor-parallel-size", "4", "--dp", "2", "--mem-fraction-static=0.88",
					"--context-length", "32Ki", "--max-total-tokens", "1.5M", "--disable-radix-cache",
					"--model-type", "llm", "--quantization", "fp8"},
			},
			want: map[string]string{
				sgModelPath: "Qwen/Qwen3-8B", "tp_size": "4", "dp_size": "2", "mem_fraction_static": "0.88",
				"context_length": "32768", "max_total_tokens": "1500000", "disable_radix_cache": "true",
				"quantization": "fp8", "pp_size": "default", "dtype": "default", "disaggregation_mode": "default",
			},
			phase: phaseUnified,
		},
		{
			name: "python3 -m sglang.launch_server in a shell, with the role flag",
			spec: containerSpec{
				Command: []string{"/bin/bash", "-c"},
				Args: []string{"set -e\nexec python3 -m sglang.launch_server --model-path $(MODEL) --tp-size 8 " +
					"--disaggregation-mode decode --disable-cuda-graph --lora-paths a=/x b=/y --host 0.0.0.0 " +
					"--nnodes 2 --node-rank 1"},
				Env: []envVar{{Name: "MODEL", Value: "deepseek-ai/DeepSeek-V3"}},
			},
			want: map[string]string{sgModelPath: "deepseek-ai/DeepSeek-V3", "tp_size": "8",
				"disaggregation_mode": "decode", "disable_cuda_graph": "true", "speculative_algorithm": "default"},
			phase: phaseDecode,
			group: groupWorker,
		},
		{
			name:  "role switch",
			spec:  containerSpec{Command: []string{"sglang", "serve", "m", "--disaggregation-mode", "prefill", "--enable-pd-role-switch"}},
			want:  map[string]string{sgEnablePDRoleSwitch: "true"},
			phase: phaseDynamic,
		},
		{
			name:  "the Dynamo SGLang launcher decides only by its flag",
			spec:  containerSpec{Command: []string{"python3", "-m", "dynamo.sglang", "--model-path", "m", "--tp-size", "2"}},
			want:  map[string]string{sgModelPath: "m", "tp_size": "2"},
			phase: "",
		},
		{
			name:  "the Dynamo SGLang launcher with a prefill mode",
			spec:  containerSpec{Command: []string{"python3", "-m", "dynamo.sglang", "--model-path", "m", "--disaggregation-mode", "prefill"}},
			want:  map[string]string{sgModelPath: "m"},
			phase: phasePrefill,
		},
		{
			name: "--config may set the values that the argv does not set",
			spec: containerSpec{Command: []string{"sglang", "serve", "--config", "/etc/sglang.yaml", "--tp-size", "2"}},
			want: map[string]string{"tp_size": "2", sgModelPath: "unknown", "dtype": "unknown"},
		},
		{
			name:  "null disaggregation mode is unified",
			spec:  containerSpec{Command: []string{"sglang", "serve", "--model-path", "m", "--disaggregation-mode", "null"}},
			want:  map[string]string{"disaggregation_mode": "null"},
			phase: phaseUnified,
		},
		{
			name: "a vLLM command line is not SGLang's",
			spec: containerSpec{Command: []string{"vllm", "serve", "m", "-tp", "2"}},
			want: map[string]string{sgModelPath: "unknown", "tp_size": "unknown"},
		},
		{
			name: "args only: an SGLang image does not start the server by itself",
			spec: containerSpec{Image: "lmsysorg/sglang:v0.5.22", Args: []string{"--model-path", "m"}},
			want: map[string]string{sgModelPath: "unknown"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := parseContainerArgs(tt.spec, engineSGLang)
			for k, v := range tt.want {
				assert.Equal(t, v, statusString(res, k), k)
			}
			assert.Equal(t, tt.phase, res.phase)
			assert.Equal(t, tt.group, res.group)
		})
	}
	// every SGLang value that the args give is an option destination
	dests := map[string]bool{}
	for _, o := range sglangOptions {
		dests[o.dest] = true
	}
	for _, name := range sglangValues {
		if name != sgVersion {
			assert.True(t, dests[name], name)
		}
	}
}

func TestFlagPhaseAndGroupVLLM(t *testing.T) {
	tests := []struct{ args, phase, group string }{
		{`--kv-transfer-config '{"kv_connector":"NixlConnector","kv_role":"kv_producer"}'`, phasePrefill, ""},
		{`--kv-transfer-config '{"kv_connector":"NixlConnector","kv_role":"kv_consumer"}'`, phaseDecode, ""},
		// llm-d runs kv_both on both sides, so it does not decide
		{`--kv-transfer-config '{"kv_connector":"NixlConnector","kv_role":"kv_both"}'`, "", ""},
		{`--kv-transfer-config.kv_role kv_consumer`, phaseDecode, ""},
		{`--kv-transfer-config $(KV_CONFIG)`, "", ""},
		{`--ec-transfer-config '{"ec_connector":"ECSharedStorageConnector","ec_role":"ec_producer"}'`, phaseEncode, ""},
		{`--ec-transfer-config '{"ec_role":"ec_consumer"}'`, phaseUnified, ""},
		{``, phaseUnified, ""},
		{`$EXTRA_ARGS`, "", ""},
		{`--headless`, phaseUnified, groupWorker},
		{`--nnodes 2 --node-rank 1`, phaseUnified, groupWorker},
		{`--nnodes 2`, phaseUnified, groupLeader},
		{`-n 2 -r 0`, phaseUnified, groupLeader},
		{`--nnodes 1`, phaseUnified, ""},
		{`--nnodes $(NODES)`, phaseUnified, ""},
	}
	for _, tt := range tests {
		spec := containerSpec{Command: []string{"sh", "-c", "vllm serve m " + tt.args},
			Env: []envVar{{Name: "KV_CONFIG", FromSource: true}, {Name: "NODES", FromSource: true}}}
		res := parseContainerArgs(spec, engineVLLM)
		require.True(t, res.found, tt.args)
		assert.Equal(t, tt.phase, res.phase, tt.args)
		assert.Equal(t, tt.group, res.group, tt.args)
	}

	// the Dynamo vLLM launcher: its own --disaggregation-mode, no transfer config
	for args, want := range map[string]string{
		"--model m --disaggregation-mode prefill": phasePrefill,
		"--model m --disaggregation-mode agg":     phaseUnified,
		"--model m --disaggregation-mode encode":  phaseEncode,
		"--model m":                               "",
	} {
		res := parseContainerArgs(containerSpec{Command: append([]string{"python3", "-m", "dynamo.vllm"}, strings.Fields(args)...)}, engineVLLM)
		require.True(t, res.found, args)
		assert.Equal(t, want, res.phase, args)
		assert.Equal(t, fieldValue{Status: statusSet, Value: "m"}, res.fields[valModel], args)
	}
}

func TestShlexSplit(t *testing.T) {
	got, err := shlexSplit(`a "b c" 'd e' f\ g "h\"i" x;y&&z`)
	require.NoError(t, err)
	assert.Equal(t, []string{"a", "b c", "d e", "f g", `h"i`, "x", ";", "y", "&&", "z"}, got)
	_, err = shlexSplit(`"open`)
	assert.Error(t, err)
}
