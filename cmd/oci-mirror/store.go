package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"syscall"

	_ "modernc.org/sqlite"
)

const cacheLayout = "mirror-v1"

var errCacheStorage = errors.New("local Mirror cache failed")

func cacheStorageError(err error) error {
	if err == nil || errors.Is(err, errCacheFull) {
		return err
	}
	return fmt.Errorf("%w: %w", errCacheStorage, err)
}

type cacheStorage struct {
	lock      *os.File
	db        *sql.DB
	staging   string
	manifests *manifestStore
	blobs     *blobStore
}

func openCacheStorage(ctx context.Context, dir string, limits cacheLimits) (_ *cacheStorage, err error) {
	root, err := filepath.Abs(filepath.Join(dir, cacheLayout))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(root, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	store := &cacheStorage{lock: lock, staging: filepath.Join(root, "staging")}
	defer func() {
		if err != nil {
			_ = store.close()
		}
	}()
	// The same root also contains staging and Blob files outside SQLite. Do
	// not reconcile or remove files while another process owns this cache.
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fmt.Errorf("lock cache directory (one Mirror process per cache): %w", err)
	}
	catalogPath := filepath.Join(root, "catalog.sqlite")
	blobsPath := filepath.Join(root, "blobs")
	if _, err := os.Stat(catalogPath); errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Stat(blobsPath); !errors.Is(statErr, os.ErrNotExist) {
			return nil, errors.New("cache catalog is missing from an initialized Blob store")
		}
	} else if err != nil {
		return nil, err
	}
	catalog := url.URL{Scheme: "file", Path: catalogPath}
	catalog.RawQuery = url.Values{"_pragma": {"journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(ON)"}}.Encode()
	store.db, err = sql.Open("sqlite", catalog.String())
	if err != nil {
		return nil, err
	}
	store.db.SetMaxOpenConns(1)
	store.manifests, err = newManifestStore(ctx, store.db, filepath.Join(root, "manifest-freshness"), limits)
	if err != nil {
		return nil, fmt.Errorf("open Manifest catalog: %w", err)
	}
	if err := os.RemoveAll(store.staging); err != nil {
		return nil, fmt.Errorf("reap interrupted Blob staging: %w", err)
	}
	if err := os.Mkdir(store.staging, 0o700); err != nil {
		return nil, err
	}
	store.blobs, err = newBlobStore(ctx, store.db, blobsPath, limits)
	if err != nil {
		return nil, fmt.Errorf("open Blob catalog: %w", err)
	}
	return store, nil
}

func (s *cacheStorage) close() error {
	var err error
	if s.db != nil {
		err = s.db.Close()
	}
	return errors.Join(err, s.lock.Close())
}
