// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package engineconfig

import (
	"regexp"
	"strings"
)

// containerSpec is the part of a Kubernetes container that the parser reads.
type containerSpec struct {
	Command []string
	Args    []string
	Env     []envVar
	EnvFrom bool
	Image   string
}

type envVar struct {
	Name string
	// Value is the literal value. FromSource is true for valueFrom entries,
	// whose value the pod spec does not show.
	Value      string
	FromSource bool
}

type invocation struct {
	// "vllm serve" | "api_server" | "legacy_api_server" | "sglang serve" |
	// "sglang launch_server" | "dynamo vllm" | "dynamo sglang"
	kind string
	args []word
}

func engineOfKind(kind string) string {
	if strings.HasSuffix(kind, "sglang") || strings.HasPrefix(kind, "sglang ") {
		return engineSGLang
	}
	return engineVLLM
}

// argsResult is what the pod args say about each value. found=false means that no
// server command line of the engine was found, so the args cannot tell any value.
type argsResult struct {
	found  bool
	fields map[string]fieldValue
	phase  string // the phase that the engine flags set, if any
	group  string // the group role that the engine flags set, if any
}

var (
	k8sNameRE        = regexp.MustCompile(`^[-._a-zA-Z][-._a-zA-Z0-9]*$`)
	templateRE       = regexp.MustCompile(`\{\{.*?\}\}|__TPL__`)
	envsubstRE       = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*\}`)
	pythonRE         = regexp.MustCompile(`^python(\d+(\.\d+)?)?$`)
	officialServeRE  = regexp.MustCompile(`(^|/)(vllm/vllm-openai(-cpu|-rocm)?|lmcache/vllm-openai)([:@]|$)`)
	ngcImageRE       = regexp.MustCompile(`^(docker\.io/)?nvcr\.io/nvidia/`)
	timeoutArgRE     = regexp.MustCompile(`^[0-9a-fx,.-]+[smhd]?$`)
	shells           = map[string]bool{"sh": true, "bash": true, "dash": true, "ash": true, "zsh": true, "ksh": true}
	wrappers         = map[string]bool{"exec": true, "nohup": true, "time": true, "command": true, "builtin": true, "sudo": true, "tini": true, "dumb-init": true}
	optionedWrappers = map[string]bool{"numactl": true, "taskset": true, "nice": true, "ionice": true, "chrt": true, "stdbuf": true, "timeout": true}
)

func basename(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// k8sExpand applies Kubernetes $(VAR) expansion (third_party/forked/golang/expansion).
func k8sExpand(s string, envmap map[string]string, envFrom bool) string {
	var out strings.Builder
	n := len(s)
	for i := 0; i < n; {
		c := s[i]
		if c == '$' && i+1 < n {
			next := s[i+1]
			if next == '$' {
				out.WriteByte('$')
				i += 2
				continue
			}
			if next == '(' {
				j := strings.IndexByte(s[i+2:], ')')
				if j == -1 {
					out.WriteString("$(")
					i += 2
					continue
				}
				name := s[i+2 : i+2+j]
				if v, ok := envmap[name]; ok {
					out.WriteString(v)
				} else if envFrom && k8sNameRE.MatchString(name) {
					out.WriteString(unk)
				} else {
					out.WriteString("$(" + name + ")")
				}
				i = i + 2 + j + 1
				continue
			}
			out.WriteByte('$')
			out.WriteByte(next)
			i += 2
			continue
		}
		out.WriteByte(c)
		i++
	}
	return out.String()
}

// buildEnv returns the literal env of a container; valueFrom entries are unknown.
func buildEnv(env []envVar, envFrom bool) map[string]string {
	envmap := map[string]string{}
	for _, e := range env {
		if e.Name == "" {
			continue
		}
		if e.FromSource {
			envmap[e.Name] = unk
			continue
		}
		envmap[e.Name] = k8sExpand(e.Value, envmap, envFrom)
	}
	return envmap
}

func markTemplates(s string) string {
	s = templateRE.ReplaceAllString(s, unk)
	return envsubstRE.ReplaceAllString(s, unk)
}

// stripWrappers removes exec/env/numactl/... prefixes.
func stripWrappers(words []word) []word {
	w := append([]word(nil), words...)
	for changed := true; len(w) > 0 && changed; {
		changed = false
		if w[0].opaque {
			return w
		}
		b := basename(w[0].s)
		switch {
		case wrappers[b]:
			w = w[1:]
			for len(w) > 0 && !w[0].opaque && w[0].s == "--" {
				w = w[1:]
			}
			changed = true
		case b == "env":
			w = w[1:]
			for len(w) > 0 && !w[0].opaque && (strings.HasPrefix(w[0].s, "-") || assignRE.MatchString(w[0].s)) {
				if w[0].s == "-u" || w[0].s == "-C" || w[0].s == "-S" {
					w = w[1:]
					if len(w) == 0 {
						break
					}
				}
				w = w[1:]
			}
			changed = true
		case optionedWrappers[b]:
			w = w[1:]
			for len(w) > 0 && !w[0].opaque && strings.HasPrefix(w[0].s, "-") {
				opt := w[0].s
				w = w[1:]
				if len(opt) == 2 && len(w) > 0 && !w[0].opaque && !strings.HasPrefix(w[0].s, "-") && b != "stdbuf" {
					w = w[1:]
				}
			}
			if (b == "taskset" || b == "chrt" || b == "timeout") && len(w) > 0 && !w[0].opaque && timeoutArgRE.MatchString(w[0].s) {
				w = w[1:]
			}
			changed = true
		case b == "uv" && len(w) > 1 && !w[1].opaque && w[1].s == "run":
			w = w[2:]
			for len(w) > 0 && !w[0].opaque && strings.HasPrefix(w[0].s, "-") {
				w = w[1:]
			}
			changed = true
		case assignRE.MatchString(w[0].s):
			w = w[1:]
			changed = true
		}
	}
	return w
}

// matchInvocation reports whether words start a vLLM or an SGLang server, or a
// Dynamo launcher of one.
func matchInvocation(words []word) (string, []word, bool) {
	if len(words) == 0 || words[0].opaque {
		return "", nil, false
	}
	b := basename(words[0].s)
	if b == "vllm" || b == "sglang" {
		if len(words) > 1 && !words[1].opaque && words[1].s == "serve" {
			return b + " serve", words[2:], true
		}
		return "", nil, false
	}
	if !pythonRE.MatchString(b) {
		return "", nil, false
	}
	j := 1
	mod := ""
	hasMod := false
	for j < len(words) && !words[j].opaque && strings.HasPrefix(words[j].s, "-") {
		a := words[j].s
		if a == "-m" {
			if j+1 < len(words) {
				mod = words[j+1].s
				hasMod = !words[j+1].opaque
				j += 2
			}
			break
		}
		if strings.HasPrefix(a, "-m") {
			mod, hasMod = a[2:], true
			j++
			break
		}
		if a == "-c" {
			return "", nil, false
		}
		if a == "-W" || a == "-X" || a == "-Q" {
			j += 2
		} else {
			j++
		}
	}
	if !hasMod {
		return "", nil, false
	}
	var rest []word
	if j < len(words) {
		rest = words[j:]
	}
	switch {
	case mod == "vllm.entrypoints.openai.api_server":
		return "api_server", rest, true
	case mod == "vllm.entrypoints.api_server":
		return "legacy_api_server", rest, true
	case (mod == "vllm.entrypoints.cli.main" || mod == "vllm") && len(rest) > 0 && !rest[0].opaque && rest[0].s == "serve":
		return "vllm serve", rest[1:], true
	case mod == "sglang.launch_server":
		return "sglang launch_server", rest, true
	case mod == "dynamo.vllm" || mod == "dynamo.sglang":
		return "dynamo " + mod[len("dynamo."):], rest, true
	}
	return "", nil, false
}

// shellScriptOf returns the script and its positional words if words run a shell
// with -c. The third result is set when the shell runs a script file instead.
func shellScriptOf(words []word) (word, []word, bool, bool) {
	if len(words) == 0 || words[0].opaque || !shells[basename(words[0].s)] {
		return word{}, nil, false, false
	}
	j := 1
	hasC := false
	for j < len(words) && !words[j].opaque {
		a := words[j].s
		if a == "--" || a == "-" {
			j++
			break
		}
		if (strings.HasPrefix(a, "-") || strings.HasPrefix(a, "+")) && len(a) > 1 {
			letters := a[1:]
			if strings.Contains(letters, "c") {
				hasC = true
			}
			j++
			if strings.Contains(letters, "o") && j < len(words) {
				j++
			}
			continue
		}
		break
	}
	if hasC {
		if j >= len(words) {
			return word{}, nil, false, false
		}
		return words[j], words[j+1:], false, true
	}
	if j < len(words) {
		return word{}, nil, true, true
	}
	return word{}, nil, false, false
}

func handleWords(words []word, ctx *shellCtx, found *[]invocation, via string) {
	words = stripWrappers(words)
	if len(words) == 0 {
		return
	}
	if kind, rest, ok := matchInvocation(words); ok {
		args := make([]word, len(rest))
		for i, a := range rest {
			switch {
			case a.opaque:
				args[i] = a
			case via == "exec":
				// exec form: nothing expands ${VAR} or {{ }}, so they are deploy-time templates
				args[i] = strWord(markTemplates(a.s))
			default:
				args[i] = strWord(templateRE.ReplaceAllString(a.s, unk))
			}
		}
		*found = append(*found, invocation{kind: kind, args: args})
		return
	}
	if script, pos, isFile, ok := shellScriptOf(words); ok {
		if isFile {
			ctx.notes = append(ctx.notes, "runs a script file")
			return
		}
		if script.opaque {
			ctx.notes = append(ctx.notes, "shell -c script is unknown")
			return
		}
		if ctx.depth >= 4 {
			return
		}
		sub := &shellCtx{envmap: ctx.envmap, vars: map[string]string{}, depth: ctx.depth + 1, posKnown: true}
		for k, v := range ctx.vars {
			sub.vars[k] = v
		}
		for _, p := range pos {
			if p.opaque {
				sub.posKnown = false
				break
			}
			sub.positional = append(sub.positional, p.s)
		}
		if !sub.posKnown {
			sub.positional = nil
		}
		parseShell(script.s, sub, found)
		ctx.notes = append(ctx.notes, sub.notes...)
	}
}

// imageEntrypoint tells how an args-only vLLM container starts: "vllm serve" for
// the official vllm-openai images, "command" for images that run the args as a
// command (NGC), "assumed" for other images whose name contains vllm, or "".
func imageEntrypoint(image string) string {
	fields := strings.Fields(strings.Trim(strings.TrimSpace(image), `"`))
	if len(fields) == 0 {
		return ""
	}
	img := fields[0]
	switch {
	case officialServeRE.MatchString(img):
		return "vllm serve"
	case ngcImageRE.MatchString(img):
		return "command"
	case strings.Contains(strings.ToLower(img), "vllm"):
		return "assumed"
	}
	return ""
}

// parseContainerArgs reads the engine flags from one container spec. SGLang
// images do not start the server by themselves, so an args-only SGLang
// container must hold the whole command line.
func parseContainerArgs(c containerSpec, engine string) argsResult {
	envmap := buildEnv(c.Env, c.EnvFrom)
	cmd := make([]string, len(c.Command))
	for i, s := range c.Command {
		cmd[i] = k8sExpand(s, envmap, c.EnvFrom)
	}
	arg := make([]string, len(c.Args))
	for i, s := range c.Args {
		arg[i] = k8sExpand(s, envmap, c.EnvFrom)
	}

	var argv []string
	if len(cmd) > 0 {
		argv = append(cmd, arg...)
	} else {
		if len(arg) == 0 {
			return argsResult{}
		}
		ep := ""
		if engine == engineVLLM {
			ep = imageEntrypoint(c.Image)
		}
		b0 := basename(arg[0])
		switch {
		case shells[b0] || b0 == engine || pythonRE.MatchString(b0):
			argv = arg // args hold the whole command line
		case ep == "":
			return argsResult{}
		case ep == "command":
			argv = arg
		case ep == "assumed" && !strings.HasPrefix(arg[0], "-"):
			return argsResult{}
		default:
			if arg[0] == "serve" {
				arg = arg[1:]
			}
			argv = append([]string{"vllm", "serve"}, arg...)
		}
	}

	var all, found []invocation
	ctx := &shellCtx{envmap: envmap, vars: map[string]string{}}
	handleWords(strWords(argv), ctx, &all, "exec")
	for _, inv := range all {
		if engineOfKind(inv.kind) == engine {
			found = append(found, inv)
		}
	}
	if len(found) == 0 {
		return argsResult{}
	}
	// several command lines (for example in if/else branches) must agree
	var results []map[string]fieldValue
	phase, group := "", ""
	for i, inv := range found {
		var pr parseResult
		if engine == engineSGLang {
			pr = sglangParse(inv.kind, inv.args)
			results = append(results, sglangFieldValues(pr))
		} else {
			pr = vllmParse(inv.kind, inv.args)
			results = append(results, vllmFieldValues(inv.kind, pr))
		}
		p, g := flagPhase(inv.kind, pr), flagGroup(pr)
		if i == 0 {
			phase, group = p, g
		}
		if p != phase {
			phase = ""
		}
		if g != group {
			group = ""
		}
	}
	fields := results[0]
	if len(results) > 1 {
		for name, fv := range fields {
			for _, other := range results[1:] {
				if other[name] != fv {
					fields[name] = fieldValue{Status: statusUnknown}
					break
				}
			}
		}
	}
	return argsResult{found: true, fields: fields, phase: phase, group: group}
}

// mentionsEngine reports whether the image or the command line names the engine.
func mentionsEngine(c containerSpec, engine string) bool {
	for _, s := range append(append([]string{c.Image}, c.Command...), c.Args...) {
		if strings.Contains(strings.ToLower(s), engine) {
			return true
		}
	}
	return false
}
