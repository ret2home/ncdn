package main

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	DefaultAnalysisMaxRequestIds = 10000

	AnalysisResultHit       = "HIT"
	AnalysisResultMiss      = "MISS"
	AnalysisResultCollapsed = "COLLAPSED"
	AnalysisResultSWR       = "SWR"
	AnalysisResultSIE       = "SIE"

	AnalysisStateHeaderWaiting = "HEADER_WAITING"
	AnalysisStateBodyWaiting   = "BODY_WAITING"
	AnalysisStateDone          = "DONE"
)

type AnalysisState struct {
	Enabled atomic.Bool `json:"-"`

	mu            sync.RWMutex
	nextRequestId uint64
	MaxRequestIds uint64               `json:"maxRequestIds"`
	StartedAt     time.Time            `json:"startedAt"`
	StoppedAt     *time.Time           `json:"stoppedAt"`
	RequestBytes  int64                `json:"requestBytes"`
	OriginBytes   int64                `json:"originBytes"`
	Keys          map[string]*KeyTrace `json:"keys"`
}

type AnalysisSnapshot struct {
	Enabled       bool                 `json:"enabled"`
	MaxRequestIds uint64               `json:"maxRequestIds"`
	StartedAt     time.Time            `json:"startedAt"`
	StoppedAt     *time.Time           `json:"stoppedAt"`
	RequestBytes  int64                `json:"requestBytes"`
	OriginBytes   int64                `json:"originBytes"`
	Keys          map[string]*KeyTrace `json:"keys"`
}

type KeyTrace struct {
	Requests []*Trace `json:"requests"`
	Origins  []*Trace `json:"origins"`
}

type Trace struct {
	TraceIdentity
	RequestTraceFields
	HeaderTrace
	TraceProgress
}

type TraceIdentity struct {
	RequestId uint64 `json:"requestId"`
	CacheKey  string `json:"cacheKey"`
	Start     int64  `json:"start"`
	End       int64  `json:"end"`
}

type RequestTraceFields struct {
	Result            string `json:"result"`
	ProducerRequestId uint64 `json:"producerRequestId"`
	ProducerCacheKey  string `json:"producerCacheKey"`
}

type HeaderTrace struct {
	ContentLength        int64  `json:"contentLength"`
	StatusCode           int    `json:"statusCode"`
	RequestCacheControl  string `json:"requestCacheControl"`
	ResponseCacheControl string `json:"responseCacheControl"`
}

type TraceProgress struct {
	ProducedBytes int64      `json:"producedBytes"`
	StartTime     time.Time  `json:"startTime"`
	HeaderTime    *time.Time `json:"headerTime"`
	EndTime       *time.Time `json:"endTime"`
	State         string     `json:"state"`
}

type AnalysisTraceRef struct {
	a      *AnalysisState
	trace  *Trace
	origin bool
}

func NewAnalysisState() *AnalysisState {
	return &AnalysisState{
		MaxRequestIds: DefaultAnalysisMaxRequestIds,
		Keys:          map[string]*KeyTrace{},
	}
}

func (a *AnalysisState) Start(now time.Time, maxRequestIds uint64) {
	a.mu.Lock()
	a.nextRequestId = 0
	a.MaxRequestIds = maxRequestIds
	a.StartedAt = now
	a.StoppedAt = nil
	a.RequestBytes = 0
	a.OriginBytes = 0
	a.Keys = map[string]*KeyTrace{}
	a.mu.Unlock()

	a.Enabled.Store(true)
}

func (a *AnalysisState) Stop(now time.Time) {
	a.Enabled.Store(false)

	a.mu.Lock()
	a.StoppedAt = &now
	a.mu.Unlock()
}

func (a *AnalysisState) IsEnabled() bool {
	return a != nil && a.Enabled.Load()
}

func (a *AnalysisState) BeginRequest(requestId uint64, cacheKey string, start, end int64, requestCacheControl string) AnalysisTraceRef {
	if !a.IsEnabled() || cacheKey == "" {
		return AnalysisTraceRef{}
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.Enabled.Load() {
		return AnalysisTraceRef{}
	}

	if requestId == 0 {
		a.nextRequestId++
		requestId = a.nextRequestId
	}

	req := &Trace{
		TraceIdentity: TraceIdentity{
			RequestId: requestId,
			CacheKey:  cacheKey,
			Start:     start,
			End:       end,
		},
		HeaderTrace: HeaderTrace{
			RequestCacheControl: requestCacheControl,
		},
		TraceProgress: TraceProgress{
			StartTime: time.Now(),
			State:     AnalysisStateHeaderWaiting,
		},
	}

	key := a.Keys[cacheKey]
	if key == nil {
		key = &KeyTrace{}
		a.Keys[cacheKey] = key
	}
	key.Requests = append(key.Requests, req)
	a.pruneLocked()

	return AnalysisTraceRef{
		a:      a,
		trace:  req,
		origin: false,
	}
}

func (a *AnalysisState) StartOrigin(ref AnalysisTraceRef) AnalysisTraceRef {
	if !a.validTraceRef(ref) {
		return AnalysisTraceRef{}
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	origin := &Trace{
		TraceIdentity: TraceIdentity{
			RequestId: ref.trace.RequestId,
			CacheKey:  ref.trace.CacheKey,
			Start:     ref.trace.Start,
			End:       ref.trace.End,
		},
		HeaderTrace: HeaderTrace{
			RequestCacheControl: ref.trace.RequestCacheControl,
		},
		TraceProgress: TraceProgress{
			StartTime: time.Now(),
			State:     AnalysisStateHeaderWaiting,
		},
	}

	key := a.Keys[ref.trace.CacheKey]
	if key == nil {
		key = &KeyTrace{}
		a.Keys[ref.trace.CacheKey] = key
	}
	key.Origins = append(key.Origins, origin)
	a.pruneLocked()

	return AnalysisTraceRef{
		a:      a,
		trace:  origin,
		origin: true,
	}
}

func (a *AnalysisState) SetRequestResult(ref AnalysisTraceRef, result string, producer AnalysisTraceRef) {
	if !a.validTraceRef(ref) {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	ref.trace.Result = result
	if producer.trace != nil {
		ref.trace.ProducerRequestId = producer.trace.RequestId
		ref.trace.ProducerCacheKey = producer.trace.CacheKey
	}
}

func (a *AnalysisState) UpdateRequestResult(ref AnalysisTraceRef, result string) {
	if !a.validTraceRef(ref) {
		return
	}

	a.mu.Lock()
	ref.trace.Result = result
	a.mu.Unlock()
}

func (a *AnalysisState) SetHeader(ref AnalysisTraceRef, statusCode int, header http.Header) {
	if !a.validTraceRef(ref) {
		return
	}

	now := time.Now()

	a.mu.Lock()
	ref.trace.StatusCode = statusCode
	ref.trace.ResponseCacheControl = strings.Join(header.Values("Cache-Control"), ",")
	ref.trace.HeaderTime = &now
	ref.trace.State = AnalysisStateBodyWaiting
	if contentLength, err := strconv.ParseInt(header.Get("Content-Length"), 10, 64); err == nil && contentLength > 0 {
		ref.trace.ContentLength = contentLength
	}
	a.mu.Unlock()
}

func (a *AnalysisState) AddBytes(ref AnalysisTraceRef, n int64) {
	if !a.validTraceRef(ref) || n <= 0 {
		return
	}

	a.mu.Lock()
	ref.trace.ProducedBytes += n
	if ref.origin {
		a.OriginBytes += n
	} else {
		a.RequestBytes += n
	}
	a.mu.Unlock()
}

func (a *AnalysisState) Finish(ref AnalysisTraceRef) {
	if !a.validTraceRef(ref) {
		return
	}

	now := time.Now()

	a.mu.Lock()
	ref.trace.EndTime = &now
	ref.trace.State = AnalysisStateDone
	a.mu.Unlock()
}

func (a *AnalysisState) Snapshot() AnalysisSnapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return AnalysisSnapshot{
		Enabled:       a.Enabled.Load(),
		MaxRequestIds: a.MaxRequestIds,
		StartedAt:     a.StartedAt,
		StoppedAt:     cloneTime(a.StoppedAt),
		RequestBytes:  a.RequestBytes,
		OriginBytes:   a.OriginBytes,
		Keys:          cloneKeys(a.Keys),
	}
}

func (a *AnalysisState) validTraceRef(ref AnalysisTraceRef) bool {
	return a != nil && a.Enabled.Load() && ref.a == a && ref.trace != nil
}

func (a *AnalysisState) pruneLocked() {
	if a.MaxRequestIds == 0 || a.nextRequestId <= a.MaxRequestIds {
		return
	}

	minRequestId := a.nextRequestId - a.MaxRequestIds + 1
	for cacheKey, key := range a.Keys {
		key.Requests = keepRecentTraces(key.Requests, minRequestId)
		key.Origins = keepRecentTraces(key.Origins, minRequestId)
		if len(key.Requests) == 0 && len(key.Origins) == 0 {
			delete(a.Keys, cacheKey)
		}
	}
}

func keepRecentTraces(traces []*Trace, minRequestId uint64) []*Trace {
	n := 0
	for _, trace := range traces {
		if trace.RequestId >= minRequestId {
			traces[n] = trace
			n++
		}
	}
	return traces[:n]
}

func cloneKeys(keys map[string]*KeyTrace) map[string]*KeyTrace {
	cloned := make(map[string]*KeyTrace, len(keys))
	for cacheKey, key := range keys {
		cloned[cacheKey] = &KeyTrace{
			Requests: cloneTraces(key.Requests),
			Origins:  cloneTraces(key.Origins),
		}
	}
	return cloned
}

func cloneTraces(traces []*Trace) []*Trace {
	cloned := make([]*Trace, 0, len(traces))
	for _, trace := range traces {
		if trace == nil {
			continue
		}
		item := *trace
		item.HeaderTime = cloneTime(trace.HeaderTime)
		item.EndTime = cloneTime(trace.EndTime)
		cloned = append(cloned, &item)
	}
	return cloned
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	cloned := *t
	return &cloned
}
