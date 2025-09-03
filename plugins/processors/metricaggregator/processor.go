// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package metricaggregator

import (
    "context"
    "go.opentelemetry.io/collector/pdata/pmetric"
    "go.opentelemetry.io/collector/pdata/pcommon"
    "go.uber.org/zap"
    "math"
    "strings"
)

type metricAggregatorProcessor struct {
    logger *zap.Logger
    config *Config
}

// metricValues holds the values for a specific metric name and attribute combination
type metricValues struct {
    histograms []pmetric.HistogramDataPoint
    gauges     []pmetric.NumberDataPoint
    sums       []pmetric.NumberDataPoint
}

func newMetricAggregatorProcessor(config *Config, logger *zap.Logger) *metricAggregatorProcessor {
    return &metricAggregatorProcessor{
        logger: logger,
        config: config,
    }
}

// getAggregationGroupKey checks if a ResourceMetric should be aggregated and returns the group key
func (p *metricAggregatorProcessor) getAggregationGroupKey(resource pcommon.Resource) string {
    for groupName, attributes := range p.config.AggregationGroups {
        allMatch := true
        for key, value := range attributes {
            if attrValue, exists := resource.Attributes().Get(key); !exists || attrValue.AsString() != value {
                allMatch = false
                break
            }
        }
        if allMatch {
            return groupName
        }
    }
    return "" // No aggregation
}

func (p *metricAggregatorProcessor) processMetrics(_ context.Context, md pmetric.Metrics) (pmetric.Metrics, error) {
    aggregationGroups := make(map[string]map[string]map[string]*metricValues)
    baselineResources := make(map[string]pcommon.Resource)
    
    newMetrics := pmetric.NewMetrics()
    
    for i := 0; i < md.ResourceMetrics().Len(); i++ {
        rm := md.ResourceMetrics().At(i)
        groupKey := p.getAggregationGroupKey(rm.Resource())
        
        if groupKey != "" {
            // This ResourceMetric should be aggregated
            if _, exists := baselineResources[groupKey]; !exists {
                // First ResourceMetric for this group - set as baseline
                baselineResources[groupKey] = rm.Resource()
                aggregationGroups[groupKey] = make(map[string]map[string]*metricValues)
            }
            
            // Add all metrics from this ResourceMetric to aggregation
            p.addResourceMetricToGroup(rm, aggregationGroups[groupKey])
        } else {
            // Pass through unchanged
            newRM := newMetrics.ResourceMetrics().AppendEmpty()
            rm.CopyTo(newRM)
        }
    }
    
    // Create aggregated ResourceMetrics
    for groupKey, metricGroups := range aggregationGroups {
        newRM := newMetrics.ResourceMetrics().AppendEmpty()
        baselineResources[groupKey].CopyTo(newRM.Resource())
        
        newSM := newRM.ScopeMetrics().AppendEmpty()
        p.addMetricsToSM(metricGroups, newSM)
    }
    
    return newMetrics, nil
}

func (p *metricAggregatorProcessor) addResourceMetricToGroup(rm pmetric.ResourceMetrics, groupMetrics map[string]map[string]*metricValues) {
    for j := 0; j < rm.ScopeMetrics().Len(); j++ {
        sm := rm.ScopeMetrics().At(j)
        for k := 0; k < sm.Metrics().Len(); k++ {
            metric := sm.Metrics().At(k)
            metricName := metric.Name()
            
            // Initialize the map for this metric name if it doesn't exist
            if _, ok := groupMetrics[metricName]; !ok {
                groupMetrics[metricName] = make(map[string]*metricValues)
            }
            
            // Process based on data type
            switch metric.Type() {
            case pmetric.MetricTypeHistogram:
                p.processHistogram(metric, groupMetrics[metricName])
            case pmetric.MetricTypeGauge:
                p.processGauge(metric, groupMetrics[metricName])
            case pmetric.MetricTypeSum:
                p.processSum(metric, groupMetrics[metricName])
            }
        }
    }
}

func (p *metricAggregatorProcessor) addMetricsToSM(metricGroups map[string]map[string]*metricValues, newSM pmetric.ScopeMetrics) {
    // Process each metric name
    for metricName, attrMap := range metricGroups {
        // Process each attribute combination
        for _, values := range attrMap {
            // Process histograms
            if len(values.histograms) > 0 {
                p.aggregateHistograms(metricName, values.histograms, newSM)
            }
            
            // Process gauges
            if len(values.gauges) > 0 {
                p.aggregateGaugesToExponentialHistogram(metricName, values.gauges, newSM)
            }
            
            // Process sums
            if len(values.sums) > 0 {
                p.aggregateSumsToExponentialHistogram(metricName, values.sums, newSM)
            }
        }
    }
}

// getAttributeHashWithMinute generates a string key from attributes for grouping
func getAttributeHash(attrs pcommon.Map) string {
    var key strings.Builder
    attrs.Range(func(k string, v pcommon.Value) bool {
        key.WriteString(k)
        key.WriteString("=")
        key.WriteString(v.AsString())
        key.WriteString(";")
        return true
    })
    return key.String()
}

// processHistogram processes a histogram metric
func (p *metricAggregatorProcessor) processHistogram(metric pmetric.Metric, attrMap map[string]*metricValues) {
    hist := metric.Histogram()
    
    for i := 0; i < hist.DataPoints().Len(); i++ {
        dp := hist.DataPoints().At(i)
        attrHash := getAttributeHash(dp.Attributes())
        
        if _, ok := attrMap[attrHash]; !ok {
            attrMap[attrHash] = &metricValues{}
        }
        
        // Store the histogram data point
        attrMap[attrHash].histograms = append(attrMap[attrHash].histograms, dp)
    }
}

// processGauge processes a gauge metric
func (p *metricAggregatorProcessor) processGauge(metric pmetric.Metric, attrMap map[string]*metricValues) {
    gauge := metric.Gauge()
    
    for i := 0; i < gauge.DataPoints().Len(); i++ {
        dp := gauge.DataPoints().At(i)
        attrHash := getAttributeHash(dp.Attributes())
        
        if _, ok := attrMap[attrHash]; !ok {
            attrMap[attrHash] = &metricValues{}
        }
        
        // Store the gauge data point
        attrMap[attrHash].gauges = append(attrMap[attrHash].gauges, dp)
    }
}

// processSum processes a sum metric
func (p *metricAggregatorProcessor) processSum(metric pmetric.Metric, attrMap map[string]*metricValues) {
    sum := metric.Sum()
    
    for i := 0; i < sum.DataPoints().Len(); i++ {
        dp := sum.DataPoints().At(i)
        attrHash := getAttributeHash(dp.Attributes())
        
        if _, ok := attrMap[attrHash]; !ok {
            attrMap[attrHash] = &metricValues{}
        }
        
        // Store the sum data point
        attrMap[attrHash].sums = append(attrMap[attrHash].sums, dp)
    }
}


// aggregateHistograms aggregates histograms with the same attributes
func (p *metricAggregatorProcessor) aggregateHistograms(
    metricName string, 
    histograms []pmetric.HistogramDataPoint, 
    newSM pmetric.ScopeMetrics) {
    
    if len(histograms) == 0 {
        return
    }
    
    newMetric := newSM.Metrics().AppendEmpty()
    newMetric.SetName(metricName)
    
    newHist := newMetric.SetEmptyHistogram()

    newHist.SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
    
    newDP := newHist.DataPoints().AppendEmpty()
    
    histograms[0].Attributes().CopyTo(newDP.Attributes())
    
    newDP.SetStartTimestamp(histograms[0].StartTimestamp())
    newDP.SetTimestamp(histograms[0].Timestamp())
    
    newDP.SetCount(0)
    newDP.SetSum(0)
    
    explicitBounds := histograms[0].ExplicitBounds()
    newDP.ExplicitBounds().FromRaw(explicitBounds.AsRaw())
    
    newDP.BucketCounts().FromRaw(make([]uint64, len(explicitBounds.AsRaw())+1))
    
    // Aggregate data from all histograms
    for _, dp := range histograms {
        // Add count and sum
        newDP.SetCount(newDP.Count() + dp.Count())
        newDP.SetSum(newDP.Sum() + dp.Sum())
        
        // Add bucket counts
        for j := 0; j < dp.BucketCounts().Len(); j++ {
            newDP.BucketCounts().SetAt(j, newDP.BucketCounts().At(j) + dp.BucketCounts().At(j))
        }
        
        // Update min/max if available
        if dp.HasMin() {
            if !newDP.HasMin() || dp.Min() < newDP.Min() {
                newDP.SetMin(dp.Min())
            }
        }
        if dp.HasMax() {
            if !newDP.HasMax() || dp.Max() > newDP.Max() {
                newDP.SetMax(dp.Max())
            }
        }
    }
}

func (p *metricAggregatorProcessor) aggregateGaugesToExponentialHistogram(
    metricName string, 
    gauges []pmetric.NumberDataPoint, 
    newSM pmetric.ScopeMetrics) {
    
    if len(gauges) == 0 {
        return
    }
    
    newMetric := newSM.Metrics().AppendEmpty()
    newMetric.SetName(metricName)
    
    newExpHist := newMetric.SetEmptyExponentialHistogram()
    newExpHist.SetAggregationTemporality(pmetric.AggregationTemporalityDelta)
    
    newDP := newExpHist.DataPoints().AppendEmpty()
    
    gauges[0].Attributes().CopyTo(newDP.Attributes())
    newDP.SetStartTimestamp(gauges[0].StartTimestamp())
    newDP.SetTimestamp(gauges[0].Timestamp())
    newDP.SetScale(5) // Default scale
    
    // Collect all values and calculate bucket indices
    var values []float64
    var bucketIndices []int
    var min, max, sum float64
    var count uint64
    var zeroCount uint64
    hasMin := false
    
    for _, dp := range gauges {
        val := dp.DoubleValue()
        
        if val == 0 {
            zeroCount++
        } else {
            values = append(values, val)
            bucketIndices = append(bucketIndices, p.determineBucketIndex(val, newDP.Scale()))
        }
        
        // Update min/max/sum
        if !hasMin || val < min {
            min = val
            hasMin = true
        }
        if val > max {
            max = val
        }
        sum += val
        count++
    }
    
    // Set basic stats
    newDP.SetMin(min)
    newDP.SetMax(max)
    newDP.SetSum(sum)
    newDP.SetCount(count)
    newDP.SetZeroCount(zeroCount)
    
    // Build exponential histogram buckets
    if len(bucketIndices) > 0 {
        p.buildExponentialHistogramBuckets(bucketIndices, newDP)
    }
}

// aggregateSumsToExponentialHistogram aggregates sums to an exponential histogram
// Implementation is similar to aggregateGaugesToExponentialHistogram
func (p *metricAggregatorProcessor) aggregateSumsToExponentialHistogram(
    metricName string, 
    sums []pmetric.NumberDataPoint, 
    newSM pmetric.ScopeMetrics) {
    
    // Implementation is almost identical to aggregateGaugesToExponentialHistogram, reusing the same logic
    p.aggregateGaugesToExponentialHistogram(metricName, sums, newSM)
}

// buildExponentialHistogramBuckets creates the bucket structure for exponential histogram
func (p *metricAggregatorProcessor) buildExponentialHistogramBuckets(bucketIndices []int, dp pmetric.ExponentialHistogramDataPoint) {
    if len(bucketIndices) == 0 {
        return
    }
    
    // Find min and max bucket indices
    minIndex := bucketIndices[0]
    maxIndex := bucketIndices[0]
    for _, idx := range bucketIndices {
        if idx < minIndex {
            minIndex = idx
        }
        if idx > maxIndex {
            maxIndex = idx
        }
    }
    
    // Set offset and create bucket counts array
    offset := minIndex
    arraySize := maxIndex - minIndex + 1
    bucketCounts := make([]uint64, arraySize)
    
    // Count values in each bucket
    for _, idx := range bucketIndices {
        arrayIndex := idx - offset
        bucketCounts[arrayIndex]++
    }
    
    // Set the buckets in the data point
    dp.Positive().SetOffset(int32(offset))
    dp.Positive().BucketCounts().FromRaw(bucketCounts)
}

// determineBucketIndex calculates the bucket index for a value in an exponential histogram
func (p *metricAggregatorProcessor) determineBucketIndex(value float64, scale int32) int {
    if value <= 0 {
        return 0
    }
    // For exponential histograms: bucket = floor(log2(value) * 2^scale)
    return int(math.Floor(math.Log2(value) * math.Pow(2, float64(scale))))
}