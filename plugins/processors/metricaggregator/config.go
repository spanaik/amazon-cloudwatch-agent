// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package metricaggregator

import (
    "go.opentelemetry.io/collector/component"
)

// Config holds the configuration for the metricaggregator processor.
type Config struct {
    // AggregationGroups specifies resource attribute patterns for aggregation
    // Each group aggregates ResourceMetrics with matching attributes
    AggregationGroups map[string]map[string]string `mapstructure:"aggregation_groups"`
}


// Verify Config implements component.Config interface.
var _ component.Config = (*Config)(nil)

// Validate validates the processor configuration.
func (cfg *Config) Validate() error {
    return nil
}