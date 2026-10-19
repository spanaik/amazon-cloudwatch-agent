// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package signaltoinfometricconnector

import (
	"context"
	"time"

	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"
)

// scopeName is the instrumentation scope of the emitted metrics.
const scopeName = "github.com/aws/amazon-cloudwatch-agent/connector/signaltoinfometricconnector"

// downstreamErrorLogInterval bounds how often a failure of the next
// consumer is logged.
const downstreamErrorLogInterval = time.Minute

// buildMetrics returns one ResourceMetrics per entry. The resource has the
// included resource attributes, then, when identity_output_prefix is set,
// "<prefix><identity key>" for each identity key. The single gauge data
// point has the value 1, the facts as attributes in config order, the emit
// time as Timestamp, and FirstSeen as StartTimestamp. The record time is
// never the Timestamp, because CloudWatch rejects data points older than
// 14 days.
func (c *infoMetricConnector) buildMetrics(now time.Time, entries []emitEntry) pmetric.Metrics {
	md := pmetric.NewMetrics()
	rms := md.ResourceMetrics()
	rms.EnsureCapacity(len(entries))
	ts := pcommon.NewTimestampFromTime(now)
	nowNanos := now.UnixNano()
	prefix := c.cfg.IdentityOutputPrefix
	for i := range entries {
		e := &entries[i].entity
		rule := c.rules[entries[i].rule].cfg

		rm := rms.AppendEmpty()
		res := rm.Resource().Attributes()
		res.EnsureCapacity(len(e.Resource) + len(e.Identity))
		for _, p := range e.Resource {
			res.PutStr(p.Key, p.Value)
		}
		if prefix != "" {
			for _, p := range e.Identity {
				res.PutStr(c.cfg.identityOutputKey(p.Key), p.Value)
			}
		}

		sm := rm.ScopeMetrics().AppendEmpty()
		sm.Scope().SetName(scopeName)
		m := sm.Metrics().AppendEmpty()
		m.SetName(rule.Name)
		m.SetDescription(rule.Description)
		m.SetUnit(rule.Unit)
		dp := m.SetEmptyGauge().DataPoints().AppendEmpty()
		dp.SetIntValue(1)
		dp.SetTimestamp(ts)
		// A record time ahead of the emit time (clock skew) must not give a
		// start time after the data point time.
		dp.SetStartTimestamp(pcommon.NewTimestampFromTime(time.Unix(0, min(e.FirstSeen, nowNanos))))
		attrs := dp.Attributes()
		attrs.EnsureCapacity(len(e.Facts))
		for _, p := range e.Facts {
			attrs.PutStr(p.Key, p.Value)
		}
	}
	return md
}

// emit builds the metrics for entries and sends them to the next consumer.
// A downstream error is logged at most once per downstreamErrorLogInterval
// and not returned: the logs pipeline that the connector shares with other
// exporters must not fail because of the metrics side.
func (c *infoMetricConnector) emit(ctx context.Context, entries []emitEntry) {
	if len(entries) == 0 {
		return
	}
	if err := c.next.ConsumeMetrics(ctx, c.buildMetrics(c.now(), entries)); err != nil {
		c.logDownstreamError(err)
	}
}

func (c *infoMetricConnector) logDownstreamError(err error) {
	now := c.now().UnixNano()
	last := c.lastDownstreamErrLog.Load()
	if last != 0 && now-last < downstreamErrorLogInterval.Nanoseconds() {
		c.suppressedDownstreamErrs.Add(1)
		return
	}
	if !c.lastDownstreamErrLog.CompareAndSwap(last, now) {
		c.suppressedDownstreamErrs.Add(1)
		return
	}
	c.logger.Warn("Failed to send Info metrics to the next consumer",
		zap.Error(err),
		zap.Int64("suppressed_since_last_log", c.suppressedDownstreamErrs.Swap(0)),
	)
}
