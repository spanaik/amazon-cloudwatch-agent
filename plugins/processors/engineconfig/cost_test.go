// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package engineconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
)

// The cost harness replays each captured scrape (testdata/cost/<name>.prom) with
// its capture file (<name>.json: the resource and scope attributes of the stored
// series, the data point attributes that the agent adds, and the record of the
// pod) through each shape, and bills the stored data points: for each data point,
// len(key)+len(value) of every resource, scope and data point attribute. The
// resource attributes of the capture already hold the chart's workload keys, so
// the record adds none (emit_workload is off). Run with -v to see the table; with
// COST_OUT set, the table is also written there as JSON.

type costCapture struct {
	Resource     map[string]string `json:"resource"`
	ScopeName    string            `json:"scope_name"`
	ScopeVersion string            `json:"scope_version"`
	Scope        map[string]string `json:"scope"`
	Datapoint    map[string]string `json:"datapoint"`
	Record       struct {
		Engine     string            `json:"engine"`
		Version    string            `json:"version"`
		Generation string            `json:"generation"`
		Role       string            `json:"role"`
		GroupRole  string            `json:"group_role"`
		Workload   owner             `json:"workload"`
		Serving    owner             `json:"serving"`
		Model      string            `json:"model"`
		Command    []string          `json:"command"`
		Configured map[string]string `json:"configured"`
		Effective  map[string]string `json:"effective"`
	} `json:"record"`
}

func (c costCapture) record() record {
	r := c.Record
	return record{engine: r.Engine, version: r.Version, generation: r.Generation, role: r.Role, groupRole: r.GroupRole,
		workload: r.Workload, serving: r.Serving, model: r.Model, configured: r.Configured, effective: r.Effective}
}

// scrapeMetrics builds what the prometheus receiver gives for the kept families
// of the engine (the agent drops the _created gauges): one data point per sample
// of a counter or gauge, one per label set of a histogram or summary.
func scrapeMetrics(t *testing.T, path string, c costCapture) pmetric.Metrics {
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(f)
	require.NoError(t, err)

	md := pmetric.NewMetrics()
	rm := md.ResourceMetrics().AppendEmpty()
	for k, v := range c.Resource {
		rm.Resource().Attributes().PutStr(k, v)
	}
	sm := rm.ScopeMetrics().AppendEmpty()
	sm.Scope().SetName(c.ScopeName)
	sm.Scope().SetVersion(c.ScopeVersion)
	for k, v := range c.Scope {
		sm.Scope().Attributes().PutStr(k, v)
	}
	ts := pcommon.Timestamp(1_792_775_300_000_000_000)
	for _, name := range sortedFamilies(families) {
		if !strings.HasPrefix(name, c.Record.Engine+":") || strings.HasSuffix(name, "_created") {
			continue
		}
		fam := families[name]
		m := sm.Metrics().AppendEmpty()
		m.SetName(name)
		for _, s := range fam.GetMetric() {
			var attrs pcommon.Map
			switch fam.GetType() {
			case dto.MetricType_COUNTER:
				sum := m.Sum()
				if m.Type() != pmetric.MetricTypeSum {
					sum = m.SetEmptySum()
					sum.SetIsMonotonic(true)
					sum.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
				}
				dp := sum.DataPoints().AppendEmpty()
				dp.SetDoubleValue(s.GetCounter().GetValue())
				dp.SetTimestamp(ts)
				attrs = dp.Attributes()
			case dto.MetricType_HISTOGRAM, dto.MetricType_GAUGE_HISTOGRAM:
				h := m.Histogram()
				if m.Type() != pmetric.MetricTypeHistogram {
					h = m.SetEmptyHistogram()
					h.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
				}
				dp := h.DataPoints().AppendEmpty()
				dp.SetCount(s.GetHistogram().GetSampleCount())
				dp.SetSum(s.GetHistogram().GetSampleSum())
				dp.SetTimestamp(ts)
				attrs = dp.Attributes()
			case dto.MetricType_SUMMARY:
				sum := m.Summary()
				if m.Type() != pmetric.MetricTypeSummary {
					sum = m.SetEmptySummary()
				}
				dp := sum.DataPoints().AppendEmpty()
				dp.SetCount(s.GetSummary().GetSampleCount())
				dp.SetTimestamp(ts)
				attrs = dp.Attributes()
			default: // gauge, untyped
				g := m.Gauge()
				if m.Type() != pmetric.MetricTypeGauge {
					g = m.SetEmptyGauge()
				}
				dp := g.DataPoints().AppendEmpty()
				dp.SetDoubleValue(s.GetGauge().GetValue() + s.GetUntyped().GetValue())
				dp.SetTimestamp(ts)
				attrs = dp.Attributes()
			}
			for _, l := range s.GetLabel() {
				attrs.PutStr(l.GetName(), l.GetValue())
			}
			for k, v := range c.Datapoint {
				attrs.PutStr(k, v)
			}
		}
	}
	return md
}

func sortedFamilies(families map[string]*dto.MetricFamily) []string {
	keys := map[string]string{}
	for k := range families {
		keys[k] = k
	}
	return sortedKeys(keys)
}

func mapBytes(m pcommon.Map) int {
	n := 0
	m.Range(func(k string, v pcommon.Value) bool {
		n += len(k) + len(v.AsString())
		return true
	})
	return n
}

// billedBytes sums the attribute bytes of every stored data point of the scrape.
func billedBytes(md pmetric.Metrics) (bytes, datapoints int) {
	rms := md.ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		res := mapBytes(rms.At(i).Resource().Attributes())
		sms := rms.At(i).ScopeMetrics()
		for j := 0; j < sms.Len(); j++ {
			scope := mapBytes(sms.At(j).Scope().Attributes())
			ms := sms.At(j).Metrics()
			for k := 0; k < ms.Len(); k++ {
				forEachDataPoint(ms.At(k), func(attrs pcommon.Map, _, _ pcommon.Timestamp) {
					bytes += res + scope + mapBytes(attrs)
					datapoints++
				})
			}
		}
	}
	return bytes, datapoints
}

var costShapes = []struct {
	name  string
	apply func(rm pmetric.ResourceMetrics, r record, eng engine) bool // false: not for this engine
}{
	{"none", func(pmetric.ResourceMetrics, record, engine) bool { return true }},
	{"S1 per_family + resource_identity", func(rm pmetric.ResourceMetrics, r record, eng engine) bool {
		if eng.name() != engineVLLM {
			return false
		}
		emitPerFamily(rm, r)
		putAttributes(rm.Resource().Attributes(), r.identity())
		return true
	}},
	{"S2 full record on every data point", func(rm pmetric.ResourceMetrics, r record, _ engine) bool {
		forEachMetric(rm, func(_ pmetric.ScopeMetrics, m pmetric.Metric) bool {
			forEachDataPoint(m, func(attrs pcommon.Map, _, _ pcommon.Timestamp) { putAttributes(attrs, r.attributes(false)) })
			return true
		})
		return true
	}},
	{"S3 info", func(rm pmetric.ResourceMetrics, r record, eng engine) bool {
		emitInfo(rm, eng.anchorFamily(), r.attributes(false))
		return true
	}},
	{"S4 anchor", func(rm pmetric.ResourceMetrics, r record, eng engine) bool {
		return emitAnchor(rm, eng.anchorFamily(), r.attributes(false))
	}},
}

type costRow struct {
	Capture    string  `json:"capture"`
	Shape      string  `json:"shape"`
	Datapoints int     `json:"datapoints"`
	Bytes      int     `json:"bytes_per_scrape"`
	Delta      int     `json:"delta_bytes"`
	Percent    float64 `json:"delta_percent"`
}

func TestCost(t *testing.T) {
	files, err := filepath.Glob("testdata/cost/*.prom")
	require.NoError(t, err)
	require.NotEmpty(t, files)
	var rows []costRow
	for _, path := range files {
		name := strings.TrimSuffix(filepath.Base(path), ".prom")
		raw, err := os.ReadFile(strings.TrimSuffix(path, ".prom") + ".json")
		require.NoError(t, err, "a scrape needs its capture file")
		var c costCapture
		require.NoError(t, json.Unmarshal(raw, &c))
		eng, err := newEngine(c.Record.Engine, nil)
		require.NoError(t, err)
		base := scrapeMetrics(t, path, c)
		r := c.record()

		// the fixed record is what the sources give: the parser on the command
		// line, and the engine metrics of the same scrape
		if len(c.Record.Command) > 0 {
			res := parseContainerArgs(containerSpec{Command: c.Record.Command}, eng.name())
			require.True(t, res.found)
			configured := map[string]string{}
			for k, v := range eng.configured(containerInfo{Args: res.fields}) {
				configured[k] = v.value
			}
			assert.Equal(t, r.configured, configured, name)
		}
		for k, v := range eng.scraped(base.ResourceMetrics().At(0)) {
			if internalValues[k] {
				continue // identity or phase, not an effective value of the record
			}
			assert.Equal(t, v.value, r.effective[k], name+" "+k)
		}

		noneBytes, _ := billedBytes(base)
		for _, s := range costShapes {
			md := pmetric.NewMetrics()
			base.CopyTo(md)
			if !s.apply(md.ResourceMetrics().At(0), r, eng) {
				continue
			}
			bytes, dps := billedBytes(md)
			rows = append(rows, costRow{Capture: name, Shape: s.name, Datapoints: dps, Bytes: bytes,
				Delta: bytes - noneBytes, Percent: 100 * float64(bytes-noneBytes) / float64(noneBytes)})
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\n%-16s %-36s %10s %16s %12s %9s\n", "capture", "shape", "datapoints", "bytes/scrape", "delta", "percent")
	for _, row := range rows {
		fmt.Fprintf(&b, "%-16s %-36s %10d %16d %12d %8.2f%%\n", row.Capture, row.Shape, row.Datapoints, row.Bytes, row.Delta, row.Percent)
	}
	t.Log(b.String())
	if out := os.Getenv("COST_OUT"); out != "" {
		data, err := json.MarshalIndent(rows, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(out, append(data, '\n'), 0o600))
	}

	// the shapes keep their order of cost
	for i := 0; i+len(costShapes) <= len(rows); i += len(costShapes) {
		if rows[i+1].Shape != costShapes[1].name {
			continue
		}
		none, s1, s2, s3, s4 := rows[i], rows[i+1], rows[i+2], rows[i+3], rows[i+4]
		assert.Zero(t, none.Delta)
		assert.Equal(t, none.Datapoints+1, s3.Datapoints)
		assert.Less(t, s4.Delta, s3.Delta)
		assert.Less(t, s3.Delta, s1.Delta)
		assert.Less(t, s1.Delta, s2.Delta)
	}
}
