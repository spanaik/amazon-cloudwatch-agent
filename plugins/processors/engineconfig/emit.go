// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package engineconfig

import (
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

// Record shapes. The PoC runs them side by side to compare them.
const (
	// shapeAnchor puts the full record on the data points of one real gauge per engine.
	shapeAnchor = "anchor"
	// shapeInfo adds one gauge series per engine, value 1, that carries the record.
	shapeInfo = "info"
	// shapePerFamily puts each value only on the families that it explains (vLLM).
	shapePerFamily = "per_family"
)

// emitAnchor puts the attributes on every data point of the anchor family. It
// reports whether the scrape has the family.
func emitAnchor(rm pmetric.ResourceMetrics, family string, attrs []attribute) bool {
	found := false
	forEachMetric(rm, func(_ pmetric.ScopeMetrics, m pmetric.Metric) bool {
		if m.Name() == family {
			found = true
			forEachDataPoint(m, func(dp pcommon.Map, _, _ pcommon.Timestamp) { putAttributes(dp, attrs) })
		}
		return true
	})
	return found
}

// emitInfo adds the Info metric: one gauge data point with value 1 and the
// attributes, in the scope of the anchor data point and with its timestamps, else
// in the scope of the newest data point of the scrape with its timestamp.
func emitInfo(rm pmetric.ResourceMetrics, family string, attrs []attribute) {
	var scope pmetric.ScopeMetrics
	var ts, start pcommon.Timestamp
	found, anchored, exists := false, false, false
	forEachMetric(rm, func(sm pmetric.ScopeMetrics, m pmetric.Metric) bool {
		if m.Name() == infoMetricName {
			exists = true // never add a second one
			return false
		}
		isAnchor := m.Name() == family
		forEachDataPoint(m, func(_ pcommon.Map, t, s pcommon.Timestamp) {
			switch {
			case isAnchor && !anchored:
				scope, ts, start, found, anchored = sm, t, s, true, true
			case !anchored && (!found || t > ts):
				scope, ts, found = sm, t, true
			}
		})
		return true
	})
	if exists || !found {
		return
	}
	m := scope.Metrics().AppendEmpty()
	m.SetName(infoMetricName)
	dp := m.SetEmptyGauge().DataPoints().AppendEmpty()
	dp.SetDoubleValue(1)
	dp.SetTimestamp(ts)
	dp.SetStartTimestamp(start)
	putAttributes(dp.Attributes(), attrs)
}

// emitPerFamily puts the configured and effective values of each vLLM family on
// its data points, by the attribution map of families.go.
func emitPerFamily(rm pmetric.ResourceMetrics, r record) {
	forEachMetric(rm, func(_ pmetric.ScopeMetrics, m pmetric.Metric) bool {
		names := valuesForMetric(m.Name())
		if len(names) == 0 {
			return true
		}
		var attrs []attribute
		for _, name := range names {
			if v, ok := r.configured[name]; ok {
				attrs = append(attrs, attribute{configuredKey(r.engine, name), v})
			}
			if v, ok := r.effective[name]; ok {
				attrs = append(attrs, attribute{effectiveKey(r.engine, name), v})
			}
		}
		forEachDataPoint(m, func(dp pcommon.Map, _, _ pcommon.Timestamp) { putAttributes(dp, attrs) })
		return true
	})
}

// putAttributes never overwrites an attribute that is already present.
func putAttributes(dst pcommon.Map, attrs []attribute) {
	for _, a := range attrs {
		if _, exists := dst.Get(a.key); !exists {
			dst.PutStr(a.key, a.value)
		}
	}
}

// forEachMetric calls fn for each metric of the scrape until fn returns false.
func forEachMetric(rm pmetric.ResourceMetrics, fn func(pmetric.ScopeMetrics, pmetric.Metric) bool) {
	sms := rm.ScopeMetrics()
	for i := 0; i < sms.Len(); i++ {
		ms := sms.At(i).Metrics()
		for j := 0; j < ms.Len(); j++ {
			if !fn(sms.At(i), ms.At(j)) {
				return
			}
		}
	}
}

// forEachDataPoint calls fn with the attributes, timestamp and start timestamp of
// each data point.
func forEachDataPoint(m pmetric.Metric, fn func(attrs pcommon.Map, ts, start pcommon.Timestamp)) {
	switch m.Type() {
	case pmetric.MetricTypeGauge:
		dps := m.Gauge().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			fn(dps.At(i).Attributes(), dps.At(i).Timestamp(), dps.At(i).StartTimestamp())
		}
	case pmetric.MetricTypeSum:
		dps := m.Sum().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			fn(dps.At(i).Attributes(), dps.At(i).Timestamp(), dps.At(i).StartTimestamp())
		}
	case pmetric.MetricTypeHistogram:
		dps := m.Histogram().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			fn(dps.At(i).Attributes(), dps.At(i).Timestamp(), dps.At(i).StartTimestamp())
		}
	case pmetric.MetricTypeExponentialHistogram:
		dps := m.ExponentialHistogram().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			fn(dps.At(i).Attributes(), dps.At(i).Timestamp(), dps.At(i).StartTimestamp())
		}
	case pmetric.MetricTypeSummary:
		dps := m.Summary().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			fn(dps.At(i).Attributes(), dps.At(i).Timestamp(), dps.At(i).StartTimestamp())
		}
	}
}
