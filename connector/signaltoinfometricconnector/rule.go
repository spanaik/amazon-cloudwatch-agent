// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package signaltoinfometricconnector

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl"
	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottllog"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.uber.org/zap"
)

// compiledRule is a RuleConfig with its OTTL compiled for the hot path.
type compiledRule struct {
	cfg *RuleConfig
	// conditions is nil when the rule has no conditions.
	conditions *ottl.ConditionSequence[*ottllog.TransformContext]
	attrs      []compiledAttr
}

// compiledAttr is an AttributeConfig with its value expression compiled.
type compiledAttr struct {
	key          string
	expr         *ottl.ValueExpression[*ottllog.TransformContext]
	defaultValue string
	optional     bool
}

// compileRules compiles the OTTL of every rule. Validate has already parsed
// the same expressions, so an error here means the configuration was not
// validated.
func compileRules(cfg *Config, set component.TelemetrySettings) ([]compiledRule, error) {
	parser, err := newLogParser(set)
	if err != nil {
		return nil, fmt.Errorf("failed to create OTTL log parser: %w", err)
	}
	rules := make([]compiledRule, len(cfg.Rules))
	for i := range cfg.Rules {
		rc := &cfg.Rules[i]
		rule := compiledRule{cfg: rc, attrs: make([]compiledAttr, len(rc.Attributes))}
		if len(rc.Conditions) > 0 {
			conditions, err := parser.ParseConditions(rc.Conditions)
			if err != nil {
				return nil, fmt.Errorf("%s: conditions: %w", rc.label(i), err)
			}
			seq := ottl.NewConditionSequence(conditions, set,
				ottl.WithLogicOperation[*ottllog.TransformContext](ottl.Or),
				ottl.WithConditionSequenceErrorMode[*ottllog.TransformContext](cfg.ErrorMode),
			)
			rule.conditions = &seq
		}
		for j := range rc.Attributes {
			ac := &rc.Attributes[j]
			expr, err := parser.ParseValueExpression(ac.valueExpression())
			if err != nil {
				return nil, fmt.Errorf("%s: attributes[%d]: %w", rc.label(i), j, err)
			}
			rule.attrs[j] = compiledAttr{key: ac.Key, expr: expr, defaultValue: ac.DefaultValue, optional: ac.Optional}
		}
		rules[i] = rule
	}
	return rules, nil
}

// matchesBody applies the body_contains check: the body must be a string
// that contains at least one of the substrings. A rule without
// body_contains passes. It does not allocate, because it runs for every
// record and rule.
func (r *compiledRule) matchesBody(lr plog.LogRecord) bool {
	subs := r.cfg.BodyContains
	if len(subs) == 0 {
		return true
	}
	body := lr.Body()
	if body.Type() != pcommon.ValueTypeStr {
		return false
	}
	s := body.Str()
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// matchesConditions applies the OTTL conditions, ORed. A rule without
// conditions passes. The error is non-nil only in propagate mode; the
// condition sequence logs (ignore) or drops (silent) errors otherwise.
func (r *compiledRule) matchesConditions(ctx context.Context, tCtx *ottllog.TransformContext) (bool, error) {
	if r.conditions == nil {
		return true, nil
	}
	return r.conditions.Eval(ctx, tCtx)
}

// resourceInfo is what a rule takes from the resource of a ResourceLogs.
// It is computed at most once per ResourceLogs and rule.
type resourceInfo struct {
	resolved bool
	// ok is false when an identity key is missing or empty.
	ok bool
	// key is the encoded identity, the key of the entity in the state.
	key      string
	identity []kv
	resource []kv
}

// resolveResource reads the identity and the included resource attributes.
// Values are converted with AsString, truncated to maxLen bytes, and cloned
// so that the state does not keep the input's memory alive. An included
// attribute that is absent or empty is omitted. The identity key is
// computed from the truncated values, so the state and the emitted labels
// agree.
func (r *compiledRule) resolveResource(attrs pcommon.Map, maxLen int) resourceInfo {
	identity := make([]kv, 0, len(r.cfg.Identity))
	for _, k := range r.cfg.Identity {
		v, ok := attrs.Get(k)
		if !ok {
			return resourceInfo{resolved: true}
		}
		s := truncateUTF8(v.AsString(), maxLen)
		if s == "" {
			return resourceInfo{resolved: true}
		}
		identity = append(identity, kv{Key: k, Value: strings.Clone(s)})
	}
	var resource []kv
	if len(r.cfg.IncludeResourceAttributes) > 0 {
		resource = make([]kv, 0, len(r.cfg.IncludeResourceAttributes))
	}
	for _, k := range r.cfg.IncludeResourceAttributes {
		v, ok := attrs.Get(k)
		if !ok {
			continue
		}
		if s := truncateUTF8(v.AsString(), maxLen); s != "" {
			resource = append(resource, kv{Key: k, Value: strings.Clone(s)})
		}
	}
	return resourceInfo{resolved: true, ok: true, key: encodeIdentity(identity), identity: identity, resource: resource}
}

// extractFacts evaluates the attributes in config order. A value that is an
// error, nil, or empty (also after truncation) is missing: default_value
// replaces it, optional omits it, and otherwise the record is not used (the
// bool is false). The error is non-nil only in propagate mode, when a
// required fact without a default fails to evaluate.
func (r *compiledRule) extractFacts(ctx context.Context, tCtx *ottllog.TransformContext, maxLen int, errorMode ottl.ErrorMode, logger *zap.Logger) ([]kv, bool, error) {
	facts := make([]kv, 0, len(r.attrs))
	for i := range r.attrs {
		attr := &r.attrs[i]
		raw, err := attr.expr.Eval(ctx, tCtx)
		var v string
		if err == nil {
			v = truncateUTF8(valueString(raw), maxLen)
		}
		switch {
		case v != "":
			facts = append(facts, kv{Key: attr.key, Value: strings.Clone(v)})
		case attr.defaultValue != "":
			// Validate ensures that default_value fits max_value_length.
			facts = append(facts, kv{Key: attr.key, Value: attr.defaultValue})
		case attr.optional:
			// Omitted.
		default:
			if err != nil && errorMode == ottl.PropagateError {
				return nil, false, fmt.Errorf("rule %q: failed to evaluate required attribute %q: %w", r.cfg.Name, attr.key, err)
			}
			if errorMode != ottl.SilentError {
				if ce := logger.Check(zap.DebugLevel, "Matching log record not used: a required attribute has no value"); ce != nil {
					ce.Write(zap.String("rule", r.cfg.Name), zap.String("attribute", attr.key), zap.Error(err))
				}
			}
			return nil, false, nil
		}
	}
	return facts, true, nil
}

// encodeIdentity encodes identity values as a state key. Each value is
// prefixed with its length ("<len>:<value>"), so different value lists
// never produce the same key.
func encodeIdentity(identity []kv) string {
	n := 0
	for _, p := range identity {
		n += len(p.Value) + 4
	}
	var b strings.Builder
	b.Grow(n)
	for _, p := range identity {
		b.WriteString(strconv.Itoa(len(p.Value)))
		b.WriteByte(':')
		b.WriteString(p.Value)
	}
	return b.String()
}

// valueString converts the result of a value expression to a string with
// pcommon.Value.AsString semantics: int 4 is "4", bool true is "true", and
// maps and slices are JSON. Types that pcommon cannot hold, for example
// time.Time, use their fmt representation. nil is "".
func valueString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case pcommon.Value:
		return t.AsString()
	case pcommon.Map:
		val := pcommon.NewValueMap()
		t.CopyTo(val.Map())
		return val.AsString()
	case pcommon.Slice:
		val := pcommon.NewValueSlice()
		t.CopyTo(val.Slice())
		return val.AsString()
	}
	val := pcommon.NewValueEmpty()
	if err := val.FromRaw(v); err != nil {
		return fmt.Sprint(v)
	}
	return val.AsString()
}

// truncateUTF8 returns s cut to at most n bytes without splitting a UTF-8
// sequence: if the cut falls inside a multi-byte character, it moves back to
// the start of that character. Invalid UTF-8 is cut at n.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// s[n] is the first byte that does not fit. In valid UTF-8 the start of
	// its character is at most utf8.UTFMax-1 bytes earlier.
	for cut := n; cut >= 0 && cut > n-utf8.UTFMax; cut-- {
		if utf8.RuneStart(s[cut]) {
			return s[:cut]
		}
	}
	return s[:n]
}
