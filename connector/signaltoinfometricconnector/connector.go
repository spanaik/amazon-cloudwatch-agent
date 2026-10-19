// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package signaltoinfometricconnector

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/open-telemetry/opentelemetry-collector-contrib/pkg/ottl/contexts/ottllog"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/connector"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.uber.org/zap"
)

// infoMetricConnector matches log records against the rules, keeps the
// latest facts per rule and entity, and emits an Info metric when the facts
// of an entity change.
type infoMetricConnector struct {
	logger *zap.Logger
	cfg    *Config
	next   consumer.Metrics
	rules  []compiledRule
	state  *state
	// now returns the current time. Tests replace it.
	now func() time.Time

	// lastDownstreamErrLog is the time (Unix nanoseconds) of the last
	// logged downstream error.
	lastDownstreamErrLog     atomic.Int64
	suppressedDownstreamErrs atomic.Int64
}

var _ connector.Logs = (*infoMetricConnector)(nil)

func newConnector(set connector.Settings, cfg *Config, next consumer.Metrics) (*infoMetricConnector, error) {
	rules, err := compileRules(cfg, set.TelemetrySettings)
	if err != nil {
		return nil, err
	}
	specs := make([]ruleSpec, len(cfg.Rules))
	for i := range cfg.Rules {
		rc := &cfg.Rules[i]
		specs[i] = ruleSpec{name: rc.Name, identity: rc.Identity, maxEntities: rc.effectiveMaxEntities()}
	}
	return &infoMetricConnector{
		logger: set.Logger,
		cfg:    cfg,
		next:   next,
		rules:  rules,
		state:  newState(specs, cfg.MaxStateBytes),
		now:    time.Now,
	}, nil
}

// Start does not emit: the state is empty until logs arrive.
func (c *infoMetricConnector) Start(context.Context, component.Host) error {
	c.logger.Debug("Starting signal_to_info_metric connector", zap.Int("rules", len(c.rules)))
	return nil
}

func (c *infoMetricConnector) Shutdown(context.Context) error {
	return nil
}

// Capabilities reports that the connector does not mutate its input. The
// connector shares a logs pipeline with other exporters, and MutatesData
// false lets the fanout pass the same data to all of them without a copy.
func (c *infoMetricConnector) Capabilities() consumer.Capabilities {
	return consumer.Capabilities{MutatesData: false}
}

// batch is the per-call scratch space of ConsumeLogs. It is allocated only
// when a record passes the body_contains check of a rule.
type batch struct {
	// resources caches the resourceInfo of the current ResourceLogs, per
	// rule.
	resources []resourceInfo
	obs       []observation
}

// ConsumeLogs scans the records, then applies the observations to the state
// under one lock, then, with emit_on_change, emits the entities that were
// inserted or changed. It may be called concurrently. It returns an error
// only in propagate mode, for an OTTL evaluation error of a condition or of
// a required attribute without a default; the observations made before that
// record are still applied.
func (c *infoMetricConnector) ConsumeLogs(ctx context.Context, ld plog.Logs) error {
	var b *batch
	var err error
	rls := ld.ResourceLogs()
scan:
	for i := 0; i < rls.Len(); i++ {
		rl := rls.At(i)
		if b != nil {
			clear(b.resources)
		}
		sls := rl.ScopeLogs()
		for j := 0; j < sls.Len(); j++ {
			sl := sls.At(j)
			lrs := sl.LogRecords()
			for k := 0; k < lrs.Len(); k++ {
				lr := lrs.At(k)
				for ri := range c.rules {
					// The hot path: most records fail here, without allocating.
					if !c.rules[ri].matchesBody(lr) {
						continue
					}
					if b == nil {
						b = &batch{resources: make([]resourceInfo, len(c.rules))}
					}
					if err = c.observe(ctx, b, ri, rl, sl, lr); err != nil {
						break scan
					}
				}
			}
		}
	}
	if b != nil && len(b.obs) > 0 {
		c.apply(ctx, b.obs)
	}
	return err
}

// observe evaluates a rule on a record that passed body_contains and, if
// the record yields an entity, appends an observation to b.
func (c *infoMetricConnector) observe(ctx context.Context, b *batch, ri int, rl plog.ResourceLogs, sl plog.ScopeLogs, lr plog.LogRecord) error {
	rule := &c.rules[ri]
	res := &b.resources[ri]
	if res.resolved && !res.ok {
		return nil
	}

	tCtx := ottllog.NewTransformContextPtr(rl, sl, lr)
	defer tCtx.Close()
	match, err := rule.matchesConditions(ctx, tCtx)
	if err != nil {
		return fmt.Errorf("rule %q: %w", rule.cfg.Name, err)
	}
	if !match {
		return nil
	}

	if !res.resolved {
		*res = rule.resolveResource(rl.Resource().Attributes(), c.cfg.MaxValueLength)
		if !res.ok {
			if ce := c.logger.Check(zap.DebugLevel, "Matching log record not used: an identity attribute is missing or empty"); ce != nil {
				ce.Write(zap.String("rule", rule.cfg.Name), zap.Strings("identity", rule.cfg.Identity))
			}
			return nil
		}
	}

	facts, ok, err := rule.extractFacts(ctx, tCtx, c.cfg.MaxValueLength, c.cfg.ErrorMode, c.logger)
	if err != nil || !ok {
		return err
	}
	b.obs = append(b.obs, observation{
		rule:     ri,
		key:      res.key,
		identity: res.identity,
		resource: res.resource,
		facts:    facts,
		time:     c.recordTime(lr),
	})
	return nil
}

// apply applies the observations and, with emit_on_change, emits the
// entities they inserted or changed. The state lock is released before
// emitting.
func (c *infoMetricConnector) apply(ctx context.Context, obs []observation) {
	res := c.state.applyBatch(obs, c.cfg.EmitOnChange)
	if len(res.evicted) > 0 {
		if ce := c.logger.Check(zap.DebugLevel, "Evicted Info metric entities"); ce != nil {
			ce.Write(zap.Int("count", len(res.evicted)))
		}
	}
	if c.cfg.EmitOnChange {
		c.emit(ctx, res.changed)
	}
}

// recordTime returns the record's timestamp, else its observed timestamp,
// else now, in Unix nanoseconds.
func (c *infoMetricConnector) recordTime(lr plog.LogRecord) int64 {
	if ts := lr.Timestamp(); ts != 0 {
		return ts.AsTime().UnixNano()
	}
	if ts := lr.ObservedTimestamp(); ts != 0 {
		return ts.AsTime().UnixNano()
	}
	return c.now().UnixNano()
}
