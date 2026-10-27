// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

// Package vllmconfig is a proof-of-concept processor that attaches vLLM engine
// configuration values to the vllm:* metric families that each value explains.
// It reads the vLLM HTTP API (/version, /v1/models), the vllm:cache_config_info
// labels of the scrape, the pod spec args of the scraped container and an
// optional env contract. Identity values go on the resource, the other values
// on the data points of the mapped families. It never overwrites an attribute.
package vllmconfig

import (
	"context"
	"net"
	"os"
	"sync"
	"time"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.uber.org/zap"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Resource attributes that the prometheus receiver sets for a scrape. The k8s.*
// ones come from the kubernetes_sd (role pod) discovery labels.
const (
	attrServerAddress     = "server.address"
	attrServerPort        = "server.port"
	attrURLScheme         = "url.scheme"
	attrServiceInstanceID = "service.instance.id"
	attrPodUID            = "k8s.pod.uid"
	attrPodName           = "k8s.pod.name"
	attrNamespace         = "k8s.namespace.name"
	attrContainerName     = "k8s.container.name"

	nodeNameEnv = "K8S_NODE_NAME"
)

// scrapeTarget is the scraped endpoint, from the resource attributes.
type scrapeTarget struct {
	scheme    string
	host      string
	port      string
	podUID    string
	podName   string
	namespace string
	container string
}

func targetFromResource(attrs pcommon.Map) scrapeTarget {
	get := func(k string) string {
		if v, ok := attrs.Get(k); ok {
			return v.AsString()
		}
		return ""
	}
	t := scrapeTarget{
		scheme:    get(attrURLScheme),
		host:      get(attrServerAddress),
		port:      get(attrServerPort),
		podUID:    get(attrPodUID),
		podName:   get(attrPodName),
		namespace: get(attrNamespace),
		container: get(attrContainerName),
	}
	if t.host == "" || t.port == "" {
		// server.address is left out for a loopback or unspecified host
		if h, p, err := net.SplitHostPort(get(attrServiceInstanceID)); err == nil {
			if t.host == "" {
				t.host = h
			}
			if t.port == "" {
				t.port = p
			}
		}
	}
	if t.scheme == "" {
		t.scheme = "http"
	}
	return t
}

type vllmConfigProcessor struct {
	cfg    *Config
	logger *zap.Logger

	// newClient builds the Kubernetes client; tests replace it.
	newClient func() (kubernetes.Interface, error)
	fetcher   httpFetcher

	resolver *resolver
	stopCh   chan struct{}

	mu   sync.RWMutex
	pods podLookup // nil: Kubernetes is not available, metrics pass through

	missMu   sync.Mutex
	lastMiss time.Time
}

func newVLLMConfigProcessor(cfg *Config, logger *zap.Logger) *vllmConfigProcessor {
	return &vllmConfigProcessor{
		cfg:       cfg,
		logger:    logger,
		newClient: inClusterClient,
		fetcher:   newAPIFetcher(),
	}
}

func inClusterClient() (kubernetes.Interface, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(config)
}

// start never fails: without Kubernetes the processor passes metrics through.
func (p *vllmConfigProcessor) start(_ context.Context, _ component.Host) error {
	p.resolver = newResolver(p.cfg, p.fetcher, p.logger)
	p.stopCh = make(chan struct{})
	nodeName := p.cfg.NodeName
	if nodeName == "" {
		nodeName = os.Getenv(nodeNameEnv)
	}
	if nodeName == "" {
		p.logger.Warn("vllmconfig: no node name (node_name or " + nodeNameEnv + "); metrics pass through without engine values")
		return nil
	}
	client, err := p.newClient()
	if err != nil {
		p.logger.Warn("vllmconfig: Kubernetes is not available; metrics pass through without engine values", zap.Error(err))
		return nil
	}
	w, err := newPodWatcher(client, nodeName, p.cfg.EnvMap, p.logger, p.resolver.forget)
	if err != nil {
		p.logger.Warn("vllmconfig: cannot watch pods; metrics pass through without engine values", zap.Error(err))
		return nil
	}
	w.start(p.stopCh)
	p.mu.Lock()
	p.pods = w
	p.mu.Unlock()
	p.logger.Info("vllmconfig: watching the pods of this node", zap.String("node", nodeName))
	return nil
}

func (p *vllmConfigProcessor) shutdown(context.Context) error {
	if p.stopCh != nil {
		close(p.stopCh)
		p.stopCh = nil
	}
	if p.resolver != nil {
		p.resolver.shutdown()
	}
	return nil
}

func (p *vllmConfigProcessor) processMetrics(_ context.Context, md pmetric.Metrics) (pmetric.Metrics, error) {
	p.mu.RLock()
	pods := p.pods
	p.mu.RUnlock()
	if pods == nil || p.resolver == nil {
		return md, nil
	}
	rms := md.ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		rm := rms.At(i)
		hasVLLM, v0 := hasVLLMMetrics(rm)
		if !hasVLLM {
			continue
		}
		t := targetFromResource(rm.Resource().Attributes())
		pod, ok := pods.lookup(t)
		if !ok {
			p.logMiss(t)
			continue
		}
		// read vllm:cache_config_info first, so this scrape already uses it
		values := p.resolver.resolve(pod, t, cacheConfigFromMetrics(rm), v0)
		attachValues(rm, values)
	}
	return md, nil
}

func (p *vllmConfigProcessor) logMiss(t scrapeTarget) {
	p.missMu.Lock()
	defer p.missMu.Unlock()
	if time.Since(p.lastMiss) < time.Minute {
		return
	}
	p.lastMiss = time.Now()
	p.logger.Debug("vllmconfig: no pod for the scrape target", zap.String("host", t.host), zap.String("port", t.port))
}

// attachValues puts the identity values on the resource and the other values on
// the data points of the families that they explain. It never overwrites.
func attachValues(rm pmetric.ResourceMetrics, values map[string]string) {
	if len(values) == 0 {
		return
	}
	res := rm.Resource().Attributes()
	for _, name := range resourceValues {
		if v, ok := values[name]; ok {
			putIfAbsent(res, attrPrefix+name, v)
		}
	}
	sms := rm.ScopeMetrics()
	for i := 0; i < sms.Len(); i++ {
		ms := sms.At(i).Metrics()
		for j := 0; j < ms.Len(); j++ {
			m := ms.At(j)
			names := valuesForMetric(m.Name())
			if len(names) == 0 {
				continue
			}
			forEachDataPoint(m, func(attrs pcommon.Map) {
				for _, name := range names {
					if v, ok := values[name]; ok {
						putIfAbsent(attrs, attrPrefix+name, v)
					}
				}
			})
		}
	}
}

func putIfAbsent(attrs pcommon.Map, key, value string) {
	if _, exists := attrs.Get(key); !exists {
		attrs.PutStr(key, value)
	}
}

func forEachDataPoint(m pmetric.Metric, fn func(pcommon.Map)) {
	switch m.Type() {
	case pmetric.MetricTypeGauge:
		dps := m.Gauge().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			fn(dps.At(i).Attributes())
		}
	case pmetric.MetricTypeSum:
		dps := m.Sum().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			fn(dps.At(i).Attributes())
		}
	case pmetric.MetricTypeHistogram:
		dps := m.Histogram().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			fn(dps.At(i).Attributes())
		}
	case pmetric.MetricTypeExponentialHistogram:
		dps := m.ExponentialHistogram().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			fn(dps.At(i).Attributes())
		}
	case pmetric.MetricTypeSummary:
		dps := m.Summary().DataPoints()
		for i := 0; i < dps.Len(); i++ {
			fn(dps.At(i).Attributes())
		}
	}
}
