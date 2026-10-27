// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package vllmconfig

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"

	"github.com/aws/amazon-cloudwatch-agent/internal/mapstructure"
)

func TestUnmarshalDefaultConfig(t *testing.T) {
	factory := NewFactory()
	cfg := factory.CreateDefaultConfig()
	assert.NoError(t, confmap.New().Unmarshal(cfg))
	assert.Equal(t, factory.CreateDefaultConfig(), cfg)
	assert.Equal(t, &Config{RefreshInterval: 10 * time.Minute, Timeout: 3 * time.Second}, cfg)
	assert.NoError(t, cfg.(*Config).Validate())
}

func TestUnmarshalConfig(t *testing.T) {
	conf := confmap.NewFromStringMap(map[string]any{
		"node_name":        "node-a",
		"refresh_interval": "5m",
		"timeout":          "2s",
		"env_map": map[string]any{
			"MODEL_PATH":             "model",
			"MAX_MODEL_LEN":          "max_model_len",
			"GPU_MEMORY_UTILIZATION": "gpu_memory_utilization",
			"TENSOR_PARALLEL_SIZE":   "tensor_parallel_size",
		},
	})
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	require.NoError(t, conf.Unmarshal(cfg))
	assert.Equal(t, "node-a", cfg.NodeName)
	assert.Equal(t, 5*time.Minute, cfg.RefreshInterval)
	assert.Equal(t, 2*time.Second, cfg.Timeout)
	// env var names keep their case
	assert.Equal(t, "max_model_len", cfg.EnvMap["MAX_MODEL_LEN"])
	assert.NoError(t, cfg.Validate())
}

func TestValidate(t *testing.T) {
	cfg := &Config{RefreshInterval: time.Minute, Timeout: time.Second, EnvMap: map[string]string{"X": "version"}}
	assert.ErrorContains(t, cfg.Validate(), `unknown value name "version"`)
	cfg = &Config{RefreshInterval: 0, Timeout: time.Second}
	assert.ErrorContains(t, cfg.Validate(), "refresh_interval")
	cfg = &Config{RefreshInterval: time.Minute}
	assert.ErrorContains(t, cfg.Validate(), "timeout")
}

// TestEncodeConfig: at start the agent re-encodes the merged configuration with
// internal/mapstructure and exits on an error, so the config must encode.
func TestEncodeConfig(t *testing.T) {
	def := NewFactory().CreateDefaultConfig()
	out, err := mapstructure.Marshal(def)
	require.NoError(t, err)
	assert.Equal(t, time.Duration(10*time.Minute), out["refresh_interval"])
	assert.Equal(t, time.Duration(3*time.Second), out["timeout"])

	withMap := &Config{NodeName: "node-a", RefreshInterval: time.Minute, Timeout: time.Second,
		EnvMap: map[string]string{"MAX_MODEL_LEN": "max_model_len"}}
	// the way the agent sees it: inside the processors map of the collector config
	type collectorLike struct {
		Processors map[component.ID]component.Config `mapstructure:"processors"`
	}
	id := component.NewIDWithName(TypeStr, "solstice_vllm")
	out, err = mapstructure.Marshal(&collectorLike{Processors: map[component.ID]component.Config{id: withMap}})
	require.NoError(t, err)
	procs := out["processors"].(map[string]any)
	got := procs["vllmconfig/solstice_vllm"].(map[string]any)
	assert.Equal(t, "node-a", got["node_name"])
	assert.Equal(t, map[string]any{"MAX_MODEL_LEN": "max_model_len"}, got["env_map"])
}
