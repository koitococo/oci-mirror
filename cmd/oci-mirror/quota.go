package main

import (
	"errors"
	"flag"
	"sync"
	"time"
)

var (
	errBusy      = errors.New("cold fetch limit reached")
	errCacheFull = errors.New("cache quota exhausted")
)

type quotaLimitError struct {
	dimension string
	err       error
}

func (e *quotaLimitError) Error() string { return e.err.Error() }
func (e *quotaLimitError) Unwrap() error { return e.err }

func quotaLimit(err error, dimension string) error {
	return &quotaLimitError{dimension: dimension, err: err}
}

type cacheLimits struct {
	manifestBytes, blobBytes           int64
	manifestEntries, blobEntries       int
	stagingBytes, upstreamStagingBytes int64
	fetches, upstreamFetches           int
	manifestTTL                        time.Duration
}

func cacheLimitFlags() *cacheLimits {
	l := new(cacheLimits)
	flag.Int64Var(&l.manifestBytes, "cache-manifest-bytes", 64<<20, "total pinned Manifest payload bytes")
	flag.IntVar(&l.manifestEntries, "cache-manifest-entries", 10000, "total Manifest digest records and Tag bindings")
	flag.DurationVar(&l.manifestTTL, "cache-manifest-ttl", 0, "maximum Manifest freshness before fetching upstream again (0 disables expiration)")
	flag.Int64Var(&l.blobBytes, "cache-blob-bytes", 16<<30, "total published Blob payload bytes")
	flag.IntVar(&l.blobEntries, "cache-blob-entries", 10000, "total published Blob records")
	flag.Int64Var(&l.stagingBytes, "cache-staging-bytes", 8<<30, "global simultaneous Blob staging bytes")
	flag.Int64Var(&l.upstreamStagingBytes, "cache-upstream-staging-bytes", 4<<30, "Blob staging bytes per logical upstream registry")
	flag.IntVar(&l.fetches, "cache-fetches", 4, "maximum simultaneous cold requests")
	flag.IntVar(&l.upstreamFetches, "cache-upstream-fetches", 2, "maximum simultaneous cold requests per logical upstream registry")
	return l
}

func (l cacheLimits) validate() error {
	if l.manifestBytes <= 0 || l.blobBytes <= 0 || l.manifestEntries <= 0 || l.blobEntries <= 0 ||
		l.stagingBytes <= 0 || l.upstreamStagingBytes <= 0 || l.fetches <= 0 || l.upstreamFetches <= 0 {
		return errors.New("cache quota limits must be positive")
	}
	if l.manifestTTL < 0 {
		return errors.New("cache-manifest-ttl must not be negative")
	}
	return nil
}

// originUsage lives only while an upstream has active work, not per cached
// repository or JWT scope. The caller holds a fetch slot through publication.
type originUsage struct {
	fetches int
	staging int64
}

type fetchQuota struct {
	limits cacheLimits
	mu     sync.Mutex
	work   sync.WaitGroup
	closed bool
	active int
	staged int64
	origin map[string]*originUsage
}

func (q *fetchQuota) acquire(origin string) (*originUsage, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	u := q.origin[origin]
	if q.closed {
		return nil, quotaLimit(errBusy, "cache_shutdown")
	}
	if q.active >= q.limits.fetches {
		return nil, quotaLimit(errBusy, "cache-fetches")
	}
	if u != nil && u.fetches >= q.limits.upstreamFetches {
		return nil, quotaLimit(errBusy, "cache-upstream-fetches")
	}
	if u == nil {
		u = new(originUsage)
		q.origin[origin] = u
	}
	u.fetches++
	q.active++
	q.work.Add(1)
	return u, nil
}

func (q *fetchQuota) release(origin string, u *originUsage) {
	q.mu.Lock()
	u.fetches--
	q.active--
	if u.fetches == 0 {
		delete(q.origin, origin)
	}
	q.mu.Unlock()
	q.work.Done()
}

func (q *fetchQuota) reserve(u *originUsage, size int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if size > q.limits.stagingBytes-q.staged {
		return quotaLimit(errCacheFull, "cache-staging-bytes")
	}
	if size > q.limits.upstreamStagingBytes-u.staging {
		return quotaLimit(errCacheFull, "cache-upstream-staging-bytes")
	}
	q.staged += size
	u.staging += size
	return nil
}

func (q *fetchQuota) releaseStaging(u *originUsage, size int64) {
	q.mu.Lock()
	q.staged -= size
	u.staging -= size
	q.mu.Unlock()
}

func (q *fetchQuota) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.work.Wait()
}
