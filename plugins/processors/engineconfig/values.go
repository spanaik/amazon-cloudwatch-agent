// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package engineconfig

import (
	"regexp"
	"strconv"
	"strings"
)

// maxValueLength bounds every attribute value.
const maxValueLength = 256

// envMappable lists the values that an env contract (env_map) can give: the
// configured vLLM values.
var envMappable = func() map[string]bool {
	out := map[string]bool{}
	for _, name := range vllmConfigured {
		out[name] = true
	}
	return out
}()

// envConverters normalizes env contract values like the matching vLLM flag.
var envConverters = map[string]converter{
	valTensorParallelSize:   convInt,
	valPipelineParallelSize: convInt,
	valDataParallelSize:     convInt,
	valNumSpeculativeTokens: optional(convInt),
	valMaxNumSeqs:           optional(convInt),
	valMaxNumBatchedTokens:  optional(convHumanInt),
	valMaxModelLen:          convHumanIntOrAuto,
	valGPUMemoryUtilization: convFloat,
	valQuantization:         optional(convStr),
	valEnforceEager:         convBool,
	valEnableChunkedPrefill: convBool,
	valEnablePrefixCaching:  convBool,
}

func convBool(raw string) (string, bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return "true", false, true
	case "0", "false", "no", "off":
		return "false", false, true
	case "", "none":
		return "", true, true
	}
	return "", false, false
}

// pick returns the value of the first source that sets it, and the index of the
// source that decides. A source that may set it but whose value is not literal
// makes the value unknown (absent). Otherwise, if a source says "not set", the
// value is "default". If no source can tell, the value is absent.
func pick(sources ...fieldValue) (string, int, bool) {
	firstDefault, sawUnknown := -1, false
	for i, s := range sources {
		switch s.Status {
		case statusSet:
			return s.Value, i, true
		case statusUnknown:
			sawUnknown = true
		case statusDefault:
			if firstDefault < 0 {
				firstDefault = i
			}
		}
	}
	if sawUnknown || firstDefault < 0 {
		return "", 0, false
	}
	return defaultValue, firstDefault, true
}

func set(v string) fieldValue { return fieldValue{Status: statusSet, Value: v} }

// envContract is what the env_map contract says about each value of a container.
type envContract map[string]fieldValue

// readEnvContract reads only the literal env values of the mapped names. A mapped
// name that the pod does not set gives "default"; a valueFrom entry, or a name
// that an envFrom source may set, gives unknown.
func readEnvContract(envMap map[string]string, env []envVar, envFrom bool) envContract {
	if len(envMap) == 0 {
		return nil
	}
	literal := buildEnv(env, envFrom)
	out := envContract{}
	for name, value := range envMap {
		v, ok := literal[name]
		var fv fieldValue
		switch {
		case !ok && envFrom:
			fv = fieldValue{Status: statusUnknown}
		case !ok:
			fv = fieldValue{Status: statusDefault}
		case strings.Contains(v, unk):
			fv = fieldValue{Status: statusUnknown}
		default:
			fv = set(v)
			if conv, has := envConverters[value]; has {
				c, isNone, convOK := conv(v)
				switch {
				case !convOK:
					fv = fieldValue{Status: statusUnknown}
				case isNone:
					fv = fieldValue{Status: statusDefault}
				default:
					fv = set(c)
				}
			}
		}
		// Two env names can map to one value: a set value wins over default.
		if prev, dup := out[value]; dup && prev.Status == statusSet {
			continue
		}
		out[value] = fv
	}
	return out
}

// useV1Env reads VLLM_USE_V1: "0", "1", "" (not set) or "?" (not literal).
func useV1Env(env []envVar, envFrom bool) string {
	literal := buildEnv(env, envFrom)
	v, ok := literal["VLLM_USE_V1"]
	switch {
	case !ok && envFrom:
		return "?"
	case !ok:
		return ""
	case strings.Contains(v, unk):
		return "?"
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false":
		return "0"
	case "1", "true":
		return "1"
	}
	return "?"
}

var (
	versionRE     = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?[0-9A-Za-z.+\-]*$`)
	versionPartRE = regexp.MustCompile(`^([0-9]+)\.([0-9]+)(?:\.([0-9]+))?`)
	// imageTagRE takes the release out of an image tag: v0.5.22-cu130 gives 0.5.22
	imageTagRE = regexp.MustCompile(`^v?([0-9]+\.[0-9]+(?:\.[0-9]+)?(?:\.post[0-9]+)?)(?:$|[-_+])`)
)

// parseVersion returns the major and minor version.
func parseVersion(v string) (int, int, bool) {
	m := versionPartRE.FindStringSubmatch(v)
	if m == nil {
		return 0, 0, false
	}
	major, err1 := strconv.Atoi(m[1])
	minor, err2 := strconv.Atoi(m[2])
	return major, minor, err1 == nil && err2 == nil
}

// versionAtLeast reports whether version is at least want (major, minor, patch).
func versionAtLeast(version string, want [3]int) bool {
	m := versionPartRE.FindStringSubmatch(version)
	if m == nil {
		return false
	}
	for i, part := range m[1:] {
		n, err := strconv.Atoi(part)
		if err != nil {
			n = 0 // no patch part
		}
		if n != want[i] {
			return n > want[i]
		}
	}
	return true
}

// imageVersion gives the release in the tag of a container image, or "".
func imageVersion(image string) string {
	image, _, _ = strings.Cut(strings.TrimSpace(image), "@")
	i := strings.LastIndexByte(image, ':')
	if i < 0 || strings.Contains(image[i:], "/") {
		return ""
	}
	if m := imageTagRE.FindStringSubmatch(image[i+1:]); m != nil {
		return m[1]
	}
	return ""
}

// v0OnlyFamilies exist only in the V0 engine.
var v0OnlyFamilies = map[string]bool{
	"vllm:num_requests_swapped":   true,
	"vllm:cpu_cache_usage_perc":   true,
	"vllm:time_in_queue_requests": true,
}

// deriveGeneration gives "V0", "V1" or "" (unknown) from the version. From v0.11.0
// the V0 engine is gone. From v0.8.0 V1 is the default unless VLLM_USE_V1=0.
// Before v0.8.0 V0 is the default unless VLLM_USE_V1=1.
func deriveGeneration(version, useV1 string) string {
	if major, minor, ok := parseVersion(version); ok {
		switch {
		case major > 0 || minor >= 11:
			return "V1"
		case minor >= 8:
			if useV1 == "0" {
				return "V0"
			}
			if useV1 != "?" {
				return "V1"
			}
		default:
			if useV1 == "1" {
				return "V1"
			}
			if useV1 != "?" {
				return "V0"
			}
		}
	}
	return ""
}

// effectiveValue normalizes an effective value that the engine prints the Python
// way, like the converters do: True and False become true and false, and None
// becomes none.
func effectiveValue(raw string) (string, bool) {
	switch raw {
	case "True":
		return "true", true
	case "False":
		return "false", true
	case "None":
		return noneValue, true
	}
	return raw, validValue(raw)
}

func validValue(v string) bool {
	if v == "" || len(v) > maxValueLength {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] == 0x7f {
			return false
		}
	}
	return true
}
