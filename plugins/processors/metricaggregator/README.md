# Metric Aggregator Processor

The Metric Aggregator Processor is an OpenTelemetry Collector processor that aggregates metrics from multiple resource instances based on configurable resource attribute patterns. It consolidates metrics from different sources (e.g., multiple Kubernetes control plane nodes) into unified metric representations.

## Overview

This processor groups ResourceMetrics that match specified resource attribute patterns and aggregates their metrics. It transforms different metric types during aggregation:

- **Histograms**: Aggregated by summing counts, sums, and bucket counts
- **Gauges**: Converted to exponential histograms with bucket distribution
- **Sums**: Converted to exponential histograms with bucket distribution

## Configuration

### Basic Structure

```yaml
processors:
  metricaggregator:
    aggregation_groups:
      <group_name>:
        <attribute_key>: <attribute_value>
        # ... additional attribute patterns
```

### Configuration Parameters

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `aggregation_groups` | map[string]map[string]string | No | Defines resource attribute patterns for grouping metrics |

#### aggregation_groups

A map where:
- **Key**: Group name (arbitrary identifier)
- **Value**: Map of resource attribute key-value pairs that must match for aggregation

ResourceMetrics are aggregated into the same group if **all** specified attributes match exactly.

### Example Configuration

```yaml
processors:
  metricaggregator:
    aggregation_groups:
      # Aggregate all Kubernetes API server metrics regardless of node
      kube_api_group:
        service.name: "containerInsightsKubeAPIServerScraper"
      
      # Aggregate metrics from specific service instances
      api_service_group:
        service.name: "api-server"
        environment: "production"
      
      # Aggregate database metrics from replica set
      db_replica_group:
        service.name: "postgres"
        cluster.name: "main-cluster"
```

## Processing Logic

### 1. Resource Grouping

The processor examines each ResourceMetric and:
1. Checks if its resource attributes match any configured aggregation group
2. Groups matching ResourceMetrics together
3. Passes through non-matching ResourceMetrics unchanged

### 2. Metric Aggregation

Within each group, metrics are aggregated by:
1. **Metric name**: Metrics with the same name are combined
2. **Attribute hash**: Data points with identical attributes are aggregated together

### 3. Type-Specific Processing

#### Histogram Aggregation
- Combines multiple histogram data points
- Sums: count, sum, and bucket counts
- Preserves: explicit bounds structure
- Updates: min/max values appropriately

#### Gauge → Exponential Histogram
- Converts gauge values to exponential histogram buckets
- Uses scale factor of 5 (default)
- Calculates bucket indices using: `floor(log2(value) * 2^scale)`
- Tracks: count, sum, min, max, zero count

#### Sum → Exponential Histogram  
- Same conversion logic as gauges
- Preserves monotonic and temporality properties

### 4. Output Structure

The processor outputs:
- **Aggregated ResourceMetrics**: One per aggregation group with combined metrics
- **Passthrough ResourceMetrics**: Unchanged metrics that don't match any group
- **Baseline attributes**: Uses attributes from the first ResourceMetric in each group

## Use Cases

### Kubernetes Control Plane Aggregation
```yaml
aggregation_groups:
  kube_control_plane:
    service.name: "containerInsightsKubeAPIServerScraper"
```
Aggregates metrics from multiple control plane nodes into unified histograms.

### Multi-Instance Service Aggregation
```yaml
aggregation_groups:
  web_service:
    service.name: "web-api"
    environment: "prod"
```
Combines metrics from multiple service instances for cluster-level visibility.

### Database Replica Aggregation
```yaml
aggregation_groups:
  db_cluster:
    service.name: "postgresql"
    cluster.id: "primary"
```
Aggregates database metrics across replica instances.

## Behavior Notes

1. **Attribute Preservation**: The first ResourceMetric in each group provides baseline resource attributes
2. **Metric Transformation**: Gauges and sums are converted to exponential histograms

## Example Input/Output

### Input
```
ResourceMetric 1: {service.name: "api-server", instance.id: "1"}
  - gauge "response_time": 100ms
  
ResourceMetric 2: {service.name: "api-server", instance.id: "2"}  
  - gauge "response_time": 150ms
```

### Configuration
```yaml
aggregation_groups:
  api_group:
    service.name: "api-server"
```

### Output
```
ResourceMetric: {service.name: "api-server", instance.id: "1"}
  - exponential_histogram "response_time": 
    count: 2, sum: 250ms, buckets: [1@100ms, 1@150ms]
```
