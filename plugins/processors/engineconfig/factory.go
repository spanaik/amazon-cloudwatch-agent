// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package engineconfig

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processorhelper"
)

const (
	stability = component.StabilityLevelDevelopment

	defaultTimeout         = 3 * time.Second
	defaultRefreshInterval = 10 * time.Minute
	defaultRetryInterval   = time.Minute
	defaultLogRoot         = "/var/log/pods"
	defaultLogReadLimit    = 1 << 20
)

var (
	TypeStr, _            = component.NewType("engineconfig")
	processorCapabilities = consumer.Capabilities{MutatesData: true}
)

func NewFactory() processor.Factory {
	return processor.NewFactory(
		TypeStr,
		createDefaultConfig,
		processor.WithMetrics(createMetricsProcessor, stability))
}

// createDefaultConfig has no engine: the engine is required.
func createDefaultConfig() component.Config {
	return &Config{
		Timeout:         defaultTimeout,
		RefreshInterval: defaultRefreshInterval,
		RetryInterval:   defaultRetryInterval,
		Shapes:          []string{shapeAnchor},
		LogRoot:         defaultLogRoot,
		LogReadLimit:    defaultLogReadLimit,
	}
}

func createMetricsProcessor(
	ctx context.Context,
	set processor.Settings,
	cfg component.Config,
	nextConsumer consumer.Metrics,
) (processor.Metrics, error) {
	processorConfig, ok := cfg.(*Config)
	if !ok {
		return nil, fmt.Errorf("configuration parsing error")
	}

	p, err := newEngineConfigProcessor(processorConfig, set.Logger)
	if err != nil {
		return nil, err
	}

	return processorhelper.NewMetrics(
		ctx,
		set,
		cfg,
		nextConsumer,
		p.processMetrics,
		processorhelper.WithCapabilities(processorCapabilities),
		processorhelper.WithStart(p.start),
		processorhelper.WithShutdown(p.shutdown))
}
