// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package engineconfig

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"

	"github.com/aws/amazon-cloudwatch-agent/internal/mapstructure"
)

func validConfig() *Config {
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	cfg.Engine = engineVLLM
	return cfg
}

func TestUnmarshalDefaultConfig(t *testing.T) {
	factory := NewFactory()
	cfg := factory.CreateDefaultConfig()
	assert.NoError(t, confmap.New().Unmarshal(cfg))
	assert.Equal(t, factory.CreateDefaultConfig(), cfg)
	assert.Equal(t, &Config{
		Timeout: 3 * time.Second, RefreshInterval: 10 * time.Minute, RetryInterval: time.Minute,
		Shapes: []string{"anchor"}, LogRoot: "/var/log/pods", LogReadLimit: 1 << 20,
	}, cfg)
	// the engine is required
	assert.ErrorContains(t, cfg.(*Config).Validate(), "engine must be")
}

func TestUnmarshalConfig(t *testing.T) {
	conf := confmap.NewFromStringMap(map[string]any{
		"engine":            "vllm",
		"node_name":         "node-a",
		"timeout":           "2s",
		"refresh_interval":  "5m",
		"retry_interval":    "30s",
		"shapes":            []any{"anchor", "info", "per_family"},
		"resource_identity": true,
		"emit_workload":     true,
		"log_fallback":      true,
		"log_root":          "/host/var/log/pods",
		"log_read_limit":    262144,
		"env_map": map[string]any{
			"MODEL_PATH":    "model",
			"MAX_MODEL_LEN": "max_model_len",
		},
	})
	cfg := NewFactory().CreateDefaultConfig().(*Config)
	require.NoError(t, conf.Unmarshal(cfg))
	assert.Equal(t, &Config{
		Engine: "vllm", NodeName: "node-a", Timeout: 2 * time.Second, RefreshInterval: 5 * time.Minute,
		RetryInterval: 30 * time.Second, Shapes: []string{"anchor", "info", "per_family"}, ResourceIdentity: true,
		EmitWorkload: true, LogFallback: true, LogRoot: "/host/var/log/pods", LogReadLimit: 262144,
		// env var names keep their case
		EnvMap: map[string]string{"MODEL_PATH": "model", "MAX_MODEL_LEN": "max_model_len"},
	}, cfg)
	assert.NoError(t, cfg.Validate())
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*Config)
		want   string
	}{
		{"sglang", func(c *Config) { c.Engine, c.Shapes = engineSGLang, []string{shapeAnchor, shapeInfo} }, ""},
		{"unknown engine", func(c *Config) { c.Engine = "tgi" }, `engine must be "vllm" or "sglang", got "tgi"`},
		{"timeout", func(c *Config) { c.Timeout = 0 }, "timeout must be positive"},
		{"refresh_interval", func(c *Config) { c.RefreshInterval = -time.Second }, "refresh_interval must be positive"},
		{"retry_interval", func(c *Config) { c.RetryInterval = 0 }, "retry_interval must be positive"},
		{"no shapes", func(c *Config) { c.Shapes = nil }, "at least one shape"},
		{"unknown shape", func(c *Config) { c.Shapes = []string{"resource"} }, `unknown shape "resource"`},
		{"shape twice", func(c *Config) { c.Shapes = []string{shapeInfo, shapeInfo} }, `"info" is listed twice`},
		{"per_family for sglang", func(c *Config) { c.Engine, c.Shapes = engineSGLang, []string{shapePerFamily} },
			`"per_family" is only for engine "vllm"`},
		{"log_fallback for sglang", func(c *Config) { c.Engine, c.LogFallback = engineSGLang, true },
			`log_fallback is only for engine "vllm"`},
		{"relative log_root", func(c *Config) { c.LogFallback, c.LogRoot = true, "var/log/pods" }, "log_root must be an absolute path"},
		{"log_read_limit", func(c *Config) { c.LogFallback, c.LogReadLimit = true, 0 }, "log_read_limit must be between"},
		{"log_read_limit too large", func(c *Config) { c.LogFallback, c.LogReadLimit = true, 1<<30 }, "log_read_limit must be between"},
		{"log settings unused without log_fallback", func(c *Config) { c.LogRoot, c.LogReadLimit = "", 0 }, ""},
		{"env_map for sglang", func(c *Config) { c.Engine, c.EnvMap = engineSGLang, map[string]string{"X": valModel} },
			`env_map is only for engine "vllm"`},
		{"env_map value", func(c *Config) { c.EnvMap = map[string]string{"X": "version"} }, `unknown value name "version"`},
		{"env_map name", func(c *Config) { c.EnvMap = map[string]string{"": valModel} }, "empty env var name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.modify(cfg)
			if tt.want == "" {
				assert.NoError(t, cfg.Validate())
			} else {
				assert.ErrorContains(t, cfg.Validate(), tt.want)
			}
		})
	}
}

// TestEncodeConfig: at start the agent re-encodes the merged configuration with
// internal/mapstructure and exits on an error, so the config must encode.
func TestEncodeConfig(t *testing.T) {
	out, err := mapstructure.Marshal(NewFactory().CreateDefaultConfig())
	require.NoError(t, err)
	assert.Equal(t, time.Duration(10*time.Minute), out["refresh_interval"])
	assert.Equal(t, time.Duration(3*time.Second), out["timeout"])
	assert.Equal(t, []any{"anchor"}, out["shapes"])
	assert.Equal(t, 1<<20, out["log_read_limit"])

	full := validConfig()
	full.NodeName, full.Shapes, full.LogFallback = "node-a", []string{shapeAnchor, shapeInfo}, true
	full.EnvMap = map[string]string{"MAX_MODEL_LEN": "max_model_len"}
	// the way the agent sees it: inside the processors map of the collector config
	type collectorLike struct {
		Processors map[component.ID]component.Config `mapstructure:"processors"`
	}
	id := component.NewIDWithName(TypeStr, "vllm")
	out, err = mapstructure.Marshal(&collectorLike{Processors: map[component.ID]component.Config{id: full}})
	require.NoError(t, err)
	got := out["processors"].(map[string]any)["engineconfig/vllm"].(map[string]any)
	assert.Equal(t, "vllm", got["engine"])
	assert.Equal(t, "node-a", got["node_name"])
	assert.Equal(t, true, got["log_fallback"])
	assert.Equal(t, []any{"anchor", "info"}, got["shapes"])
	assert.Equal(t, map[string]any{"MAX_MODEL_LEN": "max_model_len"}, got["env_map"])

	// and it decodes back to the same config
	back := NewFactory().CreateDefaultConfig().(*Config)
	require.NoError(t, confmap.NewFromStringMap(got).Unmarshal(back))
	assert.Equal(t, full, back)
}
