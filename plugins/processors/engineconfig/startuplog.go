// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package engineconfig

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// startupLog reads the vLLM startup line ("Initializing a V1 LLM engine (v...)
// with config: ...") from the container log that the kubelet writes, as a
// fallback for the effective values. It keeps only the values that the patterns
// match; no other log content is kept, logged or emitted.
type startupLog struct {
	root  string
	limit int64
}

var (
	// a path part must be one Kubernetes name or UID, never a path
	logPathPartRE = regexp.MustCompile(`^[A-Za-z0-9][-A-Za-z0-9_.]*$`)

	startupMarkers = [][]byte{
		[]byte("Initializing a V1 LLM engine (v"),
		[]byte("Initializing a V0 LLM engine (v"),
		[]byte("Initializing an LLM engine (v"),
	}

	// startupPatterns are Go ports of the patterns of the earlier chart, with a
	// list form for served_model_name. speculativeValues reads speculative_config.
	startupPatterns = []struct {
		name  string
		re    *regexp.Regexp
		fixed string // the value when the pattern has no group
	}{
		{valVersion, regexp.MustCompile(`Initializing an? (?:V[01] )?LLM engine \(v(?P<v>[^)]+)\) with config: `), ""},
		{valModel, regexp.MustCompile(`with config: model='(?P<v>[^']*)'`), ""},
		{valGeneration, regexp.MustCompile(`Initializing a (?P<v>V[01]) LLM engine`), ""},
		// "an LLM engine" (up to v0.7.0) is the V0 engine
		{valGeneration, regexp.MustCompile(`Initializing an LLM engine \(v`), "V0"},
		// vLLM prints one name: ModelConfig keeps the first name of a list
		// (get_served_model_name). A list form, should one appear, keeps its
		// first name by the same rule.
		{valServedModelName, regexp.MustCompile(`, served_model_name=(?:\[\s*(?:'([^']*)'|"([^"]*)")[^\]]*\]|([^,\[][^,]*)),`), ""},
		{valDtype, regexp.MustCompile(`, dtype=(?:torch\.)?(?P<v>[A-Za-z0-9_]+),`), ""},
		{valMaxModelLen, regexp.MustCompile(`, max_seq_len=(?P<v>\d+),`), ""},
		{valTensorParallelSize, regexp.MustCompile(`, tensor_parallel_size=(?P<v>\d+),`), ""},
		{valPipelineParallelSize, regexp.MustCompile(`, pipeline_parallel_size=(?P<v>\d+),`), ""},
		{valDataParallelSize, regexp.MustCompile(`, data_parallel_size=(?P<v>\d+),`), ""},
		{valQuantization, regexp.MustCompile(`, quantization=(?P<v>[A-Za-z0-9_.\-]+),`), ""},
		{valEnforceEager, regexp.MustCompile(`, enforce_eager=(?P<v>True|False),`), ""},
		{valKVCacheDtype, regexp.MustCompile(`, kv_cache_dtype=(?P<v>[A-Za-z0-9_]+),`), ""},
		{valEnablePrefixCaching, regexp.MustCompile(`, enable_prefix_caching=(?P<v>True|False|None),`), ""},
		// v0.6.x and v0.8.x print chunked_prefill_enabled; v0.6.3 has no comma after
		// it (llm_engine.py L251-252)
		{valEnableChunkedPrefill, regexp.MustCompile(`, (?:enable_chunked_prefill|chunked_prefill_enabled)=(?P<v>True|False)[, ]`), ""},
	}

	// speculative_config=None, or the repr of SpeculativeConfig: up to v0.8.2
	// SpeculativeConfig(draft_model='...', num_spec_tokens=N), from v0.8.3
	// SpeculativeConfig(method='...', model='...', num_spec_tokens=N)
	specConfigRE = regexp.MustCompile(`speculative_config=(None|SpeculativeConfig\(((?:[^()'"]|'[^']*'|"[^"]*")*)\))`)
	specFieldRE  = regexp.MustCompile(`^(\w+)=('[^']*'|"[^"]*"|[^,]*)(?:,\s*|$)`)
	specMethodRE = regexp.MustCompile(`^[A-Za-z0-9_.\-]+$`)
	specTokensRE = regexp.MustCompile(`^(\d+|None)$`)
)

// read returns the effective values of the startup line of one container start.
// done=false means that the line is not there yet and the file is below the
// limit, so a later read may find it.
func (l startupLog) read(namespace, pod, uid, container string, restarts int32) (map[string]sourced, bool) {
	for _, part := range []string{namespace, pod, uid, container} {
		if !logPathPartRE.MatchString(part) {
			return nil, true
		}
	}
	dir := filepath.Join(l.root, namespace+"_"+pod+"_"+uid, container)
	f, err := os.Open(filepath.Join(dir, strconv.Itoa(int(restarts))+".log"))
	if errors.Is(err, os.ErrNotExist) {
		f, err = openNewestLog(dir)
	}
	if err != nil {
		return nil, false
	}
	defer f.Close()
	line, done := findStartupLine(f, l.limit)
	if line == nil {
		return nil, done
	}
	return parseStartupLine(line), true
}

func openNewestLog(dir string) (*os.File, error) {
	matches, err := filepath.Glob(filepath.Join(dir, "*.log"))
	if err != nil {
		return nil, err
	}
	newest := ""
	var newestMod int64
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil && fi.Mode().IsRegular() && (newest == "" || fi.ModTime().UnixNano() > newestMod) {
			newest, newestMod = m, fi.ModTime().UnixNano()
		}
	}
	if newest == "" {
		return nil, os.ErrNotExist
	}
	return os.Open(newest)
}

// findStartupLine reads CRI log lines (<timestamp> <stream> <P|F> <message>) from
// the start, up to limit bytes, joins the P (partial) parts of each stream, and
// returns the message of the first full line with a startup marker. done is true
// when the line was found or the limit was reached.
func findStartupLine(r io.Reader, limit int64) ([]byte, bool) {
	br := bufio.NewReader(io.LimitReader(r, limit))
	var read int64
	partial := map[string][]byte{}
	for {
		raw, err := br.ReadBytes('\n')
		read += int64(len(raw))
		if err != nil {
			// a line without its newline may still be written, or is cut by the limit
			return nil, read >= limit
		}
		fields := bytes.SplitN(bytes.TrimRight(raw, "\r\n"), []byte(" "), 4)
		if len(fields) != 4 {
			continue
		}
		stream, tag, msg := string(fields[1]), fields[2], fields[3]
		if bytes.Equal(tag, []byte("P")) {
			partial[stream] = append(partial[stream], msg...)
			continue
		}
		if p := partial[stream]; p != nil {
			msg = append(p, msg...)
			delete(partial, stream)
		}
		for _, marker := range startupMarkers {
			if bytes.Contains(msg, marker) {
				return msg, true
			}
		}
	}
}

func parseStartupLine(line []byte) map[string]sourced {
	out := map[string]sourced{}
	s := string(line)
	for _, p := range startupPatterns {
		// the value is the first group that matched
		var raw string
		if m := p.re.FindStringSubmatch(s); m != nil {
			raw = p.fixed
			for _, g := range m[1:] {
				if g != "" {
					raw = g
					break
				}
			}
		}
		v, ok := effectiveValue(strings.TrimSpace(raw))
		if p.name == valVersion {
			ok = ok && versionRE.MatchString(v)
		}
		if ok {
			out[p.name] = sourced{v, srcStartupLine}
		}
	}
	for k, v := range speculativeValues(s) {
		out[k] = sourced{v, srcStartupLine}
	}
	return out
}

// speculativeValues reads the speculative method and the number of speculative
// tokens from speculative_config. None gives none for both.
func speculativeValues(line string) map[string]string {
	m := specConfigRE.FindStringSubmatch(line)
	if m == nil {
		return nil
	}
	if m[1] == "None" {
		return map[string]string{valSpeculativeMethod: noneValue, valNumSpeculativeTokens: noneValue}
	}
	fields := map[string]string{}
	for body := m[2]; body != ""; {
		f := specFieldRE.FindStringSubmatch(body)
		if f == nil {
			break
		}
		fields[f[1]] = strings.Trim(f[2], `'"`)
		body = body[len(f[0]):]
	}
	out := map[string]string{}
	method := fields["method"]
	if method == "" || method == "None" {
		// Up to v0.8.2 the line has no method: vLLM picks the proposer from the
		// draft model's config (model_type medusa, mlp_speculator or eagle;
		// "[ngram]" for n-gram), which the line does not show. legacySpecMethod
		// applies the name rule of the configured --speculative-model value, with
		// draft_model when the name tells nothing.
		method = ""
		for _, model := range []string{fields["draft_model"], fields["model"]} {
			if model != "" && model != "None" {
				method = legacySpecMethod(model)
				break
			}
		}
	}
	if specMethodRE.MatchString(method) {
		out[valSpeculativeMethod] = method
	}
	if n := fields["num_spec_tokens"]; specTokensRE.MatchString(n) {
		out[valNumSpeculativeTokens], _ = effectiveValue(n)
	}
	return out
}
