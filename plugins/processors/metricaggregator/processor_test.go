// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package metricaggregator

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"
)

func TestProcessMetrics_EmptyConfig(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	processor := newMetricAggregatorProcessor(&Config{}, logger)
	ctx := context.Background()

	input := generateTestMetrics("test_metric", []map[string]string{
		{"service.name": "service1"},
		{"service.name": "service2"},
	})

	output, err := processor.processMetrics(ctx, input)
	assert.NoError(t, err)
	assert.Equal(t, 2, output.ResourceMetrics().Len())
}

func TestProcessMetrics_WithAggregation(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	config := &Config{
		AggregationGroups: map[string]map[string]string{
			"api_group": {
				"service.name": "api-server",
			},
		},
	}
	processor := newMetricAggregatorProcessor(config, logger)
	ctx := context.Background()

	input := generateTestMetrics("api_requests_total", []map[string]string{
		{"service.name": "api-server", "instance.id": "1"},
		{"service.name": "api-server", "instance.id": "2"},
		{"service.name": "other-service"},
	})

	output, err := processor.processMetrics(ctx, input)
	assert.NoError(t, err)
	// Should have 2 ResourceMetrics: 1 aggregated + 1 passthrough
	assert.Equal(t, 2, output.ResourceMetrics().Len())
}

func TestProcessMetrics_GaugeToExponentialHistogramAggregation(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	config := &Config{
		AggregationGroups: map[string]map[string]string{
			"kube_group": {
				"service.name": "containerInsightsKubeAPIServerScraper",
			},
		},
	}
	processor := newMetricAggregatorProcessor(config, logger)
	ctx := context.Background()

	// Create input with 2 control plane nodes with GAUGE metrics
	input := pmetric.NewMetrics()

	// Node 1 - gauge value 100 (should convert to exp histogram bucket 64-128)
	rm1 := input.ResourceMetrics().AppendEmpty()
	rm1.Resource().Attributes().PutStr("service.name", "containerInsightsKubeAPIServerScraper")
	rm1.Resource().Attributes().PutStr("host.name", "control-plane-1")
	sm1 := rm1.ScopeMetrics().AppendEmpty()
	metric1 := sm1.Metrics().AppendEmpty()
	metric1.SetName("apiserver_request_duration")
	gauge1 := metric1.SetEmptyGauge()
	dp1 := gauge1.DataPoints().AppendEmpty()
	dp1.SetDoubleValue(100.0)
	dp1.Attributes().PutStr("method", "GET")

	// Node 2 - gauge value 150 (should convert to exp histogram bucket 128-256)
	rm2 := input.ResourceMetrics().AppendEmpty()
	rm2.Resource().Attributes().PutStr("service.name", "containerInsightsKubeAPIServerScraper")
	rm2.Resource().Attributes().PutStr("host.name", "control-plane-2")
	sm2 := rm2.ScopeMetrics().AppendEmpty()
	metric2 := sm2.Metrics().AppendEmpty()
	metric2.SetName("apiserver_request_duration")
	gauge2 := metric2.SetEmptyGauge()
	dp2 := gauge2.DataPoints().AppendEmpty()
	dp2.SetDoubleValue(150.0)
	dp2.Attributes().PutStr("method", "GET")

	// Other service - should pass through unchanged
	rm3 := input.ResourceMetrics().AppendEmpty()
	rm3.Resource().Attributes().PutStr("service.name", "other-service")
	sm3 := rm3.ScopeMetrics().AppendEmpty()
	metric3 := sm3.Metrics().AppendEmpty()
	metric3.SetName("other_metric")
	gauge3 := metric3.SetEmptyGauge()
	dp3 := gauge3.DataPoints().AppendEmpty()
	dp3.SetIntValue(50)

	output, err := processor.processMetrics(ctx, input)
	assert.NoError(t, err)

	// Verify output structure
	assert.Equal(t, 2, output.ResourceMetrics().Len())

	// Find aggregated and passthrough ResourceMetrics
	var aggregatedRM, passthroughRM pmetric.ResourceMetrics
	for i := 0; i < output.ResourceMetrics().Len(); i++ {
		rm := output.ResourceMetrics().At(i)
		if serviceName, exists := rm.Resource().Attributes().Get("service.name"); exists {
			if serviceName.AsString() == "containerInsightsKubeAPIServerScraper" {
				aggregatedRM = rm
			} else {
				passthroughRM = rm
			}
		}
	}

	// Verify aggregated ResourceMetric
	assert.NotNil(t, aggregatedRM)
	assert.Equal(t, 1, aggregatedRM.ScopeMetrics().Len())
	assert.Equal(t, 1, aggregatedRM.ScopeMetrics().At(0).Metrics().Len())

	// Verify metric was converted from gauge to exponential histogram
	aggregatedMetric := aggregatedRM.ScopeMetrics().At(0).Metrics().At(0)
	assert.Equal(t, "apiserver_request_duration", aggregatedMetric.Name())
	assert.Equal(t, pmetric.MetricTypeExponentialHistogram, aggregatedMetric.Type())
	assert.Equal(t, 1, aggregatedMetric.ExponentialHistogram().DataPoints().Len())

	// Verify converted exponential histogram data point
	aggregatedDP := aggregatedMetric.ExponentialHistogram().DataPoints().At(0)
	assert.Equal(t, uint64(2), aggregatedDP.Count()) // 2 gauge values converted
	assert.Equal(t, 250.0, aggregatedDP.Sum()) // 100 + 150
	assert.Equal(t, "GET", aggregatedDP.Attributes().AsRaw()["method"])
	
	// Verify bucket structure - gauge values 100 and 150 in separate buckets
	assert.Equal(t, int32(6), aggregatedDP.Positive().Offset()) // Starts at bucket 6 (64-128)
	assert.Equal(t, 2, aggregatedDP.Positive().BucketCounts().Len()) // Two buckets
	assert.Equal(t, uint64(1), aggregatedDP.Positive().BucketCounts().At(0)) // Bucket 6: count 1 (value 100)
	assert.Equal(t, uint64(1), aggregatedDP.Positive().BucketCounts().At(1)) // Bucket 7: count 1 (value 150)

	// Verify baseline resource attributes (from first node)
	assert.Equal(t, "containerInsightsKubeAPIServerScraper", 
		aggregatedRM.Resource().Attributes().AsRaw()["service.name"])
	assert.Equal(t, "control-plane-1", 
		aggregatedRM.Resource().Attributes().AsRaw()["host.name"])

	// Verify passthrough ResourceMetric remains as gauge
	assert.NotNil(t, passthroughRM)
	assert.Equal(t, "other-service", 
		passthroughRM.Resource().Attributes().AsRaw()["service.name"])
	passthroughMetric := passthroughRM.ScopeMetrics().At(0).Metrics().At(0)
	assert.Equal(t, "other_metric", passthroughMetric.Name())
	assert.Equal(t, pmetric.MetricTypeGauge, passthroughMetric.Type()) // Still a gauge
}

func TestGetAggregationGroupKey(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	config := &Config{
		AggregationGroups: map[string]map[string]string{
			"group1": {
				"service.name": "test-service",
			},
		},
	}
	processor := newMetricAggregatorProcessor(config, logger)

	// Test matching
	md := generateTestMetrics("test", []map[string]string{{"service.name": "test-service"}})
	resource := md.ResourceMetrics().At(0).Resource()
	result := processor.getAggregationGroupKey(resource)
	assert.Equal(t, "group1", result)

	// Test non-matching
	md2 := generateTestMetrics("test", []map[string]string{{"service.name": "other-service"}})
	resource2 := md2.ResourceMetrics().At(0).Resource()
	result2 := processor.getAggregationGroupKey(resource2)
	assert.Equal(t, "", result2)
}

func TestHistogramAggregation(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	processor := newMetricAggregatorProcessor(&Config{}, logger)

	// Create two histogram data points to aggregate
	histograms := make([]pmetric.HistogramDataPoint, 2)
	
	// First histogram: count=50, sum=5000, buckets=[10, 20, 15, 5]
	dp1 := pmetric.NewHistogramDataPoint()
	dp1.SetCount(50)
	dp1.SetSum(5000)
	dp1.ExplicitBounds().FromRaw([]float64{100, 500, 1000})
	dp1.BucketCounts().FromRaw([]uint64{10, 20, 15, 5})
	histograms[0] = dp1

	// Second histogram: count=30, sum=3000, buckets=[5, 10, 10, 5]
	dp2 := pmetric.NewHistogramDataPoint()
	dp2.SetCount(30)
	dp2.SetSum(3000)
	dp2.ExplicitBounds().FromRaw([]float64{100, 500, 1000})
	dp2.BucketCounts().FromRaw([]uint64{5, 10, 10, 5})
	histograms[1] = dp2

	// Aggregate histograms
	newSM := pmetric.NewScopeMetrics()
	processor.aggregateHistograms("test_histogram", histograms, newSM)

	// Verify the result
	assert.Equal(t, 1, newSM.Metrics().Len())
	metric := newSM.Metrics().At(0)
	assert.Equal(t, pmetric.MetricTypeHistogram, metric.Type())
	
	aggregatedDP := metric.Histogram().DataPoints().At(0)
	
	// Verify aggregated counts and sum
	assert.Equal(t, uint64(80), aggregatedDP.Count()) // 50 + 30
	assert.Equal(t, 8000.0, aggregatedDP.Sum()) // 5000 + 3000
	
	// Verify explicit bounds are preserved
	expectedBounds := []float64{100, 500, 1000}
	assert.Equal(t, len(expectedBounds), aggregatedDP.ExplicitBounds().Len())
	for i, expected := range expectedBounds {
		assert.Equal(t, expected, aggregatedDP.ExplicitBounds().At(i))
	}
	
	// Verify bucket counts are summed: [10+5, 20+10, 15+10, 5+5] = [15, 30, 25, 10]
	expectedBuckets := []uint64{15, 30, 25, 10}
	assert.Equal(t, len(expectedBuckets), aggregatedDP.BucketCounts().Len())
	for i, expected := range expectedBuckets {
		assert.Equal(t, expected, aggregatedDP.BucketCounts().At(i), "bucket %d", i)
	}
}

func generateTestMetrics(metricName string, resourceAttrs []map[string]string) pmetric.Metrics {
	md := pmetric.NewMetrics()

	for _, attrs := range resourceAttrs {
		rm := md.ResourceMetrics().AppendEmpty()
		for k, v := range attrs {
			rm.Resource().Attributes().PutStr(k, v)
		}

		sm := rm.ScopeMetrics().AppendEmpty()
		metric := sm.Metrics().AppendEmpty()
		metric.SetName(metricName)
		gauge := metric.SetEmptyGauge()
		dp := gauge.DataPoints().AppendEmpty()
		dp.SetIntValue(100)
	}

	return md
}

func TestSumToExponentialHistogramAggregation(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	config := &Config{
		AggregationGroups: map[string]map[string]string{
			"test_group": {
				"service.name": "test-service",
			},
		},
	}
	processor := newMetricAggregatorProcessor(config, logger)
	ctx := context.Background()

	// Create input with sum metrics from multiple nodes
	input := pmetric.NewMetrics()
	testValues := []int64{50, 200, 1000}

	for i, val := range testValues {
		rm := input.ResourceMetrics().AppendEmpty()
		rm.Resource().Attributes().PutStr("service.name", "test-service")
		rm.Resource().Attributes().PutStr("instance.id", fmt.Sprintf("%d", i))
		sm := rm.ScopeMetrics().AppendEmpty()
		metric := sm.Metrics().AppendEmpty()
		metric.SetName("request_count_total")
		sum := metric.SetEmptySum()
		sum.SetIsMonotonic(true)
		sum.SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
		dp := sum.DataPoints().AppendEmpty()
		dp.SetDoubleValue(float64(val))
		dp.Attributes().PutStr("method", "GET")
	}

	output, err := processor.processMetrics(ctx, input)
	assert.NoError(t, err)
	assert.Equal(t, 1, output.ResourceMetrics().Len())

	// Verify aggregated sum converted to exponential histogram
	aggregatedRM := output.ResourceMetrics().At(0)
	assert.Equal(t, 1, aggregatedRM.ScopeMetrics().Len())
	assert.Equal(t, 1, aggregatedRM.ScopeMetrics().At(0).Metrics().Len())

	metric := aggregatedRM.ScopeMetrics().At(0).Metrics().At(0)
	assert.Equal(t, "request_count_total", metric.Name())
	assert.Equal(t, pmetric.MetricTypeExponentialHistogram, metric.Type())

	dp := metric.ExponentialHistogram().DataPoints().At(0)

	// Verify basic stats
	assert.Equal(t, uint64(3), dp.Count())
	assert.Equal(t, 1250.0, dp.Sum()) // 50 + 200 + 1000
	assert.Equal(t, 50.0, dp.Min())
	assert.Equal(t, 1000.0, dp.Max())
	assert.Equal(t, "GET", dp.Attributes().AsRaw()["method"])

	// Verify bucket structure
	// 50 -> bucket 5, 200 -> bucket 7, 1000 -> bucket 9
	assert.Equal(t, int32(5), dp.Positive().Offset())
	assert.Equal(t, 5, dp.Positive().BucketCounts().Len())

	// Verify bucket counts: [1, 0, 1, 0, 1] for buckets 5,6,7,8,9
	expectedCounts := []uint64{1, 0, 1, 0, 1}
	for i, expected := range expectedCounts {
		assert.Equal(t, expected, dp.Positive().BucketCounts().At(i))
	}
}
