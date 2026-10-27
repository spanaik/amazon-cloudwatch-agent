// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package vllmconfig

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

	defaultRefreshInterval = 10 * time.Minute
	defaultTimeout         = 3 * time.Second
)

var (
	TypeStr, _            = component.NewType("vllmconfig")
	processorCapabilities = consumer.Capabilities{MutatesData: true}
)

func NewFactory() processor.Factory {
	return processor.NewFactory(
		TypeStr,
		createDefaultConfig,
		processor.WithMetrics(createMetricsProcessor, stability))
}

func createDefaultConfig() component.Config {
	return &Config{
		RefreshInterval: defaultRefreshInterval,
		Timeout:         defaultTimeout,
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

	p := newVLLMConfigProcessor(processorConfig, set.Logger)

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
