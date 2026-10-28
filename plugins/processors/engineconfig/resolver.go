// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package engineconfig

import (
	"context"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
	"k8s.io/apimachinery/pkg/types"
)

// entryTTL drops the state of a pod that was not scraped for this long.
const entryTTL = time.Hour

// podEntry is the cached state of one container start (pod UID, container and
// restart count) of a pod.
type podEntry struct {
	mu          sync.Mutex
	incarnation string
	fetched     bool
	lastAttempt time.Time
	lastFailed  bool
	routes      map[string]sourced // last good values of the HTTP routes
	scraped     map[string]sourced // last seen values of the engine metrics
	logDone     bool
	logAttempt  time.Time
	logValues   map[string]sourced
	lastSeen    time.Time
	described   string // the record of the last debug log
	// anchorWarned is not reset by a restart: the missing anchor is logged once per pod.
	anchorWarned bool
}

// reset clears the state for a new container start. The caller holds e.mu.
func (e *podEntry) reset(incarnation string) {
	e.incarnation = incarnation
	e.fetched, e.lastFailed, e.logDone = false, false, false
	e.lastAttempt, e.logAttempt = time.Time{}, time.Time{}
	e.routes, e.scraped, e.logValues = map[string]sourced{}, map[string]sourced{}, nil
	e.described = ""
}

type resolver struct {
	logger  *zap.Logger
	eng     engine
	logs    *startupLog // nil: no startup-line fallback
	refresh time.Duration
	retry   time.Duration
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

func newResolver(cfg *Config, eng engine, logger *zap.Logger) *resolver {
	ctx, cancel := context.WithCancel(context.Background())
	r := &resolver{
		logger:  logger,
		eng:     eng,
		refresh: cfg.RefreshInterval,
		retry:   cfg.RetryInterval,
		timeout: cfg.Timeout,
		now:     time.Now,
		ctx:     ctx,
		cancel:  cancel,
		entries: map[types.UID]*podEntry{},
	}
	if cfg.LogFallback {
		r.logs = &startupLog{root: cfg.LogRoot, limit: int64(cfg.LogReadLimit)}
	}
	return r
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

// warnAnchor reports whether the missing anchor family of the pod is to be
// logged: the first time only.
func (r *resolver) warnAnchor(uid types.UID) bool {
	e := r.entry(uid)
	e.mu.Lock()
	defer e.mu.Unlock()
	first := !e.anchorWarned
	e.anchorWarned = true
	return first
}

// resolve returns the record of the scraped container. On the first sight of a
// container start it reads the HTTP routes (and the startup line) synchronously
// within the timeout, so the first exported batch already carries the values.
func (r *resolver) resolve(pod podView, t scrapeTarget, scraped map[string]sourced) record {
	container, _ := pod.scrapedContainer(t)
	ports := probePorts(t, container)
	restarts := pod.restarts[container.Name]
	incarnation := string(pod.uid) + "/" + container.Name + "/" + strconv.Itoa(int(restarts))

	e := r.entry(pod.uid)
	e.mu.Lock()
	if e.incarnation != incarnation {
		// a new pod, or the engine restarted: read everything again
		e.reset(incarnation)
	}
	now := r.now()
	e.lastSeen = now
	for k, v := range scraped {
		e.scraped[k] = v
	}
	routesFirst, routesDue := !e.fetched, false
	if !routesFirst {
		wait := r.refresh
		if e.lastFailed {
			wait = r.retry
		}
		if now.Sub(e.lastAttempt) >= wait {
			routesDue = true
			e.lastAttempt = now // claim the refresh
		}
	}
	logFirst, logDue := false, false
	if r.logs != nil && !e.logDone {
		logFirst = e.logAttempt.IsZero()
		if !logFirst && now.Sub(e.logAttempt) >= r.retry {
			logDue = true
			e.logAttempt = now // claim the retry
		}
	}
	e.mu.Unlock()

	if routesFirst {
		r.fetchRoutes(e, incarnation, t, ports, false)
	}
	if logFirst {
		r.readLog(e, incarnation, pod, container.Name, restarts, false)
	}
	if routesDue || logDue {
		r.wg.Add(1)
		go func() {
			defer r.wg.Done()
			if routesDue {
				r.fetchRoutes(e, incarnation, t, ports, true)
			}
			if logDue {
				r.readLog(e, incarnation, pod, container.Name, restarts, true)
			}
		}()
	}

	// effective values by precedence: the HTTP routes, then the engine metrics,
	// then the startup line (a later map overwrites)
	e.mu.Lock()
	effective := map[string]sourced{}
	for _, src := range []map[string]sourced{e.logValues, e.scraped, e.routes} {
		for k, v := range src {
			effective[k] = v
		}
	}
	rec := buildRecord(r.eng, pod, container, effective)
	described := rec.describe()
	changed := described != e.described
	e.described = described
	e.mu.Unlock()
	if changed {
		r.logger.Debug("engineconfig: record", zap.String("pod", pod.namespace+"/"+pod.name), zap.String("record", described))
	}
	return rec
}

// fetchRoutes reads the HTTP routes once per container start at a time;
// concurrent batches of the same pod wait for the same read. refresh=false is the
// first read, which is skipped if another batch has already done it.
func (r *resolver) fetchRoutes(e *podEntry, incarnation string, t scrapeTarget, ports []string, refresh bool) {
	_, _, _ = r.group.Do("routes/"+incarnation, func() (any, error) {
		e.mu.Lock()
		done := e.fetched && e.incarnation == incarnation
		e.mu.Unlock()
		if done && !refresh {
			return nil, nil
		}
		ctx, cancel := context.WithTimeout(r.ctx, r.timeout)
		defer cancel()
		res := r.eng.routes(ctx, t, ports)
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.incarnation != incarnation {
			return nil, nil
		}
		e.fetched = true
		e.lastAttempt = r.now()
		// Keep the last good values: a value is replaced only by a new good one.
		for k, v := range res.values {
			e.routes[k] = v
		}
		e.lastFailed = res.failed
		r.logger.Debug("engineconfig: read the engine routes",
			zap.String("pod", t.namespace+"/"+t.podName),
			zap.Int("values", len(res.values)),
			zap.Bool("failed", res.failed))
		return nil, nil
	})
}

// readLog reads the startup line once per container start. If the line is not
// there yet, it is read again on the retry interval. retry=false is the first
// read, which is skipped if another batch has already done it.
func (r *resolver) readLog(e *podEntry, incarnation string, pod podView, container string, restarts int32, retry bool) {
	_, _, _ = r.group.Do("log/"+incarnation, func() (any, error) {
		e.mu.Lock()
		skip := e.logDone || e.incarnation != incarnation || (!retry && !e.logAttempt.IsZero())
		if !skip {
			e.logAttempt = r.now()
		}
		e.mu.Unlock()
		if skip {
			return nil, nil
		}
		values, done := r.logs.read(pod.namespace, pod.name, string(pod.uid), container, restarts)
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.incarnation != incarnation {
			return nil, nil
		}
		e.logDone, e.logValues = done, values
		r.logger.Debug("engineconfig: read the startup line",
			zap.String("pod", pod.namespace+"/"+pod.name),
			zap.Bool("found", values != nil),
			zap.Bool("done", done))
		return nil, nil
	})
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
