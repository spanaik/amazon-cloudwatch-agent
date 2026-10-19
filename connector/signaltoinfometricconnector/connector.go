// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package signaltoinfometricconnector

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/connector"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/pdata/plog"
	"go.uber.org/zap"
)

// infoMetricConnector is a placeholder that accepts logs and emits nothing.
// TODO: compile rules, track state per entity, and emit Info metrics.
type infoMetricConnector struct {
	logger *zap.Logger
	cfg    *Config
	next   consumer.Metrics
}

var _ connector.Logs = (*infoMetricConnector)(nil)

func newConnector(set connector.Settings, cfg *Config, next consumer.Metrics) *infoMetricConnector {
	return &infoMetricConnector{
		logger: set.Logger,
		cfg:    cfg,
		next:   next,
	}
}

func (c *infoMetricConnector) Start(context.Context, component.Host) error {
	c.logger.Debug("Starting signal_to_info_metric connector", zap.Int("rules", len(c.cfg.Rules)))
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

func (c *infoMetricConnector) ConsumeLogs(context.Context, plog.Logs) error {
	return nil
}
