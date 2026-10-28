// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

// Package engineconfig is a proof-of-concept processor that resolves one full
// configuration record per inference engine container (vLLM or SGLang) and
// writes it in the configured shapes: on the data points of one real gauge per
// engine (anchor), as one extra gauge series with value 1 (info), or per metric
// family (per_family, vLLM only). The record has the engine identity and role,
// the serving object, the configured values from the pod spec, and the effective
// values from the engine routes, the engine metrics and, optionally, the vLLM
// startup log line. It never overwrites an attribute.
package engineconfig

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

type engineConfigProcessor struct {
	cfg    *Config
	logger *zap.Logger
	eng    engine

	// newClient builds the Kubernetes client; tests replace it.
	newClient func() (kubernetes.Interface, error)

	resolver *resolver
	stopCh   chan struct{}

	mu   sync.RWMutex
	pods podLookup // nil: Kubernetes is not available, metrics pass through

	missMu   sync.Mutex
	lastMiss time.Time
}

func newEngineConfigProcessor(cfg *Config, logger *zap.Logger) (*engineConfigProcessor, error) {
	eng, err := newEngine(cfg.Engine, newAPIFetcher())
	if err != nil {
		return nil, err
	}
	return &engineConfigProcessor{
		cfg:       cfg,
		logger:    logger,
		eng:       eng,
		newClient: inClusterClient,
	}, nil
}

func inClusterClient() (kubernetes.Interface, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		return nil, err
	}
	return kubernetes.NewForConfig(config)
}

// start never fails: without Kubernetes the processor passes metrics through.
func (p *engineConfigProcessor) start(_ context.Context, _ component.Host) error {
	p.resolver = newResolver(p.cfg, p.eng, p.logger)
	p.stopCh = make(chan struct{})
	nodeName := p.cfg.NodeName
	if nodeName == "" {
		nodeName = os.Getenv(nodeNameEnv)
	}
	if nodeName == "" {
		p.logger.Warn("engineconfig: no node name (node_name or " + nodeNameEnv + "); metrics pass through without engine values")
		return nil
	}
	client, err := p.newClient()
	if err != nil {
		p.logger.Warn("engineconfig: Kubernetes is not available; metrics pass through without engine values", zap.Error(err))
		return nil
	}
	w, err := newPodWatcher(client, nodeName, p.eng.name(), p.cfg.EnvMap, p.logger, p.resolver.forget)
	if err != nil {
		p.logger.Warn("engineconfig: cannot watch pods; metrics pass through without engine values", zap.Error(err))
		return nil
	}
	w.start(p.stopCh)
	p.mu.Lock()
	p.pods = w
	p.mu.Unlock()
	p.logger.Info("engineconfig: watching the pods of this node",
		zap.String("node", nodeName), zap.String("engine", p.eng.name()), zap.Strings("shapes", p.cfg.Shapes))
	return nil
}

func (p *engineConfigProcessor) shutdown(context.Context) error {
	if p.stopCh != nil {
		close(p.stopCh)
		p.stopCh = nil
	}
	if p.resolver != nil {
		p.resolver.shutdown()
	}
	return nil
}

func (p *engineConfigProcessor) processMetrics(_ context.Context, md pmetric.Metrics) (pmetric.Metrics, error) {
	p.mu.RLock()
	pods := p.pods
	p.mu.RUnlock()
	if pods == nil || p.resolver == nil {
		return md, nil
	}
	rms := md.ResourceMetrics()
	for i := 0; i < rms.Len(); i++ {
		rm := rms.At(i)
		if !p.eng.owns(rm) {
			continue
		}
		t := targetFromResource(rm.Resource().Attributes())
		pod, ok := pods.lookup(t)
		if !ok {
			p.logMiss(t)
			continue
		}
		// read the engine metrics first, so this scrape already uses them
		rec := p.resolver.resolve(pod, t, p.eng.scraped(rm))
		p.emit(rm, rec, pod)
	}
	return md, nil
}

// emit writes the record in each configured shape.
func (p *engineConfigProcessor) emit(rm pmetric.ResourceMetrics, rec record, pod podView) {
	attrs := rec.attributes(p.cfg.EmitWorkload)
	anchor := p.eng.anchorFamily()
	for _, shape := range p.cfg.Shapes {
		switch shape {
		case shapeAnchor:
			if !emitAnchor(rm, anchor, attrs) && p.resolver.warnAnchor(pod.uid) {
				p.logger.Info("engineconfig: the anchor family is not in the scrape; the anchor shape adds nothing for this pod",
					zap.String("pod", pod.namespace+"/"+pod.name), zap.String("family", anchor))
			}
		case shapeInfo:
			emitInfo(rm, anchor, attrs)
		case shapePerFamily:
			emitPerFamily(rm, rec)
		}
	}
	if p.cfg.ResourceIdentity {
		putAttributes(rm.Resource().Attributes(), rec.identity())
	}
}

func (p *engineConfigProcessor) logMiss(t scrapeTarget) {
	p.missMu.Lock()
	defer p.missMu.Unlock()
	if time.Since(p.lastMiss) < time.Minute {
		return
	}
	p.lastMiss = time.Now()
	p.logger.Debug("engineconfig: no pod for the scrape target", zap.String("host", t.host), zap.String("port", t.port))
}
