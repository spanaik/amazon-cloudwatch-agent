// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package engineconfig

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The shell layer reads `sh -c` / `bash -c` scripts. It is a port of the reference
// parser (parse_vllm.py): a quote-aware pre-pass removes comments, line
// continuations, heredoc bodies and redirections, and replaces expansions with
// placeholders; a POSIX shlex tokenizer then splits the text into words.

// unk marks an unknown part inside a string.
const unk = "\x00"

// word is one argv item. An opaque word is a shell expansion that may expand to
// any number of argv items.
type word struct {
	s      string
	opaque bool
}

func strWord(s string) word { return word{s: s} }

func (w word) unknown() bool { return w.opaque || strings.Contains(w.s, unk) }

func strWords(ss []string) []word {
	out := make([]word, len(ss))
	for i, s := range ss {
		out[i] = strWord(s)
	}
	return out
}

type shellExp struct {
	kind   string // var | pos | at | star | special | cmdsub
	name   string
	def    string
	quoted bool
	op     string
}

var (
	braceExpRE  = regexp.MustCompile(`^(?s)([A-Za-z_][A-Za-z0-9_]*|\d+)(:?[-=+?])?(.*)$`)
	varNameRE   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*`)
	redirectRE  = regexp.MustCompile(`^(&>>?|>>|>&|<&|>\||<>|[<>])`)
	placeRE     = regexp.MustCompile("\x01(\\d+)\x01")
	assignRE    = regexp.MustCompile(`^(?s)([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)
	opaqueNames = regexp.MustCompile(`(?i)ARGS|FLAGS|OPTS|OPTIONS|EXTRA|PARAMS`)
)

const shellPunct = ";&|()\n"

var (
	keywordsOpen  = map[string]bool{"if": true, "while": true, "until": true, "case": true, "for": true, "select": true}
	keywordsClose = map[string]bool{"fi": true, "done": true, "esac": true}
	keywordsSkip  = map[string]bool{"then": true, "else": true, "elif": true, "do": true, "{": true, "}": true, "!": true, "in": true}
)

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// findMatching returns the index of the closer that matches s[i] == open, quote-aware.
func findMatching(s string, i int, open, closer byte) int {
	depth := 0
	var q byte
	j := i
	for j < len(s) {
		c := s[j]
		if q != 0 {
			if c == '\\' && q == '"' {
				j += 2
				continue
			}
			if c == q {
				q = 0
			}
		} else {
			switch {
			case c == '\'' || c == '"':
				q = c
			case c == '\\':
				j += 2
				continue
			case c == open:
				depth++
			case c == closer:
				depth--
				if depth == 0 {
					return j
				}
			}
		}
		j++
	}
	return -1
}

// shellPrepass returns the script text with expansions replaced by \x01N\x01.
func shellPrepass(script string) (string, []shellExp) {
	var out []string
	var exps []shellExp
	n := len(script)
	var state byte
	wordStart := true
	type heredoc struct {
		delim     string
		stripTabs bool
	}
	var heredocs []heredoc

	addExp := func(e shellExp) {
		exps = append(exps, e)
		out = append(out, fmt.Sprintf("\x01%d\x01", len(exps)-1))
	}

	readDollar := func(i int, quoted bool) int {
		if i+1 >= n {
			out = append(out, "$")
			return i + 1
		}
		c := script[i+1]
		if c == '{' {
			j := findMatching(script, i+1, '{', '}')
			if j == -1 {
				out = append(out, "$")
				return i + 1
			}
			body := script[i+2 : j]
			m := braceExpRE.FindStringSubmatch(body)
			if m != nil && !strings.HasPrefix(body, "#") {
				name, op, rest := m[1], m[2], m[3]
				kind := "var"
				if isDigits(name) {
					kind = "pos"
				}
				if op == "" && rest != "" {
					addExp(shellExp{kind: "special", name: name, quoted: quoted})
				} else {
					dflt := rest
					if len(dflt) >= 2 && (dflt[0] == '\'' || dflt[0] == '"') && dflt[len(dflt)-1] == dflt[0] {
						dflt = dflt[1 : len(dflt)-1]
					}
					addExp(shellExp{kind: kind, name: name, def: dflt, quoted: quoted, op: op})
				}
			} else {
				addExp(shellExp{kind: "special", name: body, quoted: quoted})
			}
			return j + 1
		}
		if c == '(' {
			j := findMatching(script, i+1, '(', ')')
			if j == -1 {
				addExp(shellExp{kind: "cmdsub", quoted: quoted})
				return n
			}
			addExp(shellExp{kind: "cmdsub", name: script[i+2 : j], quoted: quoted})
			return j + 1
		}
		if m := varNameRE.FindString(script[i+1:]); m != "" {
			addExp(shellExp{kind: "var", name: m, quoted: quoted})
			return i + 1 + len(m)
		}
		switch {
		case c >= '0' && c <= '9':
			addExp(shellExp{kind: "pos", name: string(c), quoted: quoted})
			return i + 2
		case c == '@':
			addExp(shellExp{kind: "at", quoted: quoted})
			return i + 2
		case c == '*':
			addExp(shellExp{kind: "star", quoted: quoted})
			return i + 2
		case strings.IndexByte("#?$!-", c) >= 0:
			addExp(shellExp{kind: "special", name: string(c), quoted: quoted})
			return i + 2
		}
		out = append(out, "$")
		return i + 1
	}

	skipWord := func(i int) int {
		for i < n && (script[i] == ' ' || script[i] == '\t') {
			i++
		}
		var q byte
		for i < n {
			c := script[i]
			if q != 0 {
				if c == '\\' && q == '"' {
					i += 2
					continue
				}
				if c == q {
					q = 0
				}
				i++
				continue
			}
			if c == '\'' || c == '"' {
				q = c
				i++
				continue
			}
			if c == '\\' {
				i += 2
				continue
			}
			if strings.IndexByte(" \t\n;&|()<>", c) >= 0 {
				break
			}
			i++
		}
		if i > n {
			i = n
		}
		return i
	}

	i := 0
	for i < n {
		c := script[i]
		if state == '\'' {
			out = append(out, string(c))
			if c == '\'' {
				state = 0
			}
			i++
			continue
		}
		if c == '\\' {
			if i+1 < n && script[i+1] == '\n' {
				i += 2 // line continuation
				continue
			}
			end := i + 2
			if end > n {
				end = n
			}
			out = append(out, script[i:end])
			i += 2
			wordStart = false
			continue
		}
		if state == '"' {
			switch c {
			case '"':
				state = 0
				out = append(out, string(c))
				i++
			case '$':
				i = readDollar(i, true)
			case '`':
				j := strings.IndexByte(script[i+1:], '`')
				if j == -1 {
					addExp(shellExp{kind: "cmdsub", name: script[i+1:], quoted: true})
					i = n
				} else {
					addExp(shellExp{kind: "cmdsub", name: script[i+1 : i+1+j], quoted: true})
					i = i + 1 + j + 1
				}
			default:
				out = append(out, string(c))
				i++
			}
			continue
		}
		// unquoted
		switch {
		case c == '\'' || c == '"':
			state = c
			out = append(out, string(c))
			i++
			wordStart = false
			continue
		case c == '#' && wordStart:
			j := strings.IndexByte(script[i:], '\n')
			if j == -1 {
				i = n
			} else {
				i += j
			}
			continue
		case c == '\n':
			out = append(out, "\n")
			i++
			wordStart = true
			for len(heredocs) > 0 {
				hd := heredocs[0]
				heredocs = heredocs[1:]
				for i < n {
					j := strings.IndexByte(script[i:], '\n')
					var line string
					if j == -1 {
						line = script[i:]
						i = n
					} else {
						line = script[i : i+j]
						i = i + j + 1
					}
					if hd.stripTabs {
						line = strings.TrimLeft(line, "\t")
					}
					if strings.TrimSpace(line) == hd.delim {
						break
					}
				}
			}
			continue
		case strings.HasPrefix(script[i:], "<<<"):
			i = skipWord(i + 3)
			out = append(out, " ")
			continue
		case strings.HasPrefix(script[i:], "<<"):
			j := i + 2
			stripTabs := false
			if j < n && script[j] == '-' {
				stripTabs = true
				j++
			}
			for j < n && (script[j] == ' ' || script[j] == '\t') {
				j++
			}
			k := skipWord(j)
			delim := strings.Trim(script[j:k], `'"`)
			heredocs = append(heredocs, heredoc{delim: delim, stripTabs: stripTabs})
			out = append(out, " ")
			i = k
			continue
		case c == '<' || c == '>' || (c == '&' && strings.HasPrefix(script[i:], "&>")):
			// redirection: drop a preceding fd number, the operator and the target
			k := len(out)
			for k > 0 && isDigits(out[k-1]) {
				k--
			}
			if k < len(out) && (k == 0 || out[k-1] == " " || out[k-1] == "\t" || out[k-1] == "\n") {
				out = out[:k]
			}
			op := redirectRE.FindString(script[i:])
			i += len(op)
			if (op == ">&" || op == "<&") && i < n && (script[i] == '-' || (script[i] >= '0' && script[i] <= '9')) {
				for i < n && (script[i] == '-' || (script[i] >= '0' && script[i] <= '9')) {
					i++
				}
			} else {
				i = skipWord(i)
			}
			out = append(out, " ")
			continue
		case c == '$':
			i = readDollar(i, false)
			wordStart = false
			continue
		case c == '`':
			j := strings.IndexByte(script[i+1:], '`')
			if j == -1 {
				addExp(shellExp{kind: "cmdsub", name: script[i+1:]})
				i = n
			} else {
				addExp(shellExp{kind: "cmdsub", name: script[i+1 : i+1+j]})
				i = i + 1 + j + 1
			}
			continue
		}
		wordStart = strings.IndexByte(" \t;&|()", c) >= 0
		out = append(out, string(c))
		i++
	}
	return strings.Join(out, ""), exps
}

var errShellTokenize = errors.New("shell tokenize error")

// shlexSplit is a port of Python's shlex.shlex(text, posix=True,
// punctuation_chars=";&|()\n") with whitespace " \t\r", whitespace_split=True and
// no comment characters.
func shlexSplit(text string) ([]string, error) {
	isWS := func(c byte) bool { return c == ' ' || c == '\t' || c == '\r' }
	isPunct := func(c byte) bool { return strings.IndexByte(shellPunct, c) >= 0 }
	var tokens []string
	var tok strings.Builder
	quoted := false
	state := byte(' ')
	escapedState := byte('a')
	emit := func() {
		tokens = append(tokens, tok.String())
		tok.Reset()
		quoted = false
	}
	i := 0
	n := len(text)
	for {
		eof := i >= n
		var c byte
		if !eof {
			c = text[i]
		}
		switch state {
		case ' ':
			if eof {
				return tokens, nil
			}
			i++
			switch {
			case isWS(c):
			case c == '\\':
				escapedState = 'a'
				state = '\\'
			case isPunct(c):
				tok.WriteByte(c)
				state = 'c'
			case c == '\'' || c == '"':
				state = c
			default:
				tok.WriteByte(c)
				state = 'a'
			}
		case '\'', '"':
			quoted = true
			if eof {
				return nil, fmt.Errorf("%w: no closing quotation", errShellTokenize)
			}
			i++
			switch {
			case c == state:
				state = 'a'
			case c == '\\' && state == '"':
				escapedState = state
				state = '\\'
			default:
				tok.WriteByte(c)
			}
		case '\\':
			if eof {
				return nil, fmt.Errorf("%w: no escaped character", errShellTokenize)
			}
			i++
			if (escapedState == '\'' || escapedState == '"') && c != '\\' && c != escapedState {
				tok.WriteByte('\\')
			}
			tok.WriteByte(c)
			state = escapedState
		case 'a', 'c':
			if eof {
				if tok.Len() > 0 || quoted {
					emit()
				}
				return tokens, nil
			}
			if isWS(c) {
				i++
				state = ' '
				if tok.Len() > 0 || quoted {
					emit()
				}
				continue
			}
			if state == 'c' {
				if isPunct(c) {
					tok.WriteByte(c)
					i++
				} else {
					// pushback: the character starts the next token
					state = ' '
					emit()
				}
				continue
			}
			switch {
			case c == '\'' || c == '"':
				i++
				state = c
			case c == '\\':
				i++
				escapedState = 'a'
				state = '\\'
			case !isPunct(c):
				i++
				tok.WriteByte(c)
			default:
				// punctuation ends the word and is read again as the next token
				state = ' '
				if tok.Len() > 0 || quoted {
					emit()
				}
			}
		}
	}
}

type shellCtx struct {
	envmap     map[string]string // literal pod env (values may contain unk)
	vars       map[string]string // script assignments
	positional []string          // $0, $1, ... when known
	posKnown   bool
	depth      int
	cond       int
	notes      []string
}

func (c *shellCtx) lookup(name string) (string, bool) {
	if v, ok := c.vars[name]; ok {
		return v, true
	}
	if v, ok := c.envmap[name]; ok {
		return v, true
	}
	return "", false // not in the pod spec (the image ENV could define it)
}

type resolved struct {
	kind  string // str | words | opaque
	s     string
	words []string
}

func resolveExp(exp shellExp, ctx *shellCtx) resolved {
	switch exp.kind {
	case "var":
		v, ok := ctx.lookup(exp.name)
		switch exp.op {
		case ":-", "-", ":=", "=":
			if !ok {
				return resolved{kind: "str", s: exp.def}
			}
			if strings.HasPrefix(exp.op, ":") && v == "" {
				return resolved{kind: "str", s: exp.def}
			}
			return resolved{kind: "str", s: v}
		case ":+", "+", ":?", "?":
			return resolved{kind: "str", s: unk}
		}
		if !ok {
			if !exp.quoted && opaqueNames.MatchString(exp.name) {
				return resolved{kind: "opaque"}
			}
			return resolved{kind: "str", s: unk}
		}
		return resolved{kind: "str", s: v}
	case "pos":
		idx, _ := strconv.Atoi(exp.name)
		if !ctx.posKnown {
			return resolved{kind: "str", s: unk}
		}
		if idx < len(ctx.positional) {
			return resolved{kind: "str", s: ctx.positional[idx]}
		}
		return resolved{kind: "str", s: exp.def}
	case "at", "star":
		if !ctx.posKnown {
			return resolved{kind: "opaque"}
		}
		var rest []string
		if len(ctx.positional) > 1 {
			rest = ctx.positional[1:]
		}
		if exp.kind == "at" && exp.quoted {
			return resolved{kind: "words", words: rest}
		}
		return resolved{kind: "str", s: strings.Join(rest, " ")}
	}
	return resolved{kind: "str", s: unk}
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

// expandWord expands the placeholders in one shell word. split=false is for
// assignment values (no field splitting).
func expandWord(w string, exps []shellExp, ctx *shellCtx, split bool) []word {
	// split w into literal parts and placeholder indexes
	var parts []string
	var idxs []int
	last := 0
	for _, m := range placeRE.FindAllStringSubmatchIndex(w, -1) {
		parts = append(parts, w[last:m[0]])
		k, _ := strconv.Atoi(w[m[2]:m[3]])
		idxs = append(idxs, k)
		last = m[1]
	}
	parts = append(parts, w[last:])

	words := []string{""}
	literal := false
	for k, part := range parts {
		words[len(words)-1] += part
		literal = literal || part != ""
		if k >= len(idxs) {
			break
		}
		if idxs[k] >= len(exps) {
			continue
		}
		exp := exps[idxs[k]]
		r := resolveExp(exp, ctx)
		switch {
		case r.kind == "opaque":
			if !split {
				words[len(words)-1] += unk
				continue
			}
			return []word{{opaque: true}}
		case r.kind == "words":
			if len(r.words) == 0 {
				continue
			}
			words[len(words)-1] += r.words[0]
			words = append(words, r.words[1:]...)
			literal = true
		case exp.quoted || !split || strings.Contains(r.s, unk):
			words[len(words)-1] += r.s
			literal = literal || exp.quoted
		default:
			// unquoted known value: field splitting on whitespace
			pieces := strings.Fields(r.s)
			if len(pieces) == 0 {
				continue
			}
			if isSpaceByte(r.s[0]) && words[len(words)-1] != "" {
				words = append(words, "")
			}
			words[len(words)-1] += pieces[0]
			words = append(words, pieces[1:]...)
			if isSpaceByte(r.s[len(r.s)-1]) {
				words = append(words, "")
			}
		}
	}
	var out []word
	for _, s := range words {
		if s != "" {
			out = append(out, strWord(s))
		}
	}
	if len(out) == 0 && literal {
		return []word{strWord("")}
	}
	return out
}

func splitCommands(tokens []string) [][]string {
	var cmds [][]string
	var cur []string
	for _, t := range tokens {
		if t != "" && strings.Trim(t, shellPunct) == "" {
			if len(cur) > 0 {
				cmds = append(cmds, cur)
			}
			cur = nil
			continue
		}
		cur = append(cur, t)
	}
	if len(cur) > 0 {
		cmds = append(cmds, cur)
	}
	return cmds
}

func parseShell(script string, ctx *shellCtx, found *[]invocation) {
	text, exps := shellPrepass(script)
	tokens, err := shlexSplit(text)
	if err != nil {
		ctx.notes = append(ctx.notes, err.Error())
		return
	}
	for _, cmd := range splitCommands(tokens) {
		for len(cmd) > 0 && (keywordsOpen[cmd[0]] || keywordsClose[cmd[0]] || keywordsSkip[cmd[0]]) {
			kw := cmd[0]
			cmd = cmd[1:]
			switch {
			case keywordsOpen[kw]:
				ctx.cond++
				if kw == "for" || kw == "select" || kw == "case" {
					cmd = nil
				}
			case keywordsClose[kw]:
				if ctx.cond > 0 {
					ctx.cond--
				}
			}
		}
		if len(cmd) == 0 {
			continue
		}
		var assigns []string
		for len(cmd) > 0 && assignRE.MatchString(cmd[0]) {
			assigns = append(assigns, cmd[0])
			cmd = cmd[1:]
		}
		if len(cmd) > 0 {
			switch cmd[0] {
			case "export", "declare", "local", "readonly", "typeset":
				cmd = cmd[1:]
				for len(cmd) > 0 && strings.HasPrefix(cmd[0], "-") {
					cmd = cmd[1:]
				}
				for len(cmd) > 0 && assignRE.MatchString(cmd[0]) {
					assigns = append(assigns, cmd[0])
					cmd = cmd[1:]
				}
				cmd = nil
			}
		}
		if len(cmd) == 0 {
			for _, a := range assigns {
				m := assignRE.FindStringSubmatch(a)
				name, raw := m[1], m[2]
				val := ""
				if raw != "" {
					var sb strings.Builder
					for _, w := range expandWord(raw, exps, ctx, false) {
						if w.opaque {
							sb.WriteString(unk)
						} else {
							sb.WriteString(w.s)
						}
					}
					val = sb.String()
				}
				if ctx.cond > 0 {
					val = unk
				}
				ctx.vars[name] = val
			}
			continue
		}
		var words []word
		for _, t := range cmd {
			words = append(words, expandWord(t, exps, ctx, true)...)
		}
		handleWords(words, ctx, found, "shell")
	}
}
