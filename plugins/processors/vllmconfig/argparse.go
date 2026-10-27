// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package vllmconfig

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// This file emulates vLLM's FlexibleArgumentParser and Python argparse on the argv
// after `vllm serve` (or the api_server module). It is a port of the core of the
// reference parser (parse_vllm.py, flag rules of vLLM v0.32.0).

type nargsKind int

const (
	nargsZero nargsKind = iota
	nargsOne
	nargsPlus
	nargsOptional
)

type option struct {
	dest  string
	nargs nargsKind
	neg   bool // a --no-<flag> option: sets the destination to false
}

type fieldStatus int

const (
	statusUnavailable fieldStatus = iota // the source cannot tell
	statusDefault                        // the customer did not set it
	statusSet                            // an explicit value
	statusUnknown                        // the source may set it, but the value is not literal
)

type fieldValue struct {
	Status fieldStatus `json:"s"`
	Value  string      `json:"v,omitempty"`
}

// argValue is the raw value of one parsed option: a string, a bool or a list.
type argValue struct {
	s     string
	b     *bool
	list  []string
	isLst bool
}

func (v argValue) unknown() bool {
	if v.isLst {
		for _, s := range v.list {
			if strings.Contains(s, unk) {
				return true
			}
		}
		return false
	}
	return v.b == nil && strings.Contains(v.s, unk)
}

type assignment struct {
	val argValue
	pos int
}

type parseResult struct {
	assigned    map[string]assignment
	modelTag    *assignment
	opaquePos   []int
	configFile  bool
	errorsCount int
}

var (
	modelFlagRE   = regexp.MustCompile(`^(?s)--model(=.+|$)`)
	negativeNumRE = regexp.MustCompile(`^-\d+$|^-\d*\.\d+$`)
)

// matchOption ports argparse._parse_optional and _get_option_tuples (Python 3.12).
// It returns the kind ("" for a positional, "unknown", "ambiguous" or "opt"), the
// option string and the explicit value after "=", if any.
func matchOption(tok string) (string, string, *string) {
	if tok == "" || tok[0] != '-' || tok == "-" {
		return "", "", nil
	}
	if _, ok := vllmOptions[tok]; ok {
		return "opt", tok, nil
	}
	if len(tok) == 1 {
		return "", "", nil
	}
	if k, v, ok := strings.Cut(tok, "="); ok {
		if _, has := vllmOptions[k]; has {
			return "opt", k, &v
		}
	}
	type cand struct {
		opt      string
		explicit *string
	}
	var cands []cand
	if strings.HasPrefix(tok, "--") {
		prefix := tok
		var exp *string
		if k, v, ok := strings.Cut(tok, "="); ok {
			prefix = k
			exp = &v
		}
		for o := range vllmOptions {
			if strings.HasPrefix(o, prefix) {
				cands = append(cands, cand{o, exp})
			}
		}
	} else {
		short, shortExplicit := tok[:2], tok[2:]
		for o := range vllmOptions {
			if o == short {
				se := shortExplicit
				cands = append(cands, cand{o, &se})
			} else if strings.HasPrefix(o, tok) {
				cands = append(cands, cand{o, nil})
			}
		}
	}
	if len(cands) > 1 {
		return "ambiguous", tok, nil
	}
	if len(cands) == 1 {
		return "opt", cands[0].opt, cands[0].explicit
	}
	if negativeNumRE.MatchString(tok) {
		return "", "", nil
	}
	if strings.Contains(tok, " ") {
		return "", "", nil
	}
	return "unknown", tok, nil
}

// normFlag: FlexibleArgumentParser treats '_' as '-' in the key, up to the first '.'.
func normFlag(arg string) string {
	key, value, hasValue := strings.Cut(arg, "=")
	head, tail, hasDot := strings.Cut(key[2:], ".")
	out := "--" + strings.ReplaceAll(head, "_", "-")
	if hasDot {
		out += "." + tail
	}
	if hasValue {
		out += "=" + value
	}
	return out
}

func humanReadableInt(value string) (int64, bool) {
	value = strings.TrimSpace(value)
	if m := humanIntRE.FindStringSubmatch(value); m != nil {
		number, suffix := m[1], m[2]
		switch suffix {
		case "k", "m", "g", "t":
			f, err := strconv.ParseFloat(number, 64)
			if err != nil {
				return 0, false
			}
			mult := map[string]float64{"k": 1e3, "m": 1e6, "g": 1e9, "t": 1e12}[suffix]
			return int64(f * mult), true
		default:
			n, err := strconv.ParseInt(number, 10, 64)
			if err != nil {
				return 0, false
			}
			shift := map[string]uint{"K": 10, "M": 20, "G": 30, "T": 40}[suffix]
			return n << shift, true
		}
	}
	n, err := strconv.ParseInt(value, 10, 64)
	return n, err == nil
}

var humanIntRE = regexp.MustCompile(`^(\d+(?:\.\d+)?)([kKmMgGtT])$`)

func jsonOrRaw(w word) any {
	if w.opaque {
		return unk
	}
	var v any
	if err := json.Unmarshal([]byte(w.s), &v); err == nil {
		return v
	}
	if n, ok := humanReadableInt(w.s); ok {
		return n
	}
	return w.s
}

func mergeDict(orig, upd map[string]any) {
	for k, v := range upd {
		if vd, ok := v.(map[string]any); ok {
			if od, ok2 := orig[k].(map[string]any); ok2 {
				mergeDict(od, vd)
				continue
			}
		}
		if vl, ok := v.([]any); ok {
			if ol, ok2 := orig[k].([]any); ok2 {
				orig[k] = append(ol, vl...)
				continue
			}
		}
		orig[k] = v
	}
}

// vllmParse emulates vLLM + argparse on the argv after `vllm serve` (kind
// "vllm serve") or the api_server module.
func vllmParse(kind string, tokens []word) parseResult {
	res := parseResult{assigned: map[string]assignment{}}
	args := append([]word(nil), tokens...)

	// FlexibleArgumentParser.parse_args, serve-specific part: the first --model
	// becomes the positional model tag.
	if kind == "vllm serve" {
		idx := -1
		for i, a := range args {
			if !a.opaque && modelFlagRE.MatchString(a.s) {
				idx = i
				break
			}
		}
		if idx >= 0 {
			var tag word
			rest := idx + 1
			if args[idx].s == "--model" {
				if idx+1 < len(args) {
					tag = args[idx+1]
					rest = idx + 2
				} else {
					res.errorsCount++
					tag = strWord(unk)
				}
			} else {
				tag = strWord(args[idx].s[len("--model="):])
			}
			na := []word{tag}
			na = append(na, args[:idx]...)
			na = append(na, args[rest:]...)
			args = na
		}
	}
	for _, a := range args {
		if !a.opaque && (a.s == "--config" || strings.HasPrefix(a.s, "--config=")) {
			res.configFile = true
			break
		}
	}
	if res.configFile {
		var na []word
		skip := false
		for _, a := range args {
			if skip {
				skip = false
				continue
			}
			if !a.opaque && a.s == "--config" {
				skip = true
				continue
			}
			if !a.opaque && strings.HasPrefix(a.s, "--config=") {
				continue
			}
			na = append(na, a)
		}
		args = na
	}

	// underscore/dash normalization and -O
	var processed []word
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a.opaque:
			processed = append(processed, a)
		case strings.HasPrefix(a.s, "--"):
			processed = append(processed, strWord(normFlag(a.s)))
		case strings.HasPrefix(a.s, "-O") && a.s != "-O":
			level := a.s[2:]
			if a.s[2] == '=' {
				level = a.s[3:]
			}
			processed = append(processed, strWord("--optimization-level"), strWord(level))
		case a.s == "-O" && i+1 < len(args) && !args[i+1].opaque && strings.Contains("0123", args[i+1].s) && len(args[i+1].s) == 1:
			processed = append(processed, strWord("--optimization-level"))
		default:
			processed = append(processed, a)
		}
	}

	// dotted keys -> JSON dict arguments appended at the end
	deleted := map[int]bool{}
	dictArgs := map[string]map[string]any{}
	var dictOrder []string
	for i, a := range processed {
		if deleted[i] || a.opaque {
			continue
		}
		if strings.HasPrefix(a.s, "--") && !strings.Contains(a.s, ".") {
			continue
		}
		if !strings.HasPrefix(a.s, "-") || !strings.Contains(a.s, ".") {
			continue
		}
		var key string
		var valueW word
		if k, v, ok := strings.Cut(a.s, "="); ok {
			if !strings.Contains(k, ".") {
				continue
			}
			key, valueW = k, strWord(v)
		} else {
			if i+1 >= len(processed) {
				res.errorsCount++
				deleted[i] = true
				continue
			}
			key, valueW = a.s, processed[i+1]
			deleted[i+1] = true
		}
		var value any
		if strings.HasSuffix(key, "+") {
			key = key[:len(key)-1]
			if valueW.opaque {
				value = unk
			} else {
				var lst []any
				for _, s := range strings.Split(valueW.s, ",") {
					lst = append(lst, s)
				}
				value = lst
			}
		} else {
			value = jsonOrRaw(valueW)
		}
		keys := strings.Split(key, ".")
		nested := value
		for j := len(keys) - 1; j >= 1; j-- {
			nested = map[string]any{keys[j]: nested}
		}
		nd, ok := nested.(map[string]any)
		if !ok {
			nd = map[string]any{}
		}
		if _, has := dictArgs[keys[0]]; !has {
			dictArgs[keys[0]] = map[string]any{}
			dictOrder = append(dictOrder, keys[0])
		}
		mergeDict(dictArgs[keys[0]], nd)
		deleted[i] = true
	}
	if len(deleted) > 0 || len(dictOrder) > 0 {
		var np []word
		for i, a := range processed {
			if !deleted[i] {
				np = append(np, a)
			}
		}
		processed = np
		for _, k := range dictOrder {
			b, _ := json.Marshal(dictArgs[k])
			processed = append(processed, strWord(k), strWord(string(b)))
		}
	}

	// argparse
	classes := make([]byte, len(processed))
	for i, a := range processed {
		switch {
		case a.opaque:
			classes[i] = 'X'
		case strings.Contains(a.s, unk) && strings.ReplaceAll(a.s, unk, "") == "":
			classes[i] = 'U' // wholly unknown single item
		default:
			if k, _, _ := matchOption(a.s); k != "" {
				classes[i] = 'O'
			} else {
				classes[i] = 'A'
			}
		}
	}
	if kind == "vllm serve" && len(classes) > 0 && classes[0] == 'U' {
		classes[0] = 'A' // the model tag slot
	}
	isValue := func(j int) bool { return j < len(classes) && (classes[j] == 'A' || classes[j] == 'U') }
	trueV, falseV := true, false
	var positionalModel *assignment
	n := len(processed)
	for i := 0; i < n; {
		a, cl := processed[i], classes[i]
		switch cl {
		case 'X', 'U':
			res.opaquePos = append(res.opaquePos, i)
			i++
			continue
		case 'A':
			if strings.Contains(a.s, " ") && strings.HasPrefix(a.s, "--") {
				// "--tensor-parallel-size 2" as one argv item: argparse makes it a
				// positional and vLLM exits. Recover the intent.
				res.errorsCount++
				flag, val, _ := strings.Cut(a.s, " ")
				if k, opt, _ := matchOption(flag); k == "opt" {
					o := vllmOptions[opt]
					res.assigned[o.dest] = assignment{val: valueFor(o, strings.TrimSpace(val)), pos: i}
				}
				i++
				continue
			}
			if positionalModel == nil && (kind == "vllm serve" || kind == "api_server") {
				positionalModel = &assignment{val: argValue{s: a.s}, pos: i}
			} else {
				res.errorsCount++
			}
			i++
			continue
		}
		k, opt, explicit := matchOption(a.s)
		if k == "unknown" || k == "ambiguous" {
			res.errorsCount++
			if !strings.Contains(a.s, "=") && isValue(i+1) {
				i += 2
			} else {
				i++
			}
			continue
		}
		o := vllmOptions[opt]
		switch o.nargs {
		case nargsZero:
			if explicit != nil && *explicit != "" {
				res.errorsCount++
			}
			if o.neg {
				res.assigned[o.dest] = assignment{val: argValue{b: &falseV}, pos: i}
			} else {
				res.assigned[o.dest] = assignment{val: argValue{b: &trueV}, pos: i}
			}
			i++
		case nargsOptional:
			switch {
			case explicit != nil:
				res.assigned[o.dest] = assignment{val: argValue{s: *explicit}, pos: i}
				i++
			case isValue(i + 1):
				res.assigned[o.dest] = assignment{val: argValue{s: processed[i+1].s}, pos: i}
				i += 2
			default:
				res.assigned[o.dest] = assignment{val: argValue{b: &trueV}, pos: i}
				i++
			}
		case nargsPlus:
			var vals []string
			j := i + 1
			if explicit != nil {
				vals = []string{*explicit}
			} else {
				for j < n && (classes[j] == 'A' || classes[j] == 'U' || classes[j] == 'X') {
					if classes[j] == 'X' {
						vals = append(vals, unk)
					} else {
						vals = append(vals, processed[j].s)
					}
					j++
				}
			}
			if len(vals) == 0 {
				res.errorsCount++
				vals = []string{unk}
			}
			res.assigned[o.dest] = assignment{val: argValue{list: vals, isLst: true}, pos: i}
			if explicit == nil {
				i = j
			} else {
				i++
			}
		default: // nargsOne
			switch {
			case explicit != nil:
				res.assigned[o.dest] = assignment{val: argValue{s: *explicit}, pos: i}
				i++
			case isValue(i + 1):
				res.assigned[o.dest] = assignment{val: argValue{s: processed[i+1].s}, pos: i}
				i += 2
			case i+1 < n && classes[i+1] == 'X':
				res.assigned[o.dest] = assignment{val: argValue{s: unk}, pos: i}
				res.opaquePos = append(res.opaquePos, i+1)
				i += 2
			default:
				res.errorsCount++
				res.assigned[o.dest] = assignment{val: argValue{s: unk}, pos: i}
				i++
			}
		}
	}
	if kind == "vllm serve" && positionalModel != nil {
		res.modelTag = positionalModel
	}
	return res
}

func valueFor(o option, raw string) argValue {
	switch o.nargs {
	case nargsZero:
		b := !o.neg
		return argValue{b: &b}
	case nargsPlus:
		return argValue{list: strings.Fields(raw), isLst: true}
	}
	return argValue{s: raw}
}

// converters turn a raw flag string into the attribute value. ok=false means vLLM
// could not convert it; isNone means the value is Python None (not set).
type converter func(raw string) (val string, isNone bool, ok bool)

func convInt(raw string) (string, bool, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return "", false, false
	}
	return strconv.FormatInt(n, 10), false, true
}

func optional(c converter) converter {
	return func(raw string) (string, bool, bool) {
		if raw == "" || raw == "None" {
			return "", true, true
		}
		return c(raw)
	}
}

func convHumanInt(raw string) (string, bool, bool) {
	n, ok := humanReadableInt(raw)
	if !ok {
		return "", false, false
	}
	return strconv.FormatInt(n, 10), false, true
}

func convHumanIntOrAuto(raw string) (string, bool, bool) {
	v := strings.TrimSpace(raw)
	if v == "-1" || strings.EqualFold(v, "auto") {
		return "auto", false, true
	}
	return convHumanInt(v)
}

func convStr(raw string) (string, bool, bool) { return raw, false, true }

func convFloat(raw string) (string, bool, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return "", false, false
	}
	return pyFloat(f), false, true
}

// pyFloat formats a float the way Python's str() does for common values, so that
// pod args and vllm:cache_config_info labels give the same string.
func pyFloat(f float64) string {
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	a := math.Abs(f)
	if a != 0 && (a < 1e-4 || a >= 1e16) {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// argFields lists the values that the pod args give, with their converters.
var argFields = []struct {
	name string
	dest string
	conv converter
}{
	{valTensorParallelSize, "tensor_parallel_size", convInt},
	{valPipelineParallelSize, "pipeline_parallel_size", convInt},
	{valDataParallelSize, "data_parallel_size", convInt},
	{valEnforceEager, "enforce_eager", nil},
	{valMaxNumSeqs, "max_num_seqs", optional(convInt)},
	{valMaxNumBatchedTokens, "max_num_batched_tokens", optional(convHumanInt)},
	{valEnableChunkedPrefill, "enable_chunked_prefill", nil},
	{valMaxModelLen, "max_model_len", convHumanIntOrAuto},
	{valDtype, "dtype", convStr},
	{valQuantization, "quantization", optional(convStr)},
	{valKVCacheDtype, "kv_cache_dtype", convStr},
	{valEnablePrefixCaching, "enable_prefix_caching", nil},
	{valGPUMemoryUtilization, "gpu_memory_utilization", convFloat},
}

// fieldValues maps a parse result to a status and value per field.
func fieldValues(kind string, pr parseResult) map[string]fieldValue {
	res := map[string]fieldValue{}
	lastOpaque := -1
	for _, p := range pr.opaquePos {
		if p > lastOpaque {
			lastOpaque = p
		}
	}
	statusOf := func(pos int, has bool) fieldStatus {
		if !has {
			if lastOpaque >= 0 || pr.configFile {
				return statusUnknown
			}
			return statusDefault
		}
		if lastOpaque > pos {
			return statusUnknown
		}
		return statusSet
	}
	put := func(field, dest string, conv converter) {
		as, ok := pr.assigned[dest]
		if !ok {
			res[field] = fieldValue{Status: statusOf(0, false)}
			return
		}
		st := statusOf(as.pos, true)
		if st != statusSet {
			res[field] = fieldValue{Status: st}
			return
		}
		if as.val.unknown() {
			res[field] = fieldValue{Status: statusUnknown}
			return
		}
		switch {
		case as.val.b != nil:
			res[field] = fieldValue{Status: statusSet, Value: boolString(*as.val.b)}
		case as.val.isLst:
			res[field] = fieldValue{Status: statusSet, Value: strings.Join(as.val.list, ",")}
		case conv != nil:
			v, isNone, convOK := conv(as.val.s)
			switch {
			case !convOK:
				res[field] = fieldValue{Status: statusSet, Value: as.val.s}
			case isNone:
				res[field] = fieldValue{Status: statusDefault}
			default:
				res[field] = fieldValue{Status: statusSet, Value: v}
			}
		default:
			res[field] = fieldValue{Status: statusSet, Value: as.val.s}
		}
	}

	// model
	if kind == "vllm serve" {
		if mt := pr.modelTag; mt != nil {
			switch {
			case mt.val.unknown():
				res[valModel] = fieldValue{Status: statusUnknown}
			case lastOpaque >= 0 && lastOpaque < mt.pos:
				res[valModel] = fieldValue{Status: statusUnknown}
			default:
				res[valModel] = fieldValue{Status: statusSet, Value: mt.val.s}
			}
		} else {
			res[valModel] = fieldValue{Status: statusOf(0, false)}
		}
	} else {
		put(valModel, "model", nil)
	}
	for _, f := range argFields {
		put(f.name, f.dest, f.conv)
	}
	speculativeFields(pr, statusOf, res)
	return res
}

// speculativeFields reads --speculative-config (JSON or dotted keys), the v0.32
// --spec-method/--spec-model/--spec-tokens flags and the older
// --speculative-model/--num-speculative-tokens flags.
func speculativeFields(pr parseResult, statusOf func(int, bool) fieldStatus, res map[string]fieldValue) {
	var specCfg map[string]any
	specStatus := statusSet // statusSet means "no override"
	if as, ok := pr.assigned["speculative_config"]; ok {
		st := statusOf(as.pos, true)
		switch {
		case st != statusSet:
			specStatus = st
		case as.val.s == "" || as.val.s == "None":
		default:
			var cfg map[string]any
			if err := json.Unmarshal([]byte(as.val.s), &cfg); err != nil || cfg == nil {
				specStatus = statusUnknown
			} else {
				specCfg = map[string]any{}
				for k, v := range cfg {
					specCfg[strings.ReplaceAll(k, "-", "_")] = v
				}
			}
		}
	} else if st := statusOf(0, false); st == statusUnknown {
		specStatus = st
	}
	setKey := func(key string, v any) {
		if specCfg == nil {
			specCfg = map[string]any{}
		}
		specCfg[key] = v
	}
	if as, ok := pr.assigned["spec_method"]; ok {
		setKey("method", rawOrUnk(as.val))
	}
	if as, ok := pr.assigned["spec_model"]; ok {
		setKey("model", rawOrUnk(as.val))
	}
	if as, ok := pr.assigned["spec_tokens"]; ok {
		setKey("num_speculative_tokens", rawOrUnk(as.val))
	}
	if as, ok := pr.assigned["legacy_speculative_model"]; ok {
		m := rawOrUnk(as.val)
		setKey("model", m)
		if _, has := specCfg["method"]; !has {
			if s, isStr := m.(string); isStr && !strings.Contains(s, unk) && s != "" && s != "None" {
				setKey("method", legacySpecMethod(s))
			}
		}
	}
	if as, ok := pr.assigned["legacy_num_speculative_tokens"]; ok {
		setKey("num_speculative_tokens", rawOrUnk(as.val))
	}
	for _, f := range []struct{ field, key string }{
		{valSpeculativeMethod, "method"},
		{valNumSpeculativeTokens, "num_speculative_tokens"},
	} {
		if specStatus != statusSet {
			res[f.field] = fieldValue{Status: specStatus}
			continue
		}
		v, has := specCfg[f.key]
		if specCfg == nil || !has || v == nil {
			res[f.field] = fieldValue{Status: statusDefault}
			continue
		}
		s, known := jsonScalar(v)
		switch {
		case !known:
			res[f.field] = fieldValue{Status: statusUnknown}
		case f.key == "num_speculative_tokens":
			if s == "" || s == "None" {
				res[f.field] = fieldValue{Status: statusDefault}
			} else if c, _, ok := convInt(s); ok {
				res[f.field] = fieldValue{Status: statusSet, Value: c}
			} else {
				res[f.field] = fieldValue{Status: statusSet, Value: s}
			}
		default:
			res[f.field] = fieldValue{Status: statusSet, Value: s}
		}
	}
}

func rawOrUnk(v argValue) any {
	if v.unknown() || v.isLst || v.b != nil {
		return unk
	}
	return v.s
}

// jsonScalar formats a decoded JSON scalar; known=false for unknown or non-scalar values.
func jsonScalar(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		if strings.Contains(t, unk) {
			return "", false
		}
		return t, true
	case float64:
		if t == math.Trunc(t) && math.Abs(t) < 1e15 {
			return strconv.FormatInt(int64(t), 10), true
		}
		return pyFloat(t), true
	case int64:
		return strconv.FormatInt(t, 10), true
	case bool:
		return boolString(t), true
	}
	return "", false
}

// legacySpecMethod infers the method of the pre-v0.8.3 --speculative-model flag.
func legacySpecMethod(model string) string {
	m := strings.ToLower(model)
	switch {
	case m == "[ngram]" || m == "ngram":
		return "ngram"
	case strings.Contains(m, "eagle3"):
		return "eagle3"
	case strings.Contains(m, "eagle"):
		return "eagle"
	case strings.Contains(m, "medusa"):
		return "medusa"
	case strings.Contains(m, "mlp_speculator") || strings.Contains(m, "mlp-speculator"):
		return "mlp_speculator"
	}
	return "draft_model"
}
