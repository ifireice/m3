// Copyright (c) 2016 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package aggregator

import (
	"container/list"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/m3db/m3aggregator/hash"
	"github.com/m3db/m3aggregator/rate"
	"github.com/m3db/m3aggregator/runtime"
	"github.com/m3db/m3metrics/metadata"
	"github.com/m3db/m3metrics/metric/unaggregated"
	"github.com/m3db/m3x/clock"
	"github.com/m3db/m3x/close"

	"github.com/uber-go/tally"
)

const (
	defaultSoftDeadlineCheckEvery = 128
	defaultExpireBatchSize        = 1024
)

var (
	emptyHashedEntry                   hashedEntry
	errMetricMapClosed                 = errors.New("metric map is already closed")
	errWriteNewMetricRateLimitExceeded = errors.New("write new metric rate limit is exceeded")
)

type entryKey struct {
	metricType unaggregated.Type
	idHash     hash.Hash128
}

type hashedEntry struct {
	key   entryKey
	entry *Entry
}

type metricMapMetrics struct {
	newEntries                 tally.Counter
	noRateLimitWarmup          tally.Counter
	newMetricRateLimitExceeded tally.Counter
	droppedNewMetrics          tally.Counter
}

func newMetricMapMetrics(scope tally.Scope) metricMapMetrics {
	return metricMapMetrics{
		newEntries:                 scope.Counter("new-entries"),
		noRateLimitWarmup:          scope.Counter("no-rate-limit-warmup"),
		newMetricRateLimitExceeded: scope.Counter("new-metric-rate-limit-exceeded"),
		droppedNewMetrics:          scope.Counter("dropped-new-metrics"),
	}
}

// NB(xichen): use a type-specific list for hashedEntry if the conversion
// overhead between interface{} and hashedEntry becomes a problem.
type metricMap struct {
	sync.RWMutex

	shard        uint32
	opts         Options
	nowFn        clock.NowFn
	entryPool    EntryPool
	batchPercent float64

	closed            bool
	metricLists       *metricLists
	entries           map[entryKey]*list.Element
	entryList         *list.List
	entryListDelLock  sync.Mutex // Must be held when deleting elements from the entry list
	firstInsertAt     time.Time
	rateLimiter       *rate.Limiter
	runtimeOpts       runtime.Options
	runtimeOptsCloser close.SimpleCloser
	sleepFn           sleepFn
	metrics           metricMapMetrics
}

func newMetricMap(shard uint32, opts Options) *metricMap {
	metricLists := newMetricLists(shard, opts)
	scope := opts.InstrumentOptions().MetricsScope().SubScope("map")
	m := &metricMap{
		shard:        shard,
		opts:         opts,
		nowFn:        opts.ClockOptions().NowFn(),
		entryPool:    opts.EntryPool(),
		batchPercent: opts.EntryCheckBatchPercent(),
		metricLists:  metricLists,
		entries:      make(map[entryKey]*list.Element),
		entryList:    list.New(),
		sleepFn:      time.Sleep,
		metrics:      newMetricMapMetrics(scope),
	}

	runtimeOptsManager := opts.RuntimeOptionsManager()
	runtimeOpts := runtimeOptsManager.RuntimeOptions()
	m.Lock()
	m.resetRateLimiterWithLock(runtimeOpts)
	m.Unlock()

	// Register the metric map as a runtime options watcher.
	closer := runtimeOptsManager.RegisterWatcher(m)
	m.runtimeOptsCloser = closer

	return m
}

func (m *metricMap) AddUntimed(
	metric unaggregated.MetricUnion,
	metadatas metadata.StagedMetadatas,
) error {
	key := entryKey{
		metricType: metric.Type,
		idHash:     hash.Murmur3Hash128(metric.ID),
	}
	entry, err := m.findOrCreate(key)
	if err != nil {
		return err
	}
	err = entry.AddUntimed(metric, metadatas)
	entry.DecWriter()
	return err
}

func (m *metricMap) Tick(target time.Duration) tickResult {
	expiredEntries := m.deleteExpired(target)

	m.RLock()
	activeEntries := m.entryList.Len()
	m.RUnlock()

	activeElems := m.metricLists.Tick()

	return tickResult{
		ActiveEntries:  activeEntries,
		ExpiredEntries: expiredEntries,
		ActiveElems:    activeElems,
	}
}

func (m *metricMap) SetRuntimeOptions(opts runtime.Options) {
	m.Lock()
	m.runtimeOpts = opts
	m.resetRateLimiterWithLock(opts)
	m.Unlock()

	// NB(xichen): we hold onto the entry list deletion lock here to ensure no
	// entries get deleted while we iterate over the list, otherwise we may update
	// entries that have expired. This only affects the ticking goroutine as that's
	// the only goroutine deleting entries from the list, which is not performance
	// sensitive. Entries can still be inserted into the map and the entry list in
	// the meantime. The entry list deletion lock must be held before the map lock
	// to avoid deadlocks.
	m.entryListDelLock.Lock()
	m.forEachEntry(func(entry hashedEntry) {
		entry.entry.SetRuntimeOptions(opts)
	})
	m.entryListDelLock.Unlock()
}

func (m *metricMap) Close() {
	m.Lock()
	defer m.Unlock()

	if m.closed {
		return
	}
	m.runtimeOptsCloser.Close()
	m.metricLists.Close()
	m.closed = true
}

func (m *metricMap) findOrCreate(key entryKey) (*Entry, error) {
	m.RLock()
	if m.closed {
		m.RUnlock()
		return nil, errMetricMapClosed
	}
	if entry, found := m.lookupEntryWithLock(key); found {
		// NB(xichen): it is important to increase number of writers
		// within a lock so we can account for active writers
		// when deleting expired entries.
		entry.IncWriter()
		m.RUnlock()
		return entry, nil
	}
	m.RUnlock()

	m.Lock()
	if m.closed {
		m.Unlock()
		return nil, errMetricMapClosed
	}
	entry, found := m.lookupEntryWithLock(key)
	if found {
		entry.IncWriter()
		m.Unlock()
		return entry, nil
	}

	// Check if we are allowed to insert a new metric.
	now := m.nowFn()
	if m.firstInsertAt.IsZero() {
		m.firstInsertAt = now
	}
	if err := m.applyNewMetricRateLimitWithLock(now); err != nil {
		m.Unlock()
		return nil, err
	}
	entry = m.entryPool.Get()
	entry.ResetSetData(m.metricLists, m.runtimeOpts, m.opts)
	m.entries[key] = m.entryList.PushBack(hashedEntry{
		key:   key,
		entry: entry,
	})
	entry.IncWriter()
	m.Unlock()
	m.metrics.newEntries.Inc(1)

	return entry, nil
}

func (m *metricMap) lookupEntryWithLock(key entryKey) (*Entry, bool) {
	elem, exists := m.entries[key]
	if !exists {
		return nil, false
	}
	return elem.Value.(hashedEntry).entry, true
}

func (m *metricMap) deleteExpired(target time.Duration) int {
	// Determine batch size.
	m.RLock()
	numEntries := m.entryList.Len()
	m.RUnlock()
	if numEntries == 0 {
		return 0
	}

	var (
		start                = m.nowFn()
		perEntrySoftDeadline = target / time.Duration(numEntries)
		expired              []hashedEntry
		numExpired           int
		entryIdx             int
	)
	m.forEachEntry(func(entry hashedEntry) {
		now := m.nowFn()
		if entryIdx > 0 && entryIdx%defaultSoftDeadlineCheckEvery == 0 {
			targetDeadline := start.Add(time.Duration(entryIdx) * perEntrySoftDeadline)
			if now.Before(targetDeadline) {
				m.sleepFn(targetDeadline.Sub(now))
			}
		}
		if entry.entry.ShouldExpire(now) {
			expired = append(expired, entry)
		}
		if len(expired) >= defaultExpireBatchSize {
			numExpired += m.purgeExpired(now, expired)
			for i := range expired {
				expired[i] = emptyHashedEntry
			}
			expired = expired[:0]
		}
		entryIdx++
	})

	// Purge remaining expired entries.
	numExpired += m.purgeExpired(m.nowFn(), expired)
	for i := range expired {
		expired[i] = emptyHashedEntry
	}
	return numExpired
}

func (m *metricMap) purgeExpired(now time.Time, entries []hashedEntry) int {
	if len(entries) == 0 {
		return 0
	}
	var numExpired int
	m.entryListDelLock.Lock()
	m.Lock()
	for i := range entries {
		if entries[i].entry.TryExpire(now) {
			elem := m.entries[entries[i].key]
			delete(m.entries, entries[i].key)
			elem.Value = nil
			m.entryList.Remove(elem)
			numExpired++
		}
	}
	m.Unlock()
	m.entryListDelLock.Unlock()
	return numExpired
}

func (m *metricMap) forEachEntry(entryFn hashedEntryFn) {
	// Determine batch size.
	m.RLock()
	elemsLen := m.entryList.Len()
	if elemsLen == 0 {
		// If the list is empty, nothing to do.
		m.RUnlock()
		return
	}
	batchSize := int(math.Max(1.0, math.Ceil(m.batchPercent*float64(elemsLen))))
	currElem := m.entryList.Front()
	m.RUnlock()

	currEntries := make([]hashedEntry, 0, batchSize)
	for currElem != nil {
		m.RLock()
		for numChecked := 0; numChecked < batchSize && currElem != nil; numChecked++ {
			nextElem := currElem.Next()
			hashedEntry := currElem.Value.(hashedEntry)
			currEntries = append(currEntries, hashedEntry)
			currElem = nextElem
		}
		m.RUnlock()

		for _, entry := range currEntries {
			entryFn(entry)
		}
		for i := range currEntries {
			currEntries[i] = emptyHashedEntry
		}
		currEntries = currEntries[:0]
	}
}

func (m *metricMap) resetRateLimiterWithLock(runtimeOpts runtime.Options) {
	newLimit := runtimeOpts.WriteNewMetricLimitPerShardPerSecond()
	if newLimit <= 0 {
		m.rateLimiter = nil
		return
	}
	if m.rateLimiter == nil {
		nowFn := m.opts.ClockOptions().NowFn()
		m.rateLimiter = rate.NewLimiter(newLimit, nowFn)
		return
	}
	m.rateLimiter.Reset(newLimit)
}

func (m *metricMap) applyNewMetricRateLimitWithLock(now time.Time) error {
	if m.rateLimiter == nil {
		return nil
	}
	// If we are still in the warmup phase and possibly ingesting a large amount
	// of new metrics, no rate limit is applied.
	noLimitWarmupDuration := m.runtimeOpts.WriteNewMetricNoLimitWarmupDuration()
	if warmupEnd := m.firstInsertAt.Add(noLimitWarmupDuration); now.Before(warmupEnd) {
		m.metrics.noRateLimitWarmup.Inc(1)
		return nil
	}
	if m.rateLimiter.IsAllowed(1) {
		return nil
	}
	m.metrics.newMetricRateLimitExceeded.Inc(1)
	m.metrics.droppedNewMetrics.Inc(1)
	return errWriteNewMetricRateLimitExceeded
}

type hashedEntryFn func(hashedEntry)