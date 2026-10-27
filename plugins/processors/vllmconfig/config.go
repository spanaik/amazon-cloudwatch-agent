// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package vllmconfig

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"go.opentelemetry.io/collector/component"
)

// Config of the vllmconfig processor. It uses only plain types, because the
// agent re-encodes the merged OTel configuration with internal/mapstructure at
// start and exits on an error.
type Config struct {
	// NodeName limits the pod watch to the pods of this node. Empty means the
	// K8S_NODE_NAME environment variable.
	NodeName string `mapstructure:"node_name"`
	// RefreshInterval is how often the vLLM HTTP API of a pod is read again.
	RefreshInterval time.Duration `mapstructure:"refresh_interval"`
	// Timeout bounds the first, synchronous read of a pod's HTTP API.
	Timeout time.Duration `mapstructure:"timeout"`
	// EnvMap is a PoC-only env contract: env var name -> value name (for example
	// MAX_MODEL_LEN: max_model_len), for images that read engine values from
	// their own env vars instead of vLLM flags. Only literal values are read.
	EnvMap map[string]string `mapstructure:"env_map"`
}

var _ component.Config = (*Config)(nil)

func (cfg *Config) Validate() error {
	var errs []error
	if cfg.RefreshInterval <= 0 {
		errs = append(errs, fmt.Errorf("refresh_interval must be positive, got %v", cfg.RefreshInterval))
	}
	if cfg.Timeout <= 0 {
		errs = append(errs, fmt.Errorf("timeout must be positive, got %v", cfg.Timeout))
	}
	names := make([]string, 0, len(cfg.EnvMap))
	for name := range cfg.EnvMap {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if name == "" {
			errs = append(errs, errors.New("env_map has an empty env var name"))
		}
		if value := cfg.EnvMap[name]; !envMappable[value] {
			errs = append(errs, fmt.Errorf("env_map[%q]: unknown value name %q", name, value))
		}
	}
	return errors.Join(errs...)
}
