package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

type blobDescriptor struct {
	digest string
	size   int64
}

type manifestStore struct {
	db         *sql.DB
	maxBytes   int64
	maxEntries int
	ttl        time.Duration
	freshDir   string
	// Keep catalog lookups and freshness checks consistent with publication,
	// which commits to SQLite before updating the filesystem markers.
	freshMu sync.RWMutex
}

func newManifestStore(ctx context.Context, db *sql.DB, freshDir string, limits cacheLimits) (*manifestStore, error) {
	if limits.manifestTTL > 0 {
		if err := os.MkdirAll(freshDir, 0o700); err != nil {
			return nil, fmt.Errorf("create Manifest freshness directory: %w", err)
		}
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin manifest schema setup: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS manifests (
    downstream TEXT NOT NULL,
    source TEXT NOT NULL,
    digest TEXT NOT NULL,
    media_type TEXT NOT NULL,
    body BLOB NOT NULL,
    PRIMARY KEY (downstream, source, digest)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS manifest_tags (
    downstream TEXT NOT NULL,
    source TEXT NOT NULL,
    tag TEXT NOT NULL,
    digest TEXT NOT NULL,
    PRIMARY KEY (downstream, source, tag),
    FOREIGN KEY (downstream, source, digest) REFERENCES manifests (downstream, source, digest)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS manifest_blob_descriptors (
    id INTEGER PRIMARY KEY,
    downstream TEXT NOT NULL,
    source TEXT NOT NULL,
    manifest_digest TEXT NOT NULL,
    blob_digest TEXT NOT NULL,
    size INTEGER NOT NULL CHECK (size >= 0),
    UNIQUE (downstream, source, manifest_digest, blob_digest),
    FOREIGN KEY (downstream, source, manifest_digest) REFERENCES manifests (downstream, source, digest)
);
CREATE INDEX IF NOT EXISTS manifest_blob_lookup
    ON manifest_blob_descriptors (downstream, source, blob_digest, id);
`); err != nil {
		return nil, fmt.Errorf("create manifest tables: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit manifest schema: %w", err)
	}
	return &manifestStore{
		db: db, maxBytes: limits.manifestBytes, maxEntries: limits.manifestEntries,
		ttl: limits.manifestTTL, freshDir: freshDir,
	}, nil
}

type manifestQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func lookupManifest(ctx context.Context, db manifestQuerier, key cacheKey) (*manifest, error) {
	query := `SELECT digest, media_type, body FROM manifests
WHERE downstream = ? AND source = ? AND digest = ?`
	if !digestPattern.MatchString(key.reference) {
		query = `SELECT m.digest, m.media_type, m.body FROM manifest_tags AS t
LEFT JOIN manifests AS m ON (m.downstream, m.source, m.digest) = (t.downstream, t.source, t.digest)
WHERE t.downstream = ? AND t.source = ? AND t.tag = ?`
	}
	var m manifest
	if err := db.QueryRowContext(ctx, query, key.downstream, key.source, key.reference).
		Scan(&m.digest, &m.mediaType, &m.body); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("read cached manifest: %w", err)
	}
	return &m, nil
}

func (s *manifestStore) get(ctx context.Context, key cacheKey) (*manifest, error) {
	if s.ttl > 0 {
		s.freshMu.RLock()
		defer s.freshMu.RUnlock()
		if fresh, err := s.fresh(key); !fresh || err != nil {
			return nil, err
		}
	}
	return lookupManifest(ctx, s.db, key)
}

func (s *manifestStore) freshnessPath(key cacheKey) string {
	sum := sha256.Sum256([]byte(key.downstream + "\x00" + key.source + "\x00" + key.reference))
	return filepath.Join(s.freshDir, fmt.Sprintf("%x", sum))
}

func (s *manifestStore) fresh(key cacheKey) (bool, error) {
	var stat unix.Stat_t
	if err := unix.Stat(s.freshnessPath(key), &stat); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stat Manifest freshness: %w", err)
	}
	return time.Since(time.Unix(stat.Ctim.Unix())) < s.ttl, nil
}

func (s *manifestStore) markFresh(key cacheKey) error {
	// Creation or truncation advances ctime, including a refresh of the
	// same digest. Cache hits only stat the marker and never advance it.
	if err := os.WriteFile(s.freshnessPath(key), nil, 0o600); err != nil {
		return fmt.Errorf("write Manifest freshness: %w", err)
	}
	return nil
}

func (s *manifestStore) publish(ctx context.Context, key cacheKey, m *manifest, refs []blobDescriptor) (*manifest, error) {
	if s.ttl > 0 {
		s.freshMu.Lock()
		defer s.freshMu.Unlock()
	}
	// The store's single SQL connection serializes the lookup, quota check, and insert.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin manifest publication: %w", err)
	}
	defer tx.Rollback()
	fresh := true
	if s.ttl > 0 {
		fresh, err = s.fresh(key)
		if err != nil {
			return nil, err
		}
	}
	if fresh {
		if pinned, err := lookupManifest(ctx, tx, key); pinned != nil || err != nil {
			return pinned, err
		}
	}

	digestKey := cacheKey{cacheScope: key.cacheScope, reference: m.digest}
	var storedType string
	err = tx.QueryRowContext(ctx, `SELECT media_type FROM manifests
WHERE downstream = ? AND source = ? AND digest = ?`, key.downstream, key.source, m.digest).Scan(&storedType)
	newDigest := errors.Is(err, sql.ErrNoRows)
	if err != nil && !newDigest {
		return nil, fmt.Errorf("read cached Manifest media type: %w", err)
	}

	isTag := !digestPattern.MatchString(key.reference)
	var extraBytes int64
	var extraEntries int
	if newDigest {
		extraBytes = int64(len(m.body))
		extraEntries++
	}
	if isTag {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM manifest_tags
WHERE downstream = ? AND source = ? AND tag = ?)`, key.downstream, key.source, key.reference).Scan(&exists); err != nil {
			return nil, fmt.Errorf("read cached Manifest Tag binding: %w", err)
		}
		if !exists {
			extraEntries++
		}
	}
	if extraEntries > 0 {
		var usedBytes int64
		var usedEntries int
		if err := tx.QueryRowContext(ctx, `
SELECT COALESCE(SUM(length(body)), 0), COUNT(*) + (SELECT COUNT(*) FROM manifest_tags)
FROM manifests`).Scan(&usedBytes, &usedEntries); err != nil {
			return nil, fmt.Errorf("read manifest cache usage: %w", err)
		}
		if extraBytes > 0 && extraBytes > s.maxBytes-usedBytes {
			return nil, quotaLimit(errCacheFull, "cache-manifest-bytes")
		}
		if extraEntries > s.maxEntries-usedEntries {
			return nil, quotaLimit(errCacheFull, "cache-manifest-entries")
		}
	}

	if newDigest {
		if _, err := tx.ExecContext(ctx, `INSERT INTO manifests (downstream, source, digest, media_type, body)
VALUES (?, ?, ?, ?, ?)`, key.downstream, key.source, m.digest, m.mediaType, m.body); err != nil {
			return nil, fmt.Errorf("insert cached manifest: %w", err)
		}
		for _, ref := range refs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO manifest_blob_descriptors
(downstream, source, manifest_digest, blob_digest, size) VALUES (?, ?, ?, ?, ?)
ON CONFLICT (downstream, source, manifest_digest, blob_digest) DO NOTHING`,
				key.downstream, key.source, m.digest, ref.digest, ref.size); err != nil {
				return nil, fmt.Errorf("insert cached blob descriptor: %w", err)
			}
		}
	} else {
		m.mediaType = storedType
	}
	if isTag {
		if _, err := tx.ExecContext(ctx, `INSERT INTO manifest_tags (downstream, source, tag, digest)
VALUES (?, ?, ?, ?)
ON CONFLICT (downstream, source, tag) DO UPDATE SET digest = excluded.digest`, key.downstream, key.source, key.reference, m.digest); err != nil {
			return nil, fmt.Errorf("bind cached manifest tag: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit manifest publication: %w", err)
	}
	if s.ttl > 0 {
		if err := s.markFresh(digestKey); err != nil {
			return nil, err
		}
		if isTag {
			if err := s.markFresh(key); err != nil {
				return nil, err
			}
		}
	}
	return m, nil
}

func (s *manifestStore) blobSize(ctx context.Context, key cacheKey) (int64, bool, error) {
	var size int64
	// Keep the first published size when manifests disagree within the same scope.
	if err := s.db.QueryRowContext(ctx, `SELECT size FROM manifest_blob_descriptors
WHERE downstream = ? AND source = ? AND blob_digest = ? ORDER BY id LIMIT 1`,
		key.downstream, key.source, key.reference).Scan(&size); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("read cached blob descriptor: %w", err)
	}
	return size, true, nil
}
