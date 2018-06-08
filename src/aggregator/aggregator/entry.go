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
	"sync"
	"sync/atomic"
	"time"

	"github.com/m3db/m3aggregator/bitset"
	"github.com/m3db/m3aggregator/rate"
	"github.com/m3db/m3aggregator/runtime"
	"github.com/m3db/m3metrics/aggregation"
	"github.com/m3db/m3metrics/metadata"
	metricid "github.com/m3db/m3metrics/metric/id"
	"github.com/m3db/m3metrics/metric/unaggregated"
	"github.com/m3db/m3metrics/op/applied"
	"github.com/m3db/m3metrics/policy"
	xerrors "github.com/m3db/m3x/errors"

	"github.com/uber-go/tally"
)

const (
	// initialAggregationCapacity is the initial number of slots
	// allocated for aggregation metadata.
	initialAggregationCapacity = 2
)

var (
	errEmptyMetadatas              = errors.New("empty metadata list")
	errNoApplicableMetadata        = errors.New("no applicable metadata")
	errNoPipelinesInMetadata       = errors.New("no pipelines in metadata")
	errEntryClosed                 = errors.New("entry is closed")
	errWriteValueRateLimitExceeded = errors.New("write value rate limit is exceeded")
)

type entryMetrics struct {
	emptyMetadatas          tally.Counter
	noApplicableMetadata    tally.Counter
	noPipelinesInMetadata   tally.Counter
	emptyPipeline           tally.Counter
	noAggregationInPipeline tally.Counter
	valueRateLimitExceeded  tally.Counter
	droppedValues           tally.Counter
	staleMetadata           tally.Counter
	tombstonedMetadata      tally.Counter
	metadataUpdates         tally.Counter
}

func newEntryMetrics(scope tally.Scope) entryMetrics {
	return entryMetrics{
		emptyMetadatas:          scope.Counter("empty-metadatas"),
		noApplicableMetadata:    scope.Counter("no-applicable-metadata"),
		noPipelinesInMetadata:   scope.Counter("no-pipelines-in-metadata"),
		emptyPipeline:           scope.Counter("empty-pipeline"),
		noAggregationInPipeline: scope.Counter("no-aggregation-in-pipeline"),
		valueRateLimitExceeded:  scope.Counter("value-rate-limit-exceeded"),
		droppedValues:           scope.Counter("dropped-values"),
		staleMetadata:           scope.Counter("stale-metadata"),
		tombstonedMetadata:      scope.Counter("tombstoned-metadata"),
		metadataUpdates:         scope.Counter("metadata-updates"),
	}
}

// Entry keeps track of a metric's aggregations alongside the aggregation
// metadatas including storage policies, aggregation types, and remaining pipeline
// steps if any.
type Entry struct {
	sync.RWMutex

	closed              bool
	opts                Options
	rateLimiter         *rate.Limiter
	hasDefaultMetadatas bool
	cutoverNanos        int64
	lists               *metricLists
	numWriters          int32
	lastAccessNanos     int64
	aggregations        aggregationValues
	metrics             entryMetrics
	// The entry keeps a decompressor to reuse the bitset in it, so we can
	// save some heap allocations.
	decompressor aggregation.IDDecompressor
}

// NewEntry creates a new entry.
func NewEntry(lists *metricLists, runtimeOpts runtime.Options, opts Options) *Entry {
	scope := opts.InstrumentOptions().MetricsScope().SubScope("entry")
	e := &Entry{
		aggregations: make(aggregationValues, 0, initialAggregationCapacity),
		metrics:      newEntryMetrics(scope),
		decompressor: aggregation.NewPooledIDDecompressor(opts.AggregationTypesOptions().TypesPool()),
	}
	e.ResetSetData(lists, runtimeOpts, opts)
	return e
}

// IncWriter increases the writer count.
func (e *Entry) IncWriter() { atomic.AddInt32(&e.numWriters, 1) }

// DecWriter decreases the writer count.
func (e *Entry) DecWriter() { atomic.AddInt32(&e.numWriters, -1) }

// ResetSetData resets the entry and sets initial data.
// NB(xichen): we need to reset the options here to use the correct
// time lock contained in the options.
func (e *Entry) ResetSetData(lists *metricLists, runtimeOpts runtime.Options, opts Options) {
	e.Lock()
	e.closed = false
	e.opts = opts
	e.resetRateLimiterWithLock(runtimeOpts)
	e.hasDefaultMetadatas = false
	e.cutoverNanos = uninitializedCutoverNanos
	e.lists = lists
	e.numWriters = 0
	e.recordLastAccessed(e.opts.ClockOptions().NowFn()())
	e.Unlock()
}

// SetRuntimeOptions updates the parameters of the rate limiter.
func (e *Entry) SetRuntimeOptions(opts runtime.Options) {
	e.Lock()
	if e.closed {
		e.Unlock()
		return
	}
	e.resetRateLimiterWithLock(opts)
	e.Unlock()
}

// AddUntimed adds an untimed metric along with its metadatas.
func (e *Entry) AddUntimed(
	metric unaggregated.MetricUnion,
	metadatas metadata.StagedMetadatas,
) error {
	switch metric.Type {
	case unaggregated.BatchTimerType:
		var err error
		if err = e.applyValueRateLimit(int64(len(metric.BatchTimerVal))); err == nil {
			err = e.writeBatchTimerWithMetadatas(metric, metadatas)
		}
		if metric.BatchTimerVal != nil && metric.TimerValPool != nil {
			metric.TimerValPool.Put(metric.BatchTimerVal)
		}
		return err
	default:
		// For counters and gauges, there is a single value in the metric union.
		if err := e.applyValueRateLimit(1); err != nil {
			return err
		}
		return e.addUntimed(metric, metadatas)
	}
}

func (e *Entry) writeBatchTimerWithMetadatas(
	metric unaggregated.MetricUnion,
	metadatas metadata.StagedMetadatas,
) error {
	// If there is no limit on the maximum batch size per write, write
	// all timers at once.
	maxTimerBatchSizePerWrite := e.opts.MaxTimerBatchSizePerWrite()
	if maxTimerBatchSizePerWrite == 0 {
		return e.addUntimed(metric, metadatas)
	}

	// Otherwise, honor maximum timer batch size.
	var (
		timerValues    = metric.BatchTimerVal
		numTimerValues = len(timerValues)
		start, end     int
	)
	for start = 0; start < numTimerValues; start = end {
		end = start + maxTimerBatchSizePerWrite
		if end > numTimerValues {
			end = numTimerValues
		}
		splitTimer := metric
		splitTimer.BatchTimerVal = timerValues[start:end]
		if err := e.addUntimed(splitTimer, metadatas); err != nil {
			return err
		}
	}
	return nil
}

func (e *Entry) addUntimed(
	metric unaggregated.MetricUnion,
	metadatas metadata.StagedMetadatas,
) error {
	timeLock := e.opts.TimeLock()
	timeLock.RLock()

	// NB(xichen): it is important that we determine the current time
	// within the time lock. This ensures time ordering by wrapping
	// actions that need to happen before a given time within a read lock,
	// so it is guaranteed that actions before when a write lock is acquired
	// must have all completed. This is used to ensure we never write metrics
	// for times that have already been flushed.
	currTime := e.opts.ClockOptions().NowFn()()
	e.recordLastAccessed(currTime)

	e.RLock()
	if e.closed {
		e.RUnlock()
		timeLock.RUnlock()
		return errEntryClosed
	}

	// Fast exit path for the common case where the metric has default metadatas for aggregation.
	hasDefaultMetadatas := metadatas.IsDefault()
	if e.hasDefaultMetadatas && hasDefaultMetadatas {
		err := e.addMetricWithLock(currTime, metric)
		e.RUnlock()
		timeLock.RUnlock()
		return err
	}

	sm, err := e.activeStagedMetadataWithLock(currTime, metadatas)
	if err != nil {
		e.RUnlock()
		timeLock.RUnlock()
		return err
	}

	// If the metadata indicates the (rollup) metric has been tombstoned, the metric is
	// not ingested for aggregation. However, we do not update the policies asssociated
	// with this entry and mark it tombstoned because there may be a different raw metric
	// generating this same (rollup) metric that is actively emitting, meaning this entry
	// may still be very much alive.
	if sm.Tombstoned {
		e.RUnlock()
		timeLock.RUnlock()
		e.metrics.tombstonedMetadata.Inc(1)
		return nil
	}

	// It is expected that there is at least one pipeline in the metadata.
	if len(sm.Pipelines) == 0 {
		e.RUnlock()
		timeLock.RUnlock()
		e.metrics.noPipelinesInMetadata.Inc(1)
		return errNoPipelinesInMetadata
	}

	if !e.shouldUpdateMetadatasWithLock(sm) {
		err = e.addMetricWithLock(currTime, metric)
		e.RUnlock()
		timeLock.RUnlock()
		return err
	}
	e.RUnlock()

	e.Lock()
	if e.closed {
		e.Unlock()
		timeLock.RUnlock()
		return errEntryClosed
	}

	if e.shouldUpdateMetadatasWithLock(sm) {
		if err = e.updateMetadatasWithLock(metric, hasDefaultMetadatas, sm); err != nil {
			// NB(xichen): if an error occurred during policy update, the policies
			// will remain as they are, i.e., there are no half-updated policies.
			e.Unlock()
			timeLock.RUnlock()
			return err
		}
	}

	err = e.addMetricWithLock(currTime, metric)
	e.Unlock()
	timeLock.RUnlock()

	return err
}

// ShouldExpire returns whether the entry should expire.
func (e *Entry) ShouldExpire(now time.Time) bool {
	e.RLock()
	if e.closed {
		e.RUnlock()
		return false
	}
	e.RUnlock()

	return e.shouldExpire(now)
}

// TryExpire attempts to expire the entry, returning true
// if the entry is expired, and false otherwise.
func (e *Entry) TryExpire(now time.Time) bool {
	e.Lock()
	if e.closed {
		e.Unlock()
		return false
	}
	if !e.shouldExpire(now) {
		e.Unlock()
		return false
	}
	e.closed = true
	// Empty out the aggregation elements so they don't hold references
	// to other objects after being put back to pool to reduce GC overhead.
	for i := range e.aggregations {
		e.aggregations[i].elem.Value.(metricElem).MarkAsTombstoned()
		e.aggregations[i] = aggregationValue{}
	}
	e.aggregations = e.aggregations[:0]
	e.lists = nil
	pool := e.opts.EntryPool()
	e.Unlock()

	pool.Put(e)
	return true
}

func (e *Entry) writerCount() int        { return int(atomic.LoadInt32(&e.numWriters)) }
func (e *Entry) lastAccessed() time.Time { return time.Unix(0, atomic.LoadInt64(&e.lastAccessNanos)) }

func (e *Entry) recordLastAccessed(currTime time.Time) {
	atomic.StoreInt64(&e.lastAccessNanos, currTime.UnixNano())
}

// NB(xichen): we assume the metadatas are sorted by their cutover times
// in ascending order.
func (e *Entry) activeStagedMetadataWithLock(
	t time.Time,
	metadatas metadata.StagedMetadatas,
) (metadata.StagedMetadata, error) {
	// If we have no metadata to apply, simply bail.
	if len(metadatas) == 0 {
		e.metrics.emptyMetadatas.Inc(1)
		return metadata.DefaultStagedMetadata, errEmptyMetadatas
	}
	timeNanos := t.UnixNano()
	for idx := len(metadatas) - 1; idx >= 0; idx-- {
		if metadatas[idx].CutoverNanos <= timeNanos {
			return metadatas[idx], nil
		}
	}
	e.metrics.noApplicableMetadata.Inc(1)
	return metadata.DefaultStagedMetadata, errNoApplicableMetadata
}

// NB: The metadata passed in is guaranteed to have cut over based on the current time.
func (e *Entry) shouldUpdateMetadatasWithLock(sm metadata.StagedMetadata) bool {
	// If this is a stale metadata, we don't update the existing metadata.
	if e.cutoverNanos > sm.CutoverNanos {
		e.metrics.staleMetadata.Inc(1)
		return false
	}

	// If this is a newer metadata, we always update.
	if e.cutoverNanos < sm.CutoverNanos {
		return true
	}

	// Iterate over the list of pipelines and check whether we have metadata changes.
	// NB: If the incoming metadata have the same set of aggregation keys as the cached
	// set but also have duplicates, there is no need to update metadatas as long as
	// the cached set has all aggregation keys in the incoming metadata and vice versa.
	bs := bitset.New(uint(len(e.aggregations)))
	for _, pipeline := range sm.Pipelines {
		storagePolicies := e.storagePolicies(pipeline.StoragePolicies)
		for _, storagePolicy := range storagePolicies {
			key := aggregationKey{
				aggregationID: pipeline.AggregationID,
				storagePolicy: storagePolicy,
				pipeline:      pipeline.Pipeline,
			}
			idx := e.aggregations.index(key)
			if idx < 0 {
				return true
			}
			bs.Set(uint(idx))
		}
	}
	return !bs.All(uint(len(e.aggregations)))
}

func (e *Entry) storagePolicies(policies []policy.StoragePolicy) []policy.StoragePolicy {
	if !policy.IsDefaultStoragePolicies(policies) {
		return policies
	}
	return e.opts.DefaultStoragePolicies()
}

func (e *Entry) maybeCopyIDWithLock(metric unaggregated.MetricUnion) metricid.RawID {
	// If we own the ID, there is no need to copy.
	if metric.OwnsID {
		return metric.ID
	}

	// If there are existing elements for this id, try reusing
	// the id from the elements because those are owned by us.
	if len(e.aggregations) > 0 {
		return e.aggregations[0].elem.Value.(metricElem).ID()
	}

	// Otherwise it is necessary to make a copy because it's not owned by us.
	elemID := make(metricid.RawID, len(metric.ID))
	copy(elemID, metric.ID)
	return elemID
}

func (e *Entry) updateMetadatasWithLock(
	metric unaggregated.MetricUnion,
	hasDefaultMetadatas bool,
	sm metadata.StagedMetadata,
) error {
	var (
		elemID          = e.maybeCopyIDWithLock(metric)
		newAggregations = make(aggregationValues, 0, initialAggregationCapacity)
	)

	// Update the metadatas.
	for _, pipeline := range sm.Pipelines {
		storagePolicies := e.storagePolicies(pipeline.StoragePolicies)
		for _, storagePolicy := range storagePolicies {
			key := aggregationKey{
				aggregationID: pipeline.AggregationID,
				storagePolicy: storagePolicy,
				pipeline:      pipeline.Pipeline,
			}
			// Remove duplicate aggregation pipelines.
			if newAggregations.contains(key) {
				continue
			}
			if idx := e.aggregations.index(key); idx >= 0 {
				newAggregations = append(newAggregations, e.aggregations[idx])
			} else {
				aggTypes, err := e.decompressor.Decompress(key.aggregationID)
				if err != nil {
					return err
				}
				var newElem metricElem
				switch metric.Type {
				case unaggregated.CounterType:
					newElem = e.opts.CounterElemPool().Get()
				case unaggregated.BatchTimerType:
					newElem = e.opts.TimerElemPool().Get()
				case unaggregated.GaugeType:
					newElem = e.opts.GaugeElemPool().Get()
				default:
					return errInvalidMetricType
				}
				// NB: The pipeline may not be owned by us and as such we need to make a copy here.
				key.pipeline = key.pipeline.Clone()
				if err = newElem.ResetSetData(elemID, storagePolicy, aggTypes, key.pipeline); err != nil {
					return err
				}
				list, err := e.lists.FindOrCreate(storagePolicy.Resolution().Window)
				if err != nil {
					return err
				}
				newListElem, err := list.PushBack(newElem)
				if err != nil {
					return err
				}
				newAggregations = append(newAggregations, aggregationValue{key: key, elem: newListElem})
			}
		}
	}

	// Mark the outdated elements as tombstoned.
	for _, val := range e.aggregations {
		if !newAggregations.contains(val.key) {
			val.elem.Value.(metricElem).MarkAsTombstoned()
		}
	}

	// Replace the existing aggregations with new aggregations.
	e.aggregations = newAggregations
	e.hasDefaultMetadatas = hasDefaultMetadatas
	e.cutoverNanos = sm.CutoverNanos
	e.metrics.metadataUpdates.Inc(1)

	return nil
}

func (e *Entry) addMetricWithLock(timestamp time.Time, mu unaggregated.MetricUnion) error {
	multiErr := xerrors.NewMultiError()
	for _, val := range e.aggregations {
		if err := val.elem.Value.(metricElem).AddMetric(timestamp, mu); err != nil {
			multiErr = multiErr.Add(err)
		}
	}
	return multiErr.FinalError()
}

func (e *Entry) shouldExpire(now time.Time) bool {
	// Only expire the entry if there are no active writers
	// and it has reached its ttl since last accessed.
	return e.writerCount() == 0 && now.After(e.lastAccessed().Add(e.opts.EntryTTL()))
}

func (e *Entry) resetRateLimiterWithLock(runtimeOpts runtime.Options) {
	newLimit := runtimeOpts.WriteValuesPerMetricLimitPerSecond()
	if newLimit <= 0 {
		e.rateLimiter = nil
		return
	}
	if e.rateLimiter == nil {
		nowFn := e.opts.ClockOptions().NowFn()
		e.rateLimiter = rate.NewLimiter(newLimit, nowFn)
		return
	}
	e.rateLimiter.Reset(newLimit)
}

func (e *Entry) applyValueRateLimit(numValues int64) error {
	e.RLock()
	rateLimiter := e.rateLimiter
	e.RUnlock()
	if rateLimiter == nil {
		return nil
	}
	if rateLimiter.IsAllowed(numValues) {
		return nil
	}
	e.metrics.valueRateLimitExceeded.Inc(1)
	e.metrics.droppedValues.Inc(numValues)
	return errWriteValueRateLimitExceeded
}

type aggregationKey struct {
	aggregationID aggregation.ID
	storagePolicy policy.StoragePolicy
	pipeline      applied.Pipeline
}

func (k aggregationKey) Equal(other aggregationKey) bool {
	return k.aggregationID == other.aggregationID &&
		k.storagePolicy == other.storagePolicy &&
		k.pipeline.Equal(other.pipeline)
}

type aggregationValue struct {
	key  aggregationKey
	elem *list.Element
}

// TODO(xichen): benchmark the performance of using a single slice
// versus a map with a partial key versus a map with a hash of full key.
type aggregationValues []aggregationValue

func (vals aggregationValues) index(k aggregationKey) int {
	for i, val := range vals {
		if val.key.Equal(k) {
			return i
		}
	}
	return -1
}

func (vals aggregationValues) contains(k aggregationKey) bool {
	return vals.index(k) != -1
}