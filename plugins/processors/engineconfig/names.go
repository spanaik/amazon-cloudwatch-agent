// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package engineconfig

import (
	"strconv"
	"strings"
)

// This file holds every attribute key that the processor writes, the Info metric
// name, the value names, the role vocabularies, the label tables and their
// precedence. The names and rules follow the research note "Engine Record:
// Roles, Serving Objects, Owners, and Names" (section 7). All keys but
// gen_ai.request.model are proposals; change them here and nowhere else.

const (
	engineVLLM   = "vllm"
	engineSGLang = "sglang"

	// identity of the engine container
	keyEngineName       = "inference.engine.name"
	keyEngineVersion    = "inference.engine.version"
	keyEngineGeneration = "inference.engine.generation" // vLLM V0 or V1, not set for SGLang
	keyEngineRole       = "inference.engine.role"       // the phase
	keyEngineGroupRole  = "inference.engine.group.role" // only for a group above one pod

	// workload, with the chart's names (k8sattributes and
	// transform/cw_k8s_ci_v0_set_workload set them on the resource later in the
	// pipeline), so they are written only with emit_workload
	keyWorkloadType = "k8s.workload.type"
	keyWorkloadName = "k8s.workload.name"

	// serving object: <api group>/<Kind> and the object name
	keyServingKind = "inference.serving.kind"
	keyServingName = "inference.serving.name"

	// the served model name, the name that clients send
	keyRequestModel = "gen_ai.request.model"

	// configured values: <engine>.config.<name>; effective values: <engine>.effective.<name>
	configuredInfix = ".config."
	effectiveInfix  = ".effective."

	// infoMetricName is the gauge of the info shape: one series per engine, value 1.
	infoMetricName = "inference.engine.info"

	// defaultValue is the configured value of a flag that the customer did not set.
	defaultValue = "default"
	// noneValue is an effective value that the engine reports as None or null.
	noneValue = "none"
)

func configuredKey(engine, name string) string { return engine + configuredInfix + name }

func effectiveKey(engine, name string) string { return engine + effectiveInfix + name }

// requestModelValues are the value names that give gen_ai.request.model, in
// order: the served model name, else the model. For each name the effective
// value comes first, then the configured one when the customer set it.
var requestModelValues = map[string][]string{
	engineVLLM:   {valServedModelName, valModel},
	engineSGLang: {valServedModelName, sgModelPath},
}

// vLLM value names: the configured and effective value names, and the internal
// names of the identity and phase values that the sources give.
const (
	valVersion    = "version"     // identity
	valGeneration = "generation"  // identity
	valEngineType = "engine_type" // the SGLang metric label, for the phase only

	valModel                = "model"
	valServedModelName      = "served_model_name"
	valTensorParallelSize   = "tensor_parallel_size"
	valPipelineParallelSize = "pipeline_parallel_size"
	valDataParallelSize     = "data_parallel_size"
	valDtype                = "dtype"
	valQuantization         = "quantization"
	valEnforceEager         = "enforce_eager"
	valSpeculativeMethod    = "speculative_method"
	valNumSpeculativeTokens = "num_speculative_tokens"
	valMaxNumSeqs           = "max_num_seqs"
	valMaxNumBatchedTokens  = "max_num_batched_tokens"
	valEnableChunkedPrefill = "enable_chunked_prefill"
	valEnablePrefixCaching  = "enable_prefix_caching"
	valGPUMemoryUtilization = "gpu_memory_utilization"
	valKVCacheDtype         = "kv_cache_dtype"
	valMaxModelLen          = "max_model_len"
	valNumGPUBlocks         = "num_gpu_blocks"
)

// internalValues are source values that the record keeps as identity or phase,
// never as effective values.
var internalValues = map[string]bool{valVersion: true, valGeneration: true, valEngineType: true}

// SGLang value names are the SGLang server argument names. sglangValues is the
// allowlist of /server_info keys; every one but the version is also read from
// the pod args.
const (
	sgVersion            = "version"
	sgModelPath          = "model_path"
	sgDisaggMode         = "disaggregation_mode"
	sgEnablePDRoleSwitch = "enable_pd_role_switch"
)

var sglangValues = []string{
	sgVersion, sgModelPath, valServedModelName, "tp_size", "pp_size", "dp_size", "dtype", "quantization",
	"kv_cache_dtype", "mem_fraction_static", "chunked_prefill_size", "max_prefill_tokens",
	"max_running_requests", "max_total_tokens", "context_length", "schedule_policy", "disable_radix_cache",
	"disable_cuda_graph", "speculative_algorithm", "speculative_num_draft_tokens", "speculative_num_steps",
	"attention_backend", sgDisaggMode, sgEnablePDRoleSwitch,
}

// sglangDenied tells the /server_info fields that can hold secrets (note section
// 5.4). They are dropped at the top level and in every internal_states entry,
// before the allowlist applies.
func sglangDenied(key string) bool {
	switch key {
	case "api_key", "admin_api_key", "ssl_keyfile_password", "ssl_keyfile", "ssl_certfile", "ssl_ca_certs",
		"launch_command", "modelexpress_config", "kv_events_config", "sidecar_args", "tool_server",
		"otlp_traces_endpoint", "lora_paths", "json_model_override_args", "env_vars":
		return true
	}
	return strings.HasPrefix(key, "watermark_") || strings.HasPrefix(key, "remote_instance_weight_loader_") ||
		strings.HasSuffix(key, "_extra_config")
}

// Phases (inference.engine.role). unified is written when no source decides.
const (
	phaseUnified = "unified"
	phasePrefill = "prefill"
	phaseDecode  = "decode"
	phaseEncode  = "encode"
	phaseDynamic = "dynamic"
)

// Phase values of the engine sources.
var (
	// vLLM --kv-transfer-config kv_role; kv_both does not decide
	vllmKVPhases = map[string]string{"kv_producer": phasePrefill, "kv_consumer": phaseDecode}
	// vLLM --ec-transfer-config ec_role
	vllmECPhases = map[string]string{"ec_producer": phaseEncode}
	// SGLang disaggregation_mode (/server_info or --disaggregation-mode), and the
	// --disaggregation-mode of the Dynamo launchers (agg and pd are aggregated)
	disaggModePhases = map[string]string{"null": phaseUnified, "agg": phaseUnified, "pd": phaseUnified,
		"prefill": phasePrefill, "decode": phaseDecode, "encode": phaseEncode}
	// the SGLang engine_type metric label, which follows the mode from v0.5.7
	engineTypePhases = map[string]string{phaseUnified: phaseUnified, phasePrefill: phasePrefill,
		phaseDecode: phaseDecode, phaseDynamic: phaseDynamic}
	engineTypeSince = [3]int{0, 5, 7}
)

// Pod labels and annotations of the phase and group rules.
const (
	dynamoComponentType    = "nvidia.com/dynamo-component-type"
	dynamoSubComponentType = "nvidia.com/dynamo-sub-component-type"
	omeComponent           = "component"
	omeServingLabel        = "ome.io/inferenceservice"
	lwsNameLabel           = "leaderworkerset.sigs.k8s.io/name"
	lwsWorkerIndexLabel    = "leaderworkerset.sigs.k8s.io/worker-index"
	lwsSizeAnnotation      = "leaderworkerset.sigs.k8s.io/size"
	rbgComponentLabel      = "rbg.workloads.x-k8s.io/component-name"
	rayNodeTypeLabel       = "ray.io/node-type"
	podTemplateHashLabel   = "pod-template-hash"
)

// phaseLabels give the phase from platform labels, in order: the first label
// that decides wins. A value that a rule does not know does not decide.
var phaseLabels = []struct {
	key   string
	phase func(value string, labels map[string]string) string
}{
	{dynamoComponentType, func(v string, labels map[string]string) string {
		switch v {
		case phasePrefill, phaseDecode:
			return v
		case "worker": // older graphs: the sub-component type, else aggregated
			if s := labels[dynamoSubComponentType]; s == phasePrefill || s == phaseDecode {
				return s
			}
			return phaseUnified
		}
		return ""
	}},
	{"llm-d.ai/role", func(v string, _ map[string]string) string {
		return map[string]string{phasePrefill: phasePrefill, phaseDecode: phaseDecode, "prefill-decode": phaseUnified,
			"both": phaseUnified, phaseEncode: phaseEncode, "encode-prefill": phasePrefill,
			"encode-prefill-decode": phaseUnified}[v]
	}},
	// free text: only an exact vocabulary word counts
	{"disaggregatedset.x-k8s.io/role", exactPhase},
	{"rbg.workloads.x-k8s.io/role-name", exactPhase},
	// OME: a decoder decodes; the engine does not decide, because the pod alone
	// does not show whether a decoder exists. The generic key counts only on an
	// OME pod.
	{omeComponent, func(v string, labels map[string]string) string {
		if v == "decoder" && labels[omeServingLabel] != "" {
			return phaseDecode
		}
		return ""
	}},
}

func exactPhase(v string, _ map[string]string) string {
	if v == phasePrefill || v == phaseDecode || v == phaseEncode {
		return v
	}
	return ""
}

// Phase and group sources, for the debug log of value sources.
const (
	srcNoDecision  = "no source decides"
	srcEngineFlags = "engine flags"
	srcEngineType  = "sglang engine_type label"
)

// resolvePhase applies the phase precedence of the note (section 1.4): the
// engine's effective state (SGLang /server_info), the engine flags, the SGLang
// engine_type label (from v0.5.7), then the platform labels; unified when none
// decides. A label that disagrees with an engine source is named in the source,
// for the debug log.
func resolvePhase(effective map[string]sourced, flagPhase, version string, labels map[string]string) (string, string) {
	labelPhase, labelKey := "", ""
	for _, l := range phaseLabels {
		if v, ok := labels[l.key]; ok {
			if p := l.phase(v, labels); p != "" {
				labelPhase, labelKey = p, l.key
				break
			}
		}
	}
	decided := func(phase, source string) (string, string) {
		if labelPhase != "" && labelPhase != phase {
			source += " (label " + labelKey + " says " + labelPhase + ")"
		}
		return phase, source
	}
	if v := effective[sgEnablePDRoleSwitch]; v.value == "true" {
		return decided(phaseDynamic, v.source)
	}
	if v := effective[sgDisaggMode]; disaggModePhases[v.value] != "" {
		return decided(disaggModePhases[v.value], v.source)
	}
	if flagPhase != "" {
		return decided(flagPhase, srcEngineFlags)
	}
	if v := effective[valEngineType]; engineTypePhases[v.value] != "" && versionAtLeast(version, engineTypeSince) {
		return decided(engineTypePhases[v.value], srcEngineType)
	}
	if labelPhase != "" {
		return labelPhase, "label " + labelKey
	}
	return phaseUnified, srcNoDecision
}

// Group roles (inference.engine.group.role), only for a group above one pod.
const (
	groupLeader = "leader"
	groupWorker = "worker"
)

// KServe multi-node container names (KServe constants L507, L514). A single-node
// predictor also runs kserve-container, so it is the leader only with a literal
// RAY_NODE_COUNT above 1, which KServe sets for multi-node (predictor.go L652).
const (
	kserveContainer       = "kserve-container"
	kserveWorkerContainer = "worker-container"
	kserveNodeCountEnv    = "RAY_NODE_COUNT"
)

// resolveGroup applies the group precedence of the note (section 1.4): the
// engine flags (--headless, --node-rank, --nnodes), the LWS worker index with a
// size above 1, the RBG component name, the KubeRay node type (vLLM only), then
// the KServe container rule.
func resolveGroup(engine, flagGroup, kserveGroup string, labels, annotations map[string]string) (string, string) {
	if flagGroup != "" {
		return flagGroup, srcEngineFlags
	}
	if idx, err := strconv.Atoi(labels[lwsWorkerIndexLabel]); err == nil && idx >= 0 {
		if size, err := strconv.Atoi(annotations[lwsSizeAnnotation]); err == nil && size > 1 {
			if idx == 0 {
				return groupLeader, "label " + lwsWorkerIndexLabel
			}
			return groupWorker, "label " + lwsWorkerIndexLabel
		}
	}
	if v := labels[rbgComponentLabel]; v == groupLeader || v == groupWorker {
		return v, "label " + rbgComponentLabel
	}
	if engine == engineVLLM {
		switch labels[rayNodeTypeLabel] {
		case "head":
			return groupLeader, "label " + rayNodeTypeLabel
		case "worker":
			return groupWorker, "label " + rayNodeTypeLabel
		}
	}
	if kserveGroup != "" {
		return kserveGroup, "KServe container"
	}
	return "", ""
}

// servingRules give the serving object from pod labels (note section 2), first
// match wins. value, if set, is the label value that the rule needs; nameKey,
// if set, is the label that holds the name, else the label value is the name.
var servingRules = []struct{ key, value, kind, nameKey string }{
	{"serving.kserve.io/inferenceservice", "", "serving.kserve.io/InferenceService", ""},
	{"app.kubernetes.io/part-of", "llminferenceservice", "serving.kserve.io/LLMInferenceService", "app.kubernetes.io/name"},
	{omeServingLabel, "", "ome.io/InferenceService", ""},
	{"nvidia.com/dynamo-graph-deployment-name", "", "nvidia.com/DynamoGraphDeployment", ""},
	{"rbg.workloads.x-k8s.io/group-name", "", "workloads.x-k8s.io/RoleBasedGroup", ""},
	{"rolebasedgroup.workloads.x-k8s.io/name", "", "workloads.x-k8s.io/RoleBasedGroup", ""}, // legacy RBG label
	{"disaggregatedset.x-k8s.io/name", "", "disaggregatedset.x-k8s.io/DisaggregatedSet", ""},
	{"ray.io/cluster", "", "ray.io/RayCluster", ""},
	{lwsNameLabel, "", "leaderworkerset.x-k8s.io/LeaderWorkerSet", ""},
}

// servingOf gives the serving object of a pod from its labels.
func servingOf(labels map[string]string) owner {
	for _, r := range servingRules {
		v := labels[r.key]
		if v == "" || (r.value != "" && v != r.value) {
			continue
		}
		name := v
		if r.nameKey != "" {
			name = labels[r.nameKey]
		}
		if name != "" {
			return owner{Kind: r.kind, Name: name}
		}
	}
	return owner{}
}

// workloadTypes are the chart's k8s.workload.type values. emit_workload writes
// only these, so a LeaderWorkerSet (which the serving object names) or another
// owner kind gives no workload attributes.
var workloadTypes = map[string]bool{
	"Deployment": true, "StatefulSet": true, "DaemonSet": true, "Job": true, "CronJob": true, "ReplicaSet": true,
}

// Workload kinds of the top-owner rule.
const (
	kindDeployment      = "Deployment"
	kindReplicaSet      = "ReplicaSet"
	kindLeaderWorkerSet = "LeaderWorkerSet"
)

// watchedLabels and watchedAnnotations are the pod labels and annotations that
// the pod watcher keeps.
func watchedLabels() map[string]bool {
	out := map[string]bool{podTemplateHashLabel: true, dynamoSubComponentType: true, lwsWorkerIndexLabel: true,
		rbgComponentLabel: true, rayNodeTypeLabel: true}
	for _, l := range phaseLabels {
		out[l.key] = true
	}
	for _, r := range servingRules {
		out[r.key] = true
		if r.nameKey != "" {
			out[r.nameKey] = true
		}
	}
	return out
}

var watchedAnnotations = map[string]bool{lwsSizeAnnotation: true}

// owner is a Kubernetes object by kind and name.
type owner struct {
	Kind string `json:"k"`
	Name string `json:"n"`
}

// topOwner resolves the workload of a pod without more informers: a ReplicaSet
// named <deployment>-<pod-template-hash> gives the Deployment; StatefulSet,
// DaemonSet, Job (a CronJob owner is not visible) and other kinds stay as they
// are. The LeaderWorkerSet name label wins over the StatefulSet owner.
func topOwner(owners []owner, labels map[string]string) owner {
	if name := labels[lwsNameLabel]; name != "" {
		return owner{Kind: kindLeaderWorkerSet, Name: name}
	}
	if len(owners) == 0 {
		return owner{}
	}
	o := owners[0]
	if o.Kind == kindReplicaSet {
		if hash := labels[podTemplateHashLabel]; hash != "" {
			if name, ok := strings.CutSuffix(o.Name, "-"+hash); ok && name != "" {
				return owner{Kind: kindDeployment, Name: name}
			}
		}
	}
	return o
}
