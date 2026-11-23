package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type blobStore struct {
	db                      *sql.DB
	root                    string
	maxBytes, softBytes     int64
	maxEntries, softEntries int
	ttl                     time.Duration

	mu          sync.Mutex
	pins        map[cacheKey]int
	usedBytes   int64
	usedEntries int
	// An interrupted publication or eviction can leave unaccounted files.
	// Do not admit more blobs until startup reconciles the catalog and disk.
	publicationErr error
}

func newBlobStore(ctx context.Context, db *sql.DB, root string, limits cacheLimits) (*blobStore, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create blob root: %w", err)
	}
	if err := syncBlobDir(filepath.Dir(root)); err != nil {
		return nil, fmt.Errorf("sync blob root parent: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS blobs (
		downstream TEXT NOT NULL,
		source TEXT NOT NULL,
		digest TEXT NOT NULL,
		size INTEGER NOT NULL CHECK (size >= 0),
		accessed INTEGER NOT NULL,
		PRIMARY KEY (downstream, source, digest)
	);
	CREATE INDEX IF NOT EXISTS blobs_accessed ON blobs (accessed, downstream, source, digest);`); err != nil {
		return nil, fmt.Errorf("initialize blob catalog: %w", err)
	}
	softBytes, softEntries := limits.blobSoftBytes, limits.blobSoftEntries
	if softBytes == 0 {
		softBytes = limits.blobBytes
	}
	if softEntries == 0 {
		softEntries = limits.blobEntries
	}
	s := &blobStore{
		db: db, root: root, pins: make(map[cacheKey]int), ttl: limits.blobTTL,
		maxBytes: limits.blobBytes, maxEntries: limits.blobEntries,
		softBytes: softBytes, softEntries: softEntries,
	}
	if err := s.pruneOrphans(ctx); err != nil {
		return nil, fmt.Errorf("prune blob store: %w", err)
	}
	if err := s.trim(ctx, 0, 0); err != nil {
		return nil, fmt.Errorf("reconcile blob quota: %w", err)
	}
	return s, nil
}

func (s *blobStore) path(key cacheKey) string {
	// Repository names cannot contain NUL; the digest is validated before use.
	scope := sha256.Sum256([]byte(key.downstream + "\x00" + key.source))
	return filepath.Join(s.root, hex.EncodeToString(scope[:]), strings.TrimPrefix(key.reference, "sha256:"))
}

func (s *blobStore) pin(key cacheKey) func() {
	s.mu.Lock()
	s.pins[key]++
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.pins[key] == 1 {
			delete(s.pins, key)
		} else {
			s.pins[key]--
		}
	}
}

func (s *blobStore) record(ctx context.Context, key cacheKey) (size, accessed int64, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT size, accessed FROM blobs WHERE downstream=? AND source=? AND digest=?`,
		key.downstream, key.source, key.reference).Scan(&size, &accessed)
	return size, accessed, err
}

func (s *blobStore) open(ctx context.Context, key cacheKey) (*os.File, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	size, accessed, err := s.record(ctx, key)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, diagnoseFailure("catalog_lookup", "", fmt.Errorf("read blob catalog: %w", err))
	}
	path := s.path(key)
	f, err := os.Open(path)
	if err != nil {
		// Do not wrap a file-not-found error: this is a corrupt hit, not a miss.
		return nil, diagnoseFailure("published_file_open", logErrorReason(err), fmt.Errorf("open published blob %q: %v", path, err))
	}
	if err := checkBlobFile(path, size, f.Stat); err != nil {
		f.Close()
		return nil, err
	}
	// Recency is approximate; avoid a durable SQLite write on every hot read.
	if now := time.Now().UnixNano(); now-accessed >= int64(time.Second) {
		if _, err := s.db.ExecContext(ctx, `UPDATE blobs SET accessed=? WHERE downstream=? AND source=? AND digest=?`,
			now, key.downstream, key.source, key.reference); err != nil {
			_ = f.Close()
			return nil, diagnoseFailure("catalog_recency_update", "", fmt.Errorf("update blob recency: %w", err))
		}
	}
	return f, nil
}

func checkBlobFile(path string, size int64, stat func() (os.FileInfo, error)) error {
	info, err := stat()
	if err != nil {
		// Classify before preserving the non-wrapping corrupt-hit error.
		return diagnoseFailure("published_file_stat", logErrorReason(err), fmt.Errorf("stat published blob %q: %v", path, err))
	}
	mode, actualSize := info.Mode(), info.Size()
	regular := mode.IsRegular()
	if !regular || actualSize != size {
		reason := "size_mismatch"
		if !regular {
			reason = "invalid_file_type"
		}
		return diagnoseFailure("published_file_validate", reason,
			fmt.Errorf("invalid published blob %q: mode %s, size %d, expected regular file of size %d", path, mode, actualSize, size),
			"file_bytes_actual", actualSize, "file_bytes_expected", size)
	}
	return nil
}

func (s *blobStore) publish(ctx context.Context, key cacheKey, stage string, size int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	path := s.path(key)
	publishedSize, _, err := s.record(ctx, key)
	switch {
	case err == nil:
		return checkBlobFile(path, publishedSize, func() (os.FileInfo, error) { return os.Stat(path) })
	case !errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("read blob catalog before publication: %w", err)
	}
	if s.publicationErr != nil {
		return fmt.Errorf("blob publication disabled until orphan cleanup at restart: %w", s.publicationErr)
	}
	if size < 0 {
		return fmt.Errorf("invalid staged blob size %d", size)
	}
	if size > s.maxBytes {
		return quotaLimit(errCacheFull, "cache-blob-bytes")
	}
	if s.maxEntries < 1 {
		return quotaLimit(errCacheFull, "cache-blob-entries")
	}
	if err := s.trim(ctx, size, 1); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Dir(path), 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("create blob scope directory: %w", err)
	}
	if err := os.Rename(stage, path); err != nil {
		return fmt.Errorf("publish staged blob: %w", err)
	}
	// The stage data was synced by the caller. Persist both directory levels
	// before committing the row that makes this file visible to readers.
	if err := syncBlobDir(filepath.Dir(path)); err != nil {
		s.publicationErr = fmt.Errorf("sync published blob directory: %w", err)
		return s.publicationErr
	}
	if err := syncBlobDir(s.root); err != nil {
		s.publicationErr = fmt.Errorf("sync blob root: %w", err)
		return s.publicationErr
	}
	// Once the rename succeeds, finish the local commit even if the caller cancels.
	if _, err := s.db.ExecContext(context.WithoutCancel(ctx), `INSERT INTO blobs (downstream, source, digest, size, accessed) VALUES (?, ?, ?, ?, ?)`,
		key.downstream, key.source, key.reference, size, time.Now().UnixNano()); err != nil {
		s.publicationErr = fmt.Errorf("record published blob: %w", err)
		return s.publicationErr
	}
	s.usedBytes += size
	s.usedEntries++
	return nil
}

// trim targets the soft limits. A publication reserves room for its payload
// and record; startup makes a best-effort pass even if protected usage exceeds
// the hard limits after a configuration change.
func (s *blobStore) trim(ctx context.Context, reserveBytes int64, reserveEntries int) error {
	targetBytes := max(s.softBytes-reserveBytes, 0)
	targetEntries := s.softEntries - reserveEntries
	remainingBytes, remainingEntries := s.usedBytes, s.usedEntries
	if remainingBytes <= targetBytes && remainingEntries <= targetEntries {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT downstream, source, digest, size FROM blobs ORDER BY accessed, downstream, source, digest`)
	if err != nil {
		return fmt.Errorf("select blob eviction candidates: %w", err)
	}
	defer rows.Close()
	type victim struct {
		key  cacheKey
		size int64
	}
	var victims []victim
	cutoff := time.Now().Add(-s.ttl)
	for (remainingBytes > targetBytes || remainingEntries > targetEntries) && rows.Next() {
		var v victim
		if err := rows.Scan(&v.key.downstream, &v.key.source, &v.key.reference, &v.size); err != nil {
			return fmt.Errorf("read blob eviction candidate: %w", err)
		}
		if s.pins[v.key] != 0 {
			continue
		}
		if s.ttl > 0 {
			var stat unix.Stat_t
			path := s.path(v.key)
			if err := unix.Stat(path, &stat); err != nil {
				return fmt.Errorf("stat Blob TTL for %q: %w", path, err)
			}
			if time.Unix(stat.Ctim.Unix()).After(cutoff) {
				continue
			}
		}
		victims = append(victims, v)
		remainingBytes -= v.size
		remainingEntries--
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("scan blob eviction candidates: %w", err)
	}
	// Release the single database connection before issuing any writes.
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close blob eviction candidates: %w", err)
	}
	if reserveEntries != 0 {
		if remainingBytes > s.maxBytes-reserveBytes {
			return quotaLimit(errCacheFull, "cache-blob-bytes")
		}
		if remainingEntries > s.maxEntries-reserveEntries {
			return quotaLimit(errCacheFull, "cache-blob-entries")
		}
	}
	for _, v := range victims {
		if err := s.evict(ctx, v.key, v.size); err != nil {
			return err
		}
	}
	return nil
}

func (s *blobStore) evict(ctx context.Context, key cacheKey, size int64) error {
	path := s.path(key)
	if err := checkBlobFile(path, size, func() (os.FileInfo, error) { return os.Stat(path) }); err != nil {
		return err
	}
	// Commit the deletion first. A crash can now leave an orphan, but never
	// a catalog row for a legitimately evicted and unlinked file.
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(context.WithoutCancel(ctx), `DELETE FROM blobs WHERE downstream=? AND source=? AND digest=?`,
		key.downstream, key.source, key.reference); err != nil {
		s.publicationErr = fmt.Errorf("delete blob catalog row: %w", err)
		return s.publicationErr
	}
	s.usedBytes -= size
	s.usedEntries--
	if err := os.Remove(path); err != nil {
		s.publicationErr = fmt.Errorf("unlink evicted blob %q: %w", path, err)
		return s.publicationErr
	}
	if err := os.Remove(filepath.Dir(path)); err != nil && !errors.Is(err, syscall.ENOTEMPTY) {
		return fmt.Errorf("remove empty blob scope: %w", err)
	}
	return nil
}

func (s *blobStore) pruneOrphans(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT downstream, source, digest, size FROM blobs`)
	if err != nil {
		return err
	}
	defer rows.Close()
	live := make(map[string]struct{})
	for rows.Next() {
		var key cacheKey
		var size int64
		if err := rows.Scan(&key.downstream, &key.source, &key.reference, &size); err != nil {
			return err
		}
		live[s.path(key)] = struct{}{}
		s.usedBytes += size
		s.usedEntries++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	scopes, err := os.ReadDir(s.root)
	if err != nil {
		return err
	}
	for _, scope := range scopes {
		if err := ctx.Err(); err != nil {
			return err
		}
		dir := filepath.Join(s.root, scope.Name())
		if !scope.IsDir() {
			if err := os.Remove(dir); err != nil {
				return err
			}
			continue
		}
		files, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		kept := false
		for _, file := range files {
			path := filepath.Join(dir, file.Name())
			if _, ok := live[path]; ok {
				kept = true
				continue
			}
			if err := os.RemoveAll(path); err != nil {
				return err
			}
		}
		if !kept {
			if err := os.Remove(dir); err != nil {
				return err
			}
		}
	}
	return nil
}

func syncBlobDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
