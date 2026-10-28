// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package engineconfig

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestKeys(t *testing.T) {
	assert.Equal(t, "vllm.config.tensor_parallel_size", configuredKey(engineVLLM, valTensorParallelSize))
	assert.Equal(t, "vllm.effective.max_model_len", effectiveKey(engineVLLM, valMaxModelLen))
	assert.Equal(t, "sglang.config.tp_size", configuredKey(engineSGLang, "tp_size"))
	assert.Equal(t, "sglang.effective.mem_fraction_static", effectiveKey(engineSGLang, "mem_fraction_static"))
	assert.Equal(t, "inference.engine.info", infoMetricName)

	r := record{engine: engineVLLM, version: "0.29.0", generation: "V1", role: phaseDecode, groupRole: groupLeader,
		workload: owner{Kind: "Deployment", Name: "vllm-demo"},
		serving:  owner{Kind: "serving.kserve.io/InferenceService", Name: "llama"},
		model:    "llama-70b", configured: map[string]string{valDtype: "bfloat16", valModel: "org/m"},
		effective: map[string]string{valMaxModelLen: "2048"}}
	assert.Equal(t, []attribute{
		{"inference.engine.name", "vllm"}, {"inference.engine.version", "0.29.0"},
		{"inference.engine.generation", "V1"}, {"inference.engine.role", "decode"},
		{"inference.engine.group.role", "leader"}, {"gen_ai.request.model", "llama-70b"},
		{"inference.serving.kind", "serving.kserve.io/InferenceService"}, {"inference.serving.name", "llama"},
		{"vllm.config.dtype", "bfloat16"}, {"vllm.config.model", "org/m"}, {"vllm.effective.max_model_len", "2048"},
	}, r.attributes(false))
	withWorkload := r.attributes(true)
	assert.Contains(t, withWorkload, attribute{"k8s.workload.type", "Deployment"})
	assert.Contains(t, withWorkload, attribute{"k8s.workload.name", "vllm-demo"})

	// empty values and an empty serving object are left out, and a workload kind
	// that is not one of the chart's types is not written
	r = record{engine: engineSGLang, role: phaseUnified, workload: owner{Kind: kindLeaderWorkerSet, Name: "llama"}}
	assert.Equal(t, []attribute{{keyEngineName, "sglang"}, {keyEngineRole, "unified"}}, r.attributes(true))
}

func TestResolvePhase(t *testing.T) {
	info := func(kv ...string) map[string]sourced {
		out := map[string]sourced{}
		for i := 0; i < len(kv); i += 2 {
			out[kv[i]] = sourced{kv[i+1], srcServerInfo}
		}
		return out
	}
	engineType := func(v string) map[string]sourced { return map[string]sourced{valEngineType: {v, srcEngineType}} }
	tests := []struct {
		name      string
		effective map[string]sourced
		flag      string
		version   string
		labels    map[string]string
		want      string
		wantFrom  string
	}{
		{"nothing decides: unified", nil, "", "", nil, phaseUnified, srcNoDecision},
		{"server_info mode", info(sgDisaggMode, "prefill"), phaseDecode, "", nil, phasePrefill, srcServerInfo},
		{"server_info null mode", info(sgDisaggMode, "null"), "", "", nil, phaseUnified, srcServerInfo},
		{"role switch wins over the mode", info(sgEnablePDRoleSwitch, "true", sgDisaggMode, "prefill"), "", "", nil,
			phaseDynamic, srcServerInfo},
		{"flag", nil, phasePrefill, "", nil, phasePrefill, srcEngineFlags},
		{"flag beats a label, which is named", nil, phaseUnified, "", map[string]string{"llm-d.ai/role": "decode"},
			phaseUnified, srcEngineFlags + " (label llm-d.ai/role says decode)"},
		{"engine_type from v0.5.7", engineType("prefill"), "", "0.5.7", nil, phasePrefill, srcEngineType},
		{"engine_type before v0.5.7 does not decide", engineType("unified"), "", "0.5.6",
			map[string]string{"llm-d.ai/role": "prefill"}, phasePrefill, "label llm-d.ai/role"},
		{"engine_type with no version does not decide", engineType("decode"), "", "", nil, phaseUnified, srcNoDecision},
		{"engine_type of a later minor", engineType("dynamic"), "", "0.6.0", nil, phaseDynamic, srcEngineType},
		{"Dynamo component type first", nil, "", "", map[string]string{dynamoComponentType: "decode", "llm-d.ai/role": "prefill"},
			phaseDecode, "label " + dynamoComponentType},
		{"Dynamo worker with a sub-component type", nil, "", "", map[string]string{dynamoComponentType: "worker",
			dynamoSubComponentType: "prefill"}, phasePrefill, "label " + dynamoComponentType},
		{"Dynamo worker alone is aggregated", nil, "", "", map[string]string{dynamoComponentType: "worker"},
			phaseUnified, "label " + dynamoComponentType},
		{"Dynamo frontend does not decide", nil, "", "", map[string]string{dynamoComponentType: "frontend"},
			phaseUnified, srcNoDecision},
		{"llm-d encode-prefill", nil, "", "", map[string]string{"llm-d.ai/role": "encode-prefill"}, phasePrefill, "label llm-d.ai/role"},
		{"llm-d both", nil, "", "", map[string]string{"llm-d.ai/role": "both"}, phaseUnified, "label llm-d.ai/role"},
		{"DisaggregatedSet exact word", nil, "", "", map[string]string{"disaggregatedset.x-k8s.io/role": "decode"},
			phaseDecode, "label disaggregatedset.x-k8s.io/role"},
		{"RBG free text does not decide", nil, "", "", map[string]string{"rbg.workloads.x-k8s.io/role-name": "prefill-pool"},
			phaseUnified, srcNoDecision},
		{"OME decoder", nil, "", "", map[string]string{omeComponent: "decoder", omeServingLabel: "llama"},
			phaseDecode, "label component"},
		{"a component label off OME", nil, "", "", map[string]string{omeComponent: "decoder"}, phaseUnified, srcNoDecision},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			phase, from := resolvePhase(tt.effective, tt.flag, tt.version, tt.labels)
			assert.Equal(t, tt.want, phase)
			assert.Equal(t, tt.wantFrom, from)
		})
	}
}

func TestResolveGroup(t *testing.T) {
	size := map[string]string{lwsSizeAnnotation: "2"}
	tests := []struct {
		name               string
		engine, flag, ks   string
		labels, annotation map[string]string
		want               string
	}{
		{"none", engineVLLM, "", "", nil, nil, ""},
		{"flag wins", engineVLLM, groupWorker, groupLeader, map[string]string{lwsWorkerIndexLabel: "0"}, size, groupWorker},
		{"LWS leader", engineSGLang, "", "", map[string]string{lwsWorkerIndexLabel: "0"}, size, groupLeader},
		{"LWS worker", engineSGLang, "", "", map[string]string{lwsWorkerIndexLabel: "1"}, size, groupWorker},
		{"LWS of size 1 is no group", engineVLLM, "", "", map[string]string{lwsWorkerIndexLabel: "0"},
			map[string]string{lwsSizeAnnotation: "1"}, ""},
		{"LWS without a size", engineVLLM, "", "", map[string]string{lwsWorkerIndexLabel: "0"}, nil, ""},
		{"RBG component", engineSGLang, "", "", map[string]string{rbgComponentLabel: "worker"}, nil, groupWorker},
		{"Ray head for vLLM", engineVLLM, "", "", map[string]string{rayNodeTypeLabel: "head"}, nil, groupLeader},
		{"Ray head is not for SGLang", engineSGLang, "", "", map[string]string{rayNodeTypeLabel: "head"}, nil, ""},
		{"KServe container rule last", engineVLLM, "", groupLeader, nil, nil, groupLeader},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := resolveGroup(tt.engine, tt.flag, tt.ks, tt.labels, tt.annotation)
			assert.Equal(t, tt.want, got)
		})
	}
	assert.Equal(t, groupWorker, kserveGroup(kserveWorkerContainer, nil))
	assert.Equal(t, groupLeader, kserveGroup(kserveContainer, map[string]string{kserveNodeCountEnv: "2"}))
	assert.Equal(t, "", kserveGroup(kserveContainer, nil), "a single-node predictor")
	assert.Equal(t, "", kserveGroup(kserveContainer, map[string]string{kserveNodeCountEnv: unk}))
}

func TestTopOwner(t *testing.T) {
	hash := map[string]string{podTemplateHashLabel: "5dbcb77dd"}
	tests := []struct {
		name   string
		owners []owner
		labels map[string]string
		want   owner
	}{
		{"bare pod", nil, nil, owner{}},
		{"ReplicaSet of a Deployment", []owner{{"ReplicaSet", "vllm-demo-5dbcb77dd"}}, hash, owner{"Deployment", "vllm-demo"}},
		{"ReplicaSet without the hash label", []owner{{"ReplicaSet", "vllm-demo-5dbcb77dd"}}, nil, owner{"ReplicaSet", "vllm-demo-5dbcb77dd"}},
		{"ReplicaSet name without the hash", []owner{{"ReplicaSet", "standalone"}}, hash, owner{"ReplicaSet", "standalone"}},
		{"ReplicaSet named only the hash", []owner{{"ReplicaSet", "-5dbcb77dd"}}, hash, owner{"ReplicaSet", "-5dbcb77dd"}},
		{"StatefulSet", []owner{{"StatefulSet", "vllm"}}, nil, owner{"StatefulSet", "vllm"}},
		{"DaemonSet", []owner{{"DaemonSet", "vllm"}}, nil, owner{"DaemonSet", "vllm"}},
		{"Job, maybe of a CronJob", []owner{{"Job", "bench-28911520"}}, nil, owner{"Job", "bench-28911520"}},
		{"other kinds as they are", []owner{{"RayCluster", "sgl"}}, nil, owner{"RayCluster", "sgl"}},
		{"LeaderWorkerSet name wins over the StatefulSet", []owner{{"StatefulSet", "llama-0"}},
			map[string]string{lwsNameLabel: "llama"}, owner{"LeaderWorkerSet", "llama"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, topOwner(tt.owners, tt.labels))
		})
	}
}

func TestServingOf(t *testing.T) {
	tests := []struct {
		labels map[string]string
		want   owner
	}{
		{nil, owner{}},
		{map[string]string{"serving.kserve.io/inferenceservice": "llama"}, owner{"serving.kserve.io/InferenceService", "llama"}},
		{map[string]string{"app.kubernetes.io/part-of": "llminferenceservice", "app.kubernetes.io/name": "qwen"},
			owner{"serving.kserve.io/LLMInferenceService", "qwen"}},
		{map[string]string{"app.kubernetes.io/part-of": "something-else", "app.kubernetes.io/name": "qwen"}, owner{}},
		{map[string]string{omeServingLabel: "ds"}, owner{"ome.io/InferenceService", "ds"}},
		{map[string]string{"nvidia.com/dynamo-graph-deployment-name": "g"}, owner{"nvidia.com/DynamoGraphDeployment", "g"}},
		{map[string]string{"rbg.workloads.x-k8s.io/group-name": "r"}, owner{"workloads.x-k8s.io/RoleBasedGroup", "r"}},
		{map[string]string{"rolebasedgroup.workloads.x-k8s.io/name": "old"}, owner{"workloads.x-k8s.io/RoleBasedGroup", "old"}},
		{map[string]string{"disaggregatedset.x-k8s.io/name": "pd"}, owner{"disaggregatedset.x-k8s.io/DisaggregatedSet", "pd"}},
		{map[string]string{"ray.io/cluster": "sgl"}, owner{"ray.io/RayCluster", "sgl"}},
		{map[string]string{lwsNameLabel: "llama"}, owner{"leaderworkerset.x-k8s.io/LeaderWorkerSet", "llama"}},
		// the higher rank wins: a KServe multi-node service runs on LWS
		{map[string]string{lwsNameLabel: "b", "serving.kserve.io/inferenceservice": "a"},
			owner{"serving.kserve.io/InferenceService", "a"}},
		{map[string]string{"ray.io/cluster": "c", "disaggregatedset.x-k8s.io/name": "d"},
			owner{"disaggregatedset.x-k8s.io/DisaggregatedSet", "d"}},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, servingOf(tt.labels), "%v", tt.labels)
	}
}

func TestWatchedLabels(t *testing.T) {
	got := watchedLabels()
	for _, k := range []string{
		"pod-template-hash", "nvidia.com/dynamo-component-type", "nvidia.com/dynamo-sub-component-type",
		"llm-d.ai/role", "disaggregatedset.x-k8s.io/role", "rbg.workloads.x-k8s.io/role-name", "component",
		"leaderworkerset.sigs.k8s.io/worker-index", "rbg.workloads.x-k8s.io/component-name", "ray.io/node-type",
		"serving.kserve.io/inferenceservice", "app.kubernetes.io/part-of", "app.kubernetes.io/name",
		"ome.io/inferenceservice", "nvidia.com/dynamo-graph-deployment-name", "rbg.workloads.x-k8s.io/group-name",
		"rolebasedgroup.workloads.x-k8s.io/name", "disaggregatedset.x-k8s.io/name", "ray.io/cluster",
		"leaderworkerset.sigs.k8s.io/name",
	} {
		assert.True(t, got[k], k)
	}
	assert.Len(t, got, 20)
	assert.Equal(t, map[string]bool{"leaderworkerset.sigs.k8s.io/size": true}, watchedAnnotations)
}

func TestVersionAtLeastAndImageVersion(t *testing.T) {
	assert.True(t, versionAtLeast("0.5.7", engineTypeSince))
	assert.True(t, versionAtLeast("0.5.22", engineTypeSince))
	assert.True(t, versionAtLeast("0.6", engineTypeSince))
	assert.True(t, versionAtLeast("1.0.0rc1", engineTypeSince))
	assert.False(t, versionAtLeast("0.5.6.post2", engineTypeSince))
	assert.False(t, versionAtLeast("0.5", engineTypeSince))
	assert.False(t, versionAtLeast("", engineTypeSince))

	for image, want := range map[string]string{
		"lmsysorg/sglang:v0.5.22":                     "0.5.22",
		"lmsysorg/sglang:v0.5.22-cu130":               "0.5.22",
		"lmsysorg/sglang:v0.4.6.post1-cu124":          "0.4.6.post1",
		"lmsysorg/sglang:v0.5.22@sha256:e3a4f06001a2": "0.5.22",
		"registry:5000/sglang:0.5.21":                 "0.5.21",
		"registry:5000/sglang":                        "",
		"lmsysorg/sglang:latest":                      "",
		"lmsysorg/sglang@sha256:e3a4f06001a2":         "",
		"lmsysorg/sglang:dev-x86":                     "",
	} {
		assert.Equal(t, want, imageVersion(image), image)
	}
}
