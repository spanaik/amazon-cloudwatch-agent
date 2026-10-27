// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package vllmconfig

import (
	"context"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
	"k8s.io/apimachinery/pkg/types"
)

const (
	// retryInterval bounds the retries after a failed fetch.
	retryInterval = time.Minute
	// entryTTL drops the state of a pod that was not scraped for this long.
	entryTTL = time.Hour
)

// podEntry is the cached state of one pod incarnation (UID and restart count).
type podEntry struct {
	mu          sync.Mutex
	incarnation string
	fetched     bool
	lastAttempt time.Time
	lastFailed  bool
	version     string
	model       string
	maxModelLen string
	cacheConfig map[string]string // last seen vllm:cache_config_info labels
	v0Seen      bool
	lastSeen    time.Time
}

// reset clears the state for a new incarnation. The caller holds e.mu.
func (e *podEntry) reset(incarnation string) {
	e.incarnation = incarnation
	e.fetched = false
	e.lastAttempt = time.Time{}
	e.lastFailed = false
	e.version, e.model, e.maxModelLen = "", "", ""
	e.cacheConfig = nil
	e.v0Seen = false
}

type entrySnapshot struct {
	version, model, maxModelLen string
	cacheConfig                 map[string]string
	v0Seen                      bool
}

type resolver struct {
	logger  *zap.Logger
	fetcher httpFetcher
	refresh time.Duration
	timeout time.Duration
	now     func() time.Time

	ctx    context.Context // cancelled at shutdown
	cancel context.CancelFunc
	group  singleflight.Group
	wg     sync.WaitGroup

	mu        sync.Mutex
	entries   map[types.UID]*podEntry
	lastSweep time.Time
}

func newResolver(cfg *Config, fetcher httpFetcher, logger *zap.Logger) *resolver {
	ctx, cancel := context.WithCancel(context.Background())
	return &resolver{
		logger:  logger,
		fetcher: fetcher,
		refresh: cfg.RefreshInterval,
		timeout: cfg.Timeout,
		now:     time.Now,
		ctx:     ctx,
		cancel:  cancel,
		entries: map[types.UID]*podEntry{},
	}
}

func (r *resolver) shutdown() {
	r.cancel()
	r.wg.Wait()
}

func (r *resolver) forget(uid types.UID) {
	r.mu.Lock()
	delete(r.entries, uid)
	r.mu.Unlock()
}

func (r *resolver) entry(uid types.UID) *podEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if now.Sub(r.lastSweep) > entryTTL/4 {
		r.lastSweep = now
		for k, e := range r.entries {
			e.mu.Lock()
			stale := now.Sub(e.lastSeen) > entryTTL
			e.mu.Unlock()
			if stale {
				delete(r.entries, k)
			}
		}
	}
	e, ok := r.entries[uid]
	if !ok {
		e = &podEntry{}
		r.entries[uid] = e
	}
	return e
}

// resolve returns the engine values of the scraped pod. On the first sight of a
// pod incarnation it reads the HTTP API synchronously within the timeout, so the
// first exported batch already carries the values.
func (r *resolver) resolve(pod podView, t scrapeTarget, cacheConfig map[string]string, v0 bool) map[string]string {
	container, _ := pod.scrapedContainer(t)
	ports := probePorts(t, container)
	incarnation := string(pod.uid) + "/" + strconv.FormatInt(pod.restarts, 10)

	e := r.entry(pod.uid)
	e.mu.Lock()
	if e.incarnation != incarnation {
		// a new pod, or the engine restarted: read everything again
		e.reset(incarnation)
	}
	now := r.now()
	e.lastSeen = now
	if cacheConfig != nil {
		e.cacheConfig = cacheConfig
	}
	if v0 {
		e.v0Seen = true
	}
	first := !e.fetched
	due := false
	if !first {
		wait := r.refresh
		if e.lastFailed {
			wait = retryInterval
		}
		if now.Sub(e.lastAttempt) >= wait {
			due = true
			e.lastAttempt = now // claim the refresh
		}
	}
	e.mu.Unlock()

	switch {
	case first:
		r.fetch(e, incarnation, t, ports, false)
	case due:
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			r.fetch(e, incarnation, t, ports, true)
		}()
	}

	e.mu.Lock()
	snap := entrySnapshot{
		version: e.version, model: e.model, maxModelLen: e.maxModelLen,
		cacheConfig: e.cacheConfig, v0Seen: e.v0Seen,
	}
	e.mu.Unlock()
	return combine(snap, container)
}

// fetch reads the HTTP API once per incarnation at a time; concurrent batches of
// the same pod wait for the same fetch. refresh=false is the first read, which is
// skipped if another batch has already done it.
func (r *resolver) fetch(e *podEntry, incarnation string, t scrapeTarget, ports []string, refresh bool) {
	_, _, _ = r.group.Do(incarnation, func() (any, error) {
		e.mu.Lock()
		done := e.fetched && e.incarnation == incarnation
		e.mu.Unlock()
		if done && !refresh {
			return nil, nil
		}
		ctx, cancel := context.WithTimeout(r.ctx, r.timeout)
		defer cancel()
		res := r.fetcher.fetch(ctx, t.scheme, t.host, ports)
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.incarnation != incarnation {
			return nil, nil
		}
		e.fetched = true
		e.lastAttempt = r.now()
		// Keep the last good values: a value is replaced only by a new good one.
		if res.versionState == probeAvailable {
			e.version = res.version
		}
		if res.modelsState == probeAvailable {
			e.model = res.model
			if res.maxModelLen != "" {
				e.maxModelLen = res.maxModelLen
			}
		}
		e.lastFailed = res.versionState == probeFailed || res.modelsState == probeFailed
		r.logger.Debug("vllmconfig: read the vLLM API",
			zap.String("pod", t.namespace+"/"+t.podName),
			zap.Bool("version", res.versionState == probeAvailable),
			zap.Bool("models", res.modelsState == probeAvailable),
			zap.Bool("failed", e.lastFailed))
		return nil, nil
	})
}

func fromHTTP(v string) fieldValue {
	if v == "" {
		return fieldValue{}
	}
	return set(v)
}

func fromMap(m map[string]string, name string) fieldValue {
	if v, ok := m[name]; ok && v != "" {
		return set(v)
	}
	return fieldValue{}
}

func fromArgs(c containerInfo, name string) fieldValue {
	if c.Args == nil {
		return fieldValue{}
	}
	return c.Args[name]
}

func fromEnv(c containerInfo, name string) fieldValue {
	if c.Env == nil {
		return fieldValue{}
	}
	return c.Env[name]
}

// combine applies the source precedence per value: the HTTP API (effective), then
// vllm:cache_config_info (effective), then the pod args, then the env contract.
func combine(s entrySnapshot, c containerInfo) map[string]string {
	out := map[string]string{}
	put := func(name string, sources ...fieldValue) {
		if v, ok := pick(sources...); ok && validValue(v) {
			out[name] = v
		}
	}
	put(valVersion, fromHTTP(s.version))
	if engine := deriveEngine(s.version, c.UseV1, s.v0Seen); engine != "" {
		out[valEngine] = engine
	}
	put(valModel, fromHTTP(s.model), fromArgs(c, valModel), fromEnv(c, valModel))
	put(valMaxModelLen, fromHTTP(s.maxModelLen), fromArgs(c, valMaxModelLen), fromEnv(c, valMaxModelLen))
	for _, name := range []string{valKVCacheDtype, valEnablePrefixCaching, valGPUMemoryUtilization} {
		put(name, fromMap(s.cacheConfig, name), fromArgs(c, name), fromEnv(c, name))
	}
	for _, name := range []string{
		valTensorParallelSize, valPipelineParallelSize, valDataParallelSize, valDtype, valQuantization,
		valEnforceEager, valSpeculativeMethod, valNumSpeculativeTokens, valMaxNumSeqs,
		valMaxNumBatchedTokens, valEnableChunkedPrefill,
	} {
		put(name, fromArgs(c, name), fromEnv(c, name))
	}
	return out
}
