// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT

package signaltoinfometricconnector

import (
	"cmp"
	"slices"
	"sync"
)

// Size estimate of the serialized state, used for max_state_bytes. The
// estimate of one entity is the sum of the lengths of its state key and of
// every key and value it holds, plus these constants. They approximate the
// JSON encoding of an entity in a snapshot.
const (
	// entityOverheadBytes covers the field names, brackets, and separators
	// of one entity, and its three timestamps at 19 digits each:
	// "<key>":{"identity":[],"resource":[],"facts":[],"first_seen":N,"updated":N,"activity":N},
	entityOverheadBytes = 160
	// pairOverheadBytes covers the framing of one key/value pair:
	// {"key":"","value":""},
	pairOverheadBytes = 24
)

// kv is one key/value pair. Lists of pairs keep the configured order.
type kv struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// entity is the latest known facts for one identity of one rule. The state
// never modifies a slice of an entity after storing it; a change stores new
// slices. Copies of an entity can therefore share its slices, and a snapshot
// can be encoded without holding the state lock.
type entity struct {
	// Identity holds the identity keys and values, in the rule's order.
	Identity []kv `json:"identity"`
	// Resource holds the included resource attributes that were present,
	// in the rule's order.
	Resource []kv `json:"resource,omitempty"`
	// Facts holds the extracted attributes, in the rule's order. Optional
	// attributes without a value are absent.
	Facts []kv `json:"facts"`
	// FirstSeen is the record time (Unix nanoseconds) at which the current
	// facts and resource values were first seen. It is the start time of
	// the emitted series and is reset when they change.
	FirstSeen int64 `json:"first_seen"`
	// Updated is the time of the latest record whose facts were applied.
	// Records older than Updated do not replace the facts.
	Updated int64 `json:"updated"`
	// Activity is the time of the latest record seen for the entity. The
	// least active entities are evicted first.
	Activity int64 `json:"activity"`
}

// observation is what a matching record says about an entity.
type observation struct {
	rule     int
	key      string
	identity []kv
	resource []kv
	facts    []kv
	// time is the record time in Unix nanoseconds.
	time int64
}

// outcome is the result of applying one observation.
type outcome uint8

const (
	// outcomeInserted: a new entity was stored.
	outcomeInserted outcome = iota + 1
	// outcomeChanged: the facts or resource values changed and replaced
	// the previous ones; FirstSeen was reset.
	outcomeChanged
	// outcomeUnchanged: same facts and resource values; Updated and
	// Activity moved forward.
	outcomeUnchanged
	// outcomeIgnoredOlder: the record is older than the stored facts, so
	// only Activity was updated.
	outcomeIgnoredOlder
	// outcomeTooLarge: the entity alone exceeds max_state_bytes and was
	// not stored. If it existed, its stale facts were removed.
	outcomeTooLarge

	numOutcomes = int(outcomeTooLarge) + 1
)

// entityRef identifies an entity in the state.
type entityRef struct {
	rule int
	key  string
}

// applyResult is the result of applying one observation.
type applyResult struct {
	outcome outcome
	// evicted lists the entities removed to make room, and the entity
	// itself when a change made it too large.
	evicted []entityRef
}

// emitEntry is a copy of an entity to emit.
type emitEntry struct {
	rule   int
	entity entity
}

// batchResult summarizes applyBatch.
type batchResult struct {
	// counts is indexed by outcome.
	counts  [numOutcomes]int
	evicted []entityRef
	// changed holds copies of the entities inserted or changed by the
	// batch that still exist after it, in order of their first change.
	// It is filled only when requested.
	changed []emitEntry
}

// ruleSpec is what the state needs to know about a rule.
type ruleSpec struct {
	name        string
	identity    []string
	maxEntities int
}

type storedEntity struct {
	entity
	// size is the estimate of the entity's serialized size.
	size int
}

type ruleState struct {
	ruleSpec
	entities map[string]*storedEntity
}

// state holds the entities of every rule. It does not use pdata types. One
// mutex protects it; callers prepare observations before taking the lock.
type state struct {
	mu            sync.Mutex
	rules         []ruleState
	ruleIndex     map[string]int
	maxStateBytes int
	// bytes is the sum of the size estimates of all entities.
	bytes int
}

func newState(specs []ruleSpec, maxStateBytes int) *state {
	s := &state{
		rules:         make([]ruleState, len(specs)),
		ruleIndex:     make(map[string]int, len(specs)),
		maxStateBytes: maxStateBytes,
	}
	for i, spec := range specs {
		s.rules[i] = ruleState{ruleSpec: spec, entities: make(map[string]*storedEntity)}
		s.ruleIndex[spec.name] = i
	}
	return s
}

// apply applies one observation.
func (s *state) apply(o observation) applyResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyLocked(&o)
}

// applyBatch applies observations in order under one lock. When collect is
// set, it also returns copies of the entities that the batch inserted or
// changed, each once with its final value.
func (s *state) applyBatch(obs []observation, collect bool) batchResult {
	var res batchResult
	var dirty []entityRef
	var seen map[entityRef]struct{}

	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range obs {
		r := s.applyLocked(&obs[i])
		res.counts[r.outcome]++
		res.evicted = append(res.evicted, r.evicted...)
		if !collect || (r.outcome != outcomeInserted && r.outcome != outcomeChanged) {
			continue
		}
		ref := entityRef{rule: obs[i].rule, key: obs[i].key}
		if seen == nil {
			seen = make(map[entityRef]struct{})
		}
		if _, ok := seen[ref]; !ok {
			seen[ref] = struct{}{}
			dirty = append(dirty, ref)
		}
	}
	for _, ref := range dirty {
		if e, ok := s.rules[ref.rule].entities[ref.key]; ok {
			res.changed = append(res.changed, emitEntry{rule: ref.rule, entity: e.entity})
		}
	}
	return res
}

func (s *state) applyLocked(o *observation) applyResult {
	rs := &s.rules[o.rule]
	cur, exists := rs.entities[o.key]
	if !exists {
		return s.insertLocked(o.rule, o.key, entity{
			Identity:  o.identity,
			Resource:  o.resource,
			Facts:     o.facts,
			FirstSeen: o.time,
			Updated:   o.time,
			Activity:  o.time,
		})
	}

	cur.Activity = max(cur.Activity, o.time)
	if o.time < cur.Updated {
		return applyResult{outcome: outcomeIgnoredOlder}
	}
	if slices.Equal(cur.Facts, o.facts) && slices.Equal(cur.Resource, o.resource) {
		cur.Updated = o.time
		return applyResult{outcome: outcomeUnchanged}
	}

	next := entity{
		Identity:  o.identity,
		Resource:  o.resource,
		Facts:     o.facts,
		FirstSeen: o.time,
		Updated:   o.time,
		Activity:  cur.Activity,
	}
	size := entitySize(o.key, &next)
	if size > s.maxStateBytes {
		// Keeping the previous facts would emit values known to be stale.
		s.removeLocked(entityRef{rule: o.rule, key: o.key})
		return applyResult{outcome: outcomeTooLarge, evicted: []entityRef{{rule: o.rule, key: o.key}}}
	}
	s.bytes += size - cur.size
	cur.entity, cur.size = next, size
	return applyResult{outcome: outcomeChanged, evicted: s.evictForBytesLocked(o.rule, o.key)}
}

// insertLocked stores a new entity, evicting the rule's least active entity
// at max_entities, then the least active entities of any rule while the
// state is over max_state_bytes.
func (s *state) insertLocked(rule int, key string, e entity) applyResult {
	size := entitySize(key, &e)
	if size > s.maxStateBytes {
		return applyResult{outcome: outcomeTooLarge}
	}
	rs := &s.rules[rule]
	var evicted []entityRef
	for len(rs.entities) >= rs.maxEntities {
		victim, ok := s.leastActiveLocked(rule, rule, "")
		if !ok {
			break
		}
		s.removeLocked(victim)
		evicted = append(evicted, victim)
	}
	rs.entities[key] = &storedEntity{entity: e, size: size}
	s.bytes += size
	evicted = append(evicted, s.evictForBytesLocked(rule, key)...)
	return applyResult{outcome: outcomeInserted, evicted: evicted}
}

// evictForBytesLocked evicts the least active entities of any rule, never
// the entity (keepRule, keepKey), until the state fits max_state_bytes.
func (s *state) evictForBytesLocked(keepRule int, keepKey string) []entityRef {
	var evicted []entityRef
	for s.bytes > s.maxStateBytes {
		victim, ok := s.leastActiveLocked(-1, keepRule, keepKey)
		if !ok {
			break
		}
		s.removeLocked(victim)
		evicted = append(evicted, victim)
	}
	return evicted
}

// leastActiveLocked returns the entity with the smallest Activity, ties
// broken by the smallest rule name and then the smallest key. It scans one
// rule, or all rules when rule is -1, and skips the entity (skipRule,
// skipKey) when skipKey is not empty. The scan is O(n), which is fine for
// the configured limits because eviction is rare.
func (s *state) leastActiveLocked(rule, skipRule int, skipKey string) (entityRef, bool) {
	var best entityRef
	var bestActivity int64
	found := false
	for ri := range s.rules {
		if rule >= 0 && ri != rule {
			continue
		}
		for key, e := range s.rules[ri].entities {
			if skipKey != "" && ri == skipRule && key == skipKey {
				continue
			}
			if !found || e.Activity < bestActivity ||
				(e.Activity == bestActivity && s.lessRef(entityRef{rule: ri, key: key}, best)) {
				best, bestActivity, found = entityRef{rule: ri, key: key}, e.Activity, true
			}
		}
	}
	return best, found
}

func (s *state) lessRef(a, b entityRef) bool {
	return cmp.Or(
		cmp.Compare(s.rules[a.rule].name, s.rules[b.rule].name),
		cmp.Compare(a.key, b.key),
	) < 0
}

func (s *state) removeLocked(ref entityRef) {
	rs := &s.rules[ref.rule]
	if e, ok := rs.entities[ref.key]; ok {
		s.bytes -= e.size
		delete(rs.entities, ref.key)
	}
}

// entitySize estimates the serialized size of an entity; see
// entityOverheadBytes.
func entitySize(key string, e *entity) int {
	return entityOverheadBytes + len(key) + pairsSize(e.Identity) + pairsSize(e.Resource) + pairsSize(e.Facts)
}

func pairsSize(pairs []kv) int {
	n := 0
	for _, p := range pairs {
		n += pairOverheadBytes + len(p.Key) + len(p.Value)
	}
	return n
}

// stateSnapshot is the serializable form of the state: rule name, then
// identity key, then entity. Rules without entities are omitted.
type stateSnapshot struct {
	Rules map[string]map[string]entity `json:"rules"`
}

// snapshot returns a copy of the state. The entities share their slices with
// the state, which never modifies them, so the snapshot can be encoded
// without the lock.
func (s *state) snapshot() stateSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := stateSnapshot{Rules: make(map[string]map[string]entity, len(s.rules))}
	for i := range s.rules {
		rs := &s.rules[i]
		if len(rs.entities) == 0 {
			continue
		}
		entities := make(map[string]entity, len(rs.entities))
		for key, e := range rs.entities {
			entities[key] = e.entity
		}
		snap.Rules[rs.name] = entities
	}
	return snap
}

// restore replaces the state with the entities of a snapshot and returns
// how many it kept and dropped. It drops entities of rules that are no
// longer configured and entities whose identity does not match the rule's
// identity keys or the state key. It then enforces max_entities and
// max_state_bytes, keeping the most active entities. The state takes
// ownership of the snapshot's slices.
func (s *state) restore(snap stateSnapshot) (int, int) {
	type item struct {
		rule int
		key  string
		e    entity
	}
	var items []item
	dropped := 0
	for name, entities := range snap.Rules {
		ri, ok := s.ruleIndex[name]
		if !ok {
			dropped += len(entities)
			continue
		}
		for key, e := range entities {
			if !s.rules[ri].valid(key, &e) {
				dropped++
				continue
			}
			items = append(items, item{rule: ri, key: key, e: e})
		}
	}
	// Insert the least active first, so that the limits evict them.
	slices.SortFunc(items, func(a, b item) int {
		return cmp.Or(
			cmp.Compare(a.e.Activity, b.e.Activity),
			cmp.Compare(s.rules[a.rule].name, s.rules[b.rule].name),
			cmp.Compare(a.key, b.key),
		)
	})

	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.rules {
		s.rules[i].entities = make(map[string]*storedEntity)
	}
	s.bytes = 0
	for _, it := range items {
		r := s.insertLocked(it.rule, it.key, it.e)
		dropped += len(r.evicted)
		if r.outcome == outcomeTooLarge {
			dropped++
		}
	}
	kept := 0
	for i := range s.rules {
		kept += len(s.rules[i].entities)
	}
	return kept, dropped
}

// valid reports whether a restored entity belongs to the rule: its identity
// keys are the rule's identity keys in order, its values are not empty, and
// key is their encoding.
func (rs *ruleState) valid(key string, e *entity) bool {
	if len(e.Identity) != len(rs.identity) {
		return false
	}
	for i, p := range e.Identity {
		if p.Key != rs.identity[i] || p.Value == "" {
			return false
		}
	}
	return key == encodeIdentity(e.Identity)
}
