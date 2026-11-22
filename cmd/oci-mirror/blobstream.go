package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"sync"
)

// blobFlight shares staging progress without tying the producer to any request.
// The writer only extends the staged prefix; readers never share a file offset.
type blobFlight struct {
	id      string
	mu      sync.Mutex
	changed chan struct{}
	done    chan struct{}

	reader   *os.File
	length   int64
	written  int64
	finished bool
	err      error
}

func newBlobFlight() *blobFlight {
	return &blobFlight{id: newLogID(), done: make(chan struct{})}
}

func (f *blobFlight) notifyLocked() {
	if f.changed != nil {
		close(f.changed)
		f.changed = nil
	}
}

func (f *blobFlight) startStreaming(reader *os.File, length int64) {
	f.mu.Lock()
	f.reader, f.length = reader, length
	f.notifyLocked()
	f.mu.Unlock()
}

func (f *blobFlight) advance(n int) {
	f.mu.Lock()
	f.written += int64(n)
	f.notifyLocked()
	f.mu.Unlock()
}

func (f *blobFlight) closeStaging() {
	f.mu.Lock()
	reader := f.reader
	f.reader = nil
	f.mu.Unlock()
	if reader != nil {
		_ = reader.Close()
	}
}

func (f *blobFlight) finish(err error) {
	f.mu.Lock()
	f.finished, f.err = true, err
	close(f.done)
	f.notifyLocked()
	f.mu.Unlock()
}

func (f *blobFlight) wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-f.done:
		return f.err
	}
}

func earlyBlobGET(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	for name := range r.Header {
		switch http.CanonicalHeaderKey(name) {
		case "Range", "If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "If-Range":
			return false
		}
	}
	return true
}

func blobFailure(err error) *blobTransferFailure {
	var failure *blobTransferFailure
	if errors.As(err, &failure) {
		return failure
	}
	return &blobTransferFailure{stage: "download", reason: logErrorReason(err), offset: -1, err: err}
}

func blobRequestFailed(ctx context.Context, key cacheKey, f *blobFlight, stage string, err error) {
	level := slog.LevelWarn
	if ctx.Err() != nil || fetchUpstreamStatus(err) == http.StatusNotFound || errors.Is(err, errBusy) || errors.Is(err, errCacheFull) {
		level = slog.LevelDebug
	}
	if stage != "" {
		slog.Log(ctx, level, "blob_request_failed", "request_id", requestID(ctx), "flight_id", f.id, "digest", key.reference,
			"stage", stage, "reason", logErrorReason(err))
		return
	}
	slog.Log(ctx, level, "blob_request_failed", "request_id", requestID(ctx), "flight_id", f.id, "digest", key.reference)
}

func abortBlobResponse(ctx context.Context, key cacheKey, f *blobFlight, stage, reason string, sent, expected int64) {
	level := slog.LevelWarn
	if ctx.Err() != nil {
		level = slog.LevelDebug
	}
	slog.Log(ctx, level, "blob_response_aborted", "request_id", requestID(ctx), "flight_id", f.id, "digest", key.reference,
		"stage", stage, "reason", reason, "bytes_sent", sent, "bytes_expected", expected)
	panic(http.ErrAbortHandler)
}

func (c *mirrorCache) serveColdBlob(w http.ResponseWriter, r *http.Request, key cacheKey, f *blobFlight) {
	ctx := r.Context()
	var offset int64
	var buf []byte
	for ctx.Err() == nil {
		f.mu.Lock()
		finished, err := f.finished, f.err
		reader, length := f.reader, f.length
		// Keep at least one byte back until the verified blob is published.
		end := min(f.written, length-1)
		if !finished && (reader == nil || end <= offset) {
			if f.changed == nil {
				f.changed = make(chan struct{})
			}
			changed := f.changed
			f.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-changed:
				continue
			}
		}
		f.mu.Unlock()

		if finished {
			if err != nil {
				if offset != 0 {
					failure := blobFailure(err)
					abortBlobResponse(ctx, key, f, failure.stage, "producer_failed", offset, length)
					return
				}
				blobRequestFailed(ctx, key, f, "", err)
				writeFetchError(w, "blobs", err, false)
				return
			}
			if offset == 0 {
				c.servePublishedBlob(w, r, key, f)
				return
			}
			published, err := c.blobs.open(ctx, key)
			if err != nil {
				abortBlobResponse(ctx, key, f, "published_open", logErrorReason(err), offset, length)
				return
			}
			defer published.Close()
			if _, err := published.Seek(offset, io.SeekStart); err != nil {
				abortBlobResponse(ctx, key, f, "published_seek", logErrorReason(err), offset, length)
				return
			}
			if n, err := io.CopyN(w, published, length-offset); err != nil {
				// CopyN can fail on either side. Keep its fast paths and do not
				// attribute an ambiguous error to storage or the downstream.
				abortBlobResponse(ctx, key, f, "final_copy", logErrorReason(err), offset+n, length)
			}
			return
		}

		if buf == nil {
			buf = make([]byte, min(32*1024, length-1))
		}
		chunk := buf[:min(int64(len(buf)), end-offset)]
		_, err = reader.ReadAt(chunk, offset)
		// The worker can detach staging while ReadAt is in progress. If it
		// did, wait for the result instead of treating the close as corruption.
		f.mu.Lock()
		detached := f.reader != reader
		f.mu.Unlock()
		if detached {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if offset != 0 {
				abortBlobResponse(ctx, key, f, "staging_read", logErrorReason(err), offset, length)
				return
			}
			blobRequestFailed(ctx, key, f, "staging_read", err)
			writeFetchError(w, "blobs", cacheStorageError(err), false)
			return
		}
		first := offset == 0
		if first {
			setBlobHeaders(w, key.reference)
			w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
			w.Header().Set("X-Accel-Buffering", "no")
			w.Header().Set("Accept-Ranges", "bytes")
		}
		n, err := w.Write(chunk)
		offset += int64(n)
		if err != nil || n != len(chunk) {
			reason := "short_write"
			if err != nil {
				reason = logErrorReason(err)
			}
			abortBlobResponse(ctx, key, f, "downstream_write", reason, offset, length)
			return
		}
		if first {
			if err := http.NewResponseController(w).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
				abortBlobResponse(ctx, key, f, "downstream_flush", logErrorReason(err), offset, length)
			}
		}
	}
}
