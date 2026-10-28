// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package engineconfig

import (
	"sort"
	"strings"
)

// record is the full configuration record of one engine container: identity,
// phase and group role, workload, serving object, configured values (from the
// pod spec) and effective values (from the engine). The source of each value is
// kept for the debug log only.
type record struct {
	engine     string
	version    string
	generation string
	role       string // the phase
	groupRole  string
	workload   owner // kept in memory; written only with emit_workload
	serving    owner
	model      string
	configured map[string]string
	effective  map[string]string
	sources    map[string]string // attribute key -> source
}

type attribute struct{ key, value string }

// buildRecord combines what the pod spec and the engine say about a container.
// effective holds the merged effective values, with the internal values (version,
// generation, engine_type) that the sources give.
func buildRecord(eng engine, pod podView, c containerInfo, effective map[string]sourced) record {
	r := record{
		engine:     eng.name(),
		configured: map[string]string{},
		effective:  map[string]string{},
		sources:    map[string]string{},
	}
	version, gen := eng.identity(effective, c)
	r.version, r.generation = version.value, gen.value
	r.sources[keyEngineVersion], r.sources[keyEngineGeneration] = version.source, gen.source

	r.role, r.sources[keyEngineRole] = resolvePhase(effective, c.Phase, r.version, pod.labels)
	r.groupRole, r.sources[keyEngineGroupRole] = resolveGroup(r.engine, c.Group, c.KServe, pod.labels, pod.annotations)
	r.workload = topOwner(pod.owners, pod.labels)
	r.sources[keyWorkloadType] = srcOwner
	r.serving = servingOf(pod.labels)

	for name, v := range eng.configured(c) {
		r.configured[name] = v.value
		r.sources[configuredKey(r.engine, name)] = v.source
	}
	for name, v := range effective {
		if !internalValues[name] {
			r.effective[name] = v.value
			r.sources[effectiveKey(r.engine, name)] = v.source
		}
	}
	// the served model name, else the model: effective first, then configured
	for _, name := range requestModelValues[r.engine] {
		if v := r.effective[name]; v != "" && v != noneValue {
			r.model = v
			break
		}
		if v := r.configured[name]; v != "" && v != defaultValue {
			r.model = v
			break
		}
	}
	return r
}

// identity gives the identity attributes: the engine name, version, generation,
// phase, group role and the served model name. Empty values are left out.
func (r record) identity() []attribute {
	var out []attribute
	add := func(k, v string) {
		if v != "" {
			out = append(out, attribute{k, v})
		}
	}
	add(keyEngineName, r.engine)
	add(keyEngineVersion, r.version)
	add(keyEngineGeneration, r.generation)
	add(keyEngineRole, r.role)
	add(keyEngineGroupRole, r.groupRole)
	add(keyRequestModel, r.model)
	return out
}

// attributes gives the full record as attributes, in a fixed order: identity,
// serving object, workload (with emitWorkload), configured and effective values.
func (r record) attributes(emitWorkload bool) []attribute {
	out := r.identity()
	if r.serving.Kind != "" {
		out = append(out, attribute{keyServingKind, r.serving.Kind}, attribute{keyServingName, r.serving.Name})
	}
	if emitWorkload && workloadTypes[r.workload.Kind] {
		out = append(out, attribute{keyWorkloadType, r.workload.Kind}, attribute{keyWorkloadName, r.workload.Name})
	}
	for _, name := range sortedKeys(r.configured) {
		out = append(out, attribute{configuredKey(r.engine, name), r.configured[name]})
	}
	for _, name := range sortedKeys(r.effective) {
		out = append(out, attribute{effectiveKey(r.engine, name), r.effective[name]})
	}
	return out
}

// describe gives the record with the source of each value, for the debug log.
func (r record) describe() string {
	var b strings.Builder
	b.WriteString("workload=" + r.workload.Kind + "/" + r.workload.Name)
	for _, a := range r.attributes(false) {
		b.WriteString(" " + a.key + "=" + a.value)
		if src := r.sources[a.key]; src != "" {
			b.WriteString(" (" + src + ")")
		}
	}
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
