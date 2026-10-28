// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package engineconfig

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"

	"go.opentelemetry.io/collector/component"
)

// maxLogReadLimit bounds log_read_limit.
const maxLogReadLimit = 64 << 20

// Config of the engineconfig processor. It uses only plain types, because the
// agent re-encodes the merged OTel configuration with internal/mapstructure at
// start and exits on an error.
type Config struct {
	// Engine is the inference engine of the pipeline: vllm or sglang. One
	// processor serves one engine.
	Engine string `mapstructure:"engine"`
	// NodeName limits the pod watch to the pods of this node. Empty means the
	// K8S_NODE_NAME environment variable.
	NodeName string `mapstructure:"node_name"`
	// Timeout bounds the first, synchronous read of a pod's HTTP routes.
	Timeout time.Duration `mapstructure:"timeout"`
	// RefreshInterval is how often the HTTP routes of a pod are read again.
	RefreshInterval time.Duration `mapstructure:"refresh_interval"`
	// RetryInterval is how soon a failed read of the routes, or a startup line
	// that is not in the log yet, is tried again.
	RetryInterval time.Duration `mapstructure:"retry_interval"`
	// Shapes are the record shapes to write: anchor, info and per_family (vllm only).
	Shapes []string `mapstructure:"shapes"`
	// ResourceIdentity also puts the identity attributes on the resource.
	ResourceIdentity bool `mapstructure:"resource_identity"`
	// EmitWorkload also writes k8s.workload.type and k8s.workload.name, for a
	// pipeline without the k8sattributes processor and the chart's workload transform.
	EmitWorkload bool `mapstructure:"emit_workload"`
	// LogFallback reads the vLLM startup line from the container log under LogRoot
	// for the effective values (vllm only). LogReadLimit bounds the bytes read per
	// container start.
	LogFallback  bool   `mapstructure:"log_fallback"`
	LogRoot      string `mapstructure:"log_root"`
	LogReadLimit int    `mapstructure:"log_read_limit"`
	// EnvMap is a PoC-only env contract (vllm only): env var name -> value name
	// (for example MAX_MODEL_LEN: max_model_len), for images that read engine
	// values from their own env vars instead of vLLM flags. Only literal values
	// are read.
	EnvMap map[string]string `mapstructure:"env_map"`
}

var _ component.Config = (*Config)(nil)

func (cfg *Config) Validate() error {
	var errs []error
	if cfg.Engine != engineVLLM && cfg.Engine != engineSGLang {
		errs = append(errs, fmt.Errorf("engine must be %q or %q, got %q", engineVLLM, engineSGLang, cfg.Engine))
	}
	for _, d := range []struct {
		name string
		d    time.Duration
	}{{"timeout", cfg.Timeout}, {"refresh_interval", cfg.RefreshInterval}, {"retry_interval", cfg.RetryInterval}} {
		if d.d <= 0 {
			errs = append(errs, fmt.Errorf("%s must be positive, got %v", d.name, d.d))
		}
	}
	if len(cfg.Shapes) == 0 {
		errs = append(errs, errors.New("shapes must name at least one shape"))
	}
	seen := map[string]bool{}
	for _, s := range cfg.Shapes {
		switch {
		case s != shapeAnchor && s != shapeInfo && s != shapePerFamily:
			errs = append(errs, fmt.Errorf("shapes: unknown shape %q (allowed: %s, %s, %s)", s, shapeAnchor, shapeInfo, shapePerFamily))
		case seen[s]:
			errs = append(errs, fmt.Errorf("shapes: %q is listed twice", s))
		case s == shapePerFamily && cfg.Engine != engineVLLM:
			errs = append(errs, fmt.Errorf("shapes: %q is only for engine %q", s, engineVLLM))
		}
		seen[s] = true
	}
	if cfg.LogFallback {
		if cfg.Engine != engineVLLM {
			errs = append(errs, fmt.Errorf("log_fallback is only for engine %q", engineVLLM))
		}
		if !filepath.IsAbs(cfg.LogRoot) {
			errs = append(errs, fmt.Errorf("log_root must be an absolute path, got %q", cfg.LogRoot))
		}
		if cfg.LogReadLimit <= 0 || cfg.LogReadLimit > maxLogReadLimit {
			errs = append(errs, fmt.Errorf("log_read_limit must be between 1 and %d, got %d", maxLogReadLimit, cfg.LogReadLimit))
		}
	}
	if len(cfg.EnvMap) > 0 && cfg.Engine != engineVLLM {
		errs = append(errs, fmt.Errorf("env_map is only for engine %q", engineVLLM))
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
