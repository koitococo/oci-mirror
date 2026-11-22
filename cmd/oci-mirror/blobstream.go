package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
)

// blobFlight shares staging progress without tying the producer to any request.
// The writer only extends the staged prefix; readers never share a file offset.
type blobFlight struct {
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
	return &blobFlight{done: make(chan struct{})}
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
					panic(http.ErrAbortHandler)
				}
				fetchError(w, "blobs", err)
				return
			}
			if offset == 0 {
				c.servePublishedBlob(w, r, key)
				return
			}
			published, err := c.blobs.open(ctx, key)
			if err != nil {
				panic(http.ErrAbortHandler)
			}
			defer published.Close()
			if _, err := published.Seek(offset, io.SeekStart); err != nil {
				panic(http.ErrAbortHandler)
			}
			if _, err := io.CopyN(w, published, length-offset); err != nil {
				panic(http.ErrAbortHandler)
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
				panic(http.ErrAbortHandler)
			}
			fetchError(w, "blobs", cacheStorageError(err))
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
		if err != nil || n != len(chunk) {
			panic(http.ErrAbortHandler)
		}
		offset += int64(n)
		if first {
			if err := http.NewResponseController(w).Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
				panic(http.ErrAbortHandler)
			}
		}
	}
}
