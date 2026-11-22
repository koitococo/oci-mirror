package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	registry "github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

const (
	maxManifestBytes = 4 << 20
	maxBlobBytes     = 2 << 30
)

var (
	manifestTypes = []string{
		"application/vnd.oci.image.index.v1+json",
		"application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
		"application/vnd.docker.distribution.manifest.v2+json",
	}
)

type mirrorCache struct {
	*cacheStorage
	ctx       context.Context
	cancel    context.CancelFunc
	transport http.RoundTripper
	quota     fetchQuota
	blobMu    sync.Mutex
	blobTasks map[cacheKey]*blobFlight
}

type cacheScope struct {
	downstream, source string
}

type cacheKey struct {
	cacheScope
	reference string
}

func repositoryScope(repo resolvedRepository) cacheScope {
	return cacheScope{downstream: repo.downstream, source: repo.source.Name()}
}

type manifest struct {
	mediaType, digest string
	body              []byte
}

func newMirrorCache(ctx context.Context, dir string, transport http.RoundTripper, limits cacheLimits) (*mirrorCache, error) {
	if err := limits.validate(); err != nil {
		return nil, err
	}
	store, err := openCacheStorage(ctx, dir, limits)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	return &mirrorCache{
		cacheStorage: store,
		ctx:          ctx, cancel: cancel, transport: transport,
		quota:     fetchQuota{limits: limits, origin: make(map[string]*originUsage)},
		blobTasks: make(map[cacheKey]*blobFlight),
	}, nil
}

func (c *mirrorCache) close() error {
	c.cancel()
	c.quota.close()
	return c.cacheStorage.close()
}

func (c *mirrorCache) serve(w http.ResponseWriter, r *http.Request, read readRequest) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	if read.kind == "manifests" {
		c.serveManifest(w, r, read.repository, read.reference)
	} else {
		c.serveBlob(w, r, read.repository, read.reference)
	}
}

func (c *mirrorCache) serveManifest(w http.ResponseWriter, r *http.Request, repo resolvedRepository, ref string) {
	key := cacheKey{repositoryScope(repo), ref}
	accept := r.Header.Values("Accept")
	m, err := c.manifests.get(r.Context(), key)
	if err != nil {
		fetchError(w, "manifests", cacheStorageError(err))
		return
	}
	if m == nil {
		var allowed []string
		for _, mediaType := range manifestTypes {
			if accepts(accept, mediaType) {
				allowed = append(allowed, mediaType)
			}
		}
		if len(allowed) == 0 {
			registryError(w, http.StatusNotAcceptable, "MANIFEST_UNKNOWN", "no supported manifest media type accepted")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		m, err = c.fetchManifest(ctx, repo, key, allowed)
		if err != nil {
			fetchError(w, "manifests", err)
			return
		}
	}
	if !accepts(accept, m.mediaType) {
		registryError(w, http.StatusNotAcceptable, "MANIFEST_UNKNOWN", "pinned manifest media type not accepted")
		return
	}
	w.Header().Set("Content-Type", m.mediaType)
	w.Header().Set("Docker-Content-Digest", m.digest)
	w.Header().Set("Content-Length", strconv.Itoa(len(m.body)))
	w.Header().Set("Vary", "Accept")
	if r.Method == http.MethodGet {
		_, _ = w.Write(m.body)
	}
}

func (c *mirrorCache) fetchManifest(ctx context.Context, repo resolvedRepository, key cacheKey, allowed []string) (*manifest, error) {
	usage, err := c.quota.acquire(repo.logicalRegistry)
	if err != nil {
		return nil, err
	}
	defer c.quota.release(repo.logicalRegistry, usage)
	ref := key.reference
	source := publicSource{repo: repo.source, transport: c.transport}
	resp, err := source.get(ctx, "manifests", ref, strings.Join(allowed, ", "))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !slices.Contains(manifestTypes, mediaType) {
		return nil, errors.New("unsupported upstream manifest media type")
	}
	if !slices.Contains(allowed, mediaType) {
		return nil, &upstreamStatus{http.StatusNotAcceptable}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil || len(body) > maxManifestBytes {
		return nil, errors.New("upstream manifest read failed or exceeded size limit")
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(body))
	declared := resp.Header.Get("Docker-Content-Digest")
	if (digestPattern.MatchString(ref) && ref != digest) || (declared != "" && declared != digest) {
		return nil, errors.New("upstream manifest digest mismatch")
	}
	type descriptor struct {
		Digest string
		Size   *int64
	}
	var metadata struct {
		SchemaVersion int
		Config        descriptor
		Layers        []descriptor
	}
	if err := json.Unmarshal(body, &metadata); err != nil || metadata.SchemaVersion != 2 {
		return nil, errors.New("invalid upstream image manifest")
	}

	refs := make([]blobDescriptor, 0, len(metadata.Layers)+1)
	add := func(d descriptor) {
		if d.Size != nil && *d.Size >= 0 && digestPattern.MatchString(d.Digest) {
			refs = append(refs, blobDescriptor{digest: d.Digest, size: *d.Size})
		}
	}
	add(metadata.Config)
	for _, layer := range metadata.Layers {
		add(layer)
	}
	m := &manifest{mediaType: mediaType, digest: digest, body: body}
	published, err := c.manifests.publish(ctx, key, m, refs)
	if err != nil {
		return nil, cacheStorageError(err)
	}
	if published == m {
		log.Printf("mirror manifest published digest=%s", digest)
	}
	return published, nil
}

func accepts(values []string, mediaType string) bool {
	if len(values) == 0 {
		return true
	}
	family, _, _ := strings.Cut(mediaType, "/")
	best, quality := -1, 0.0
	for _, value := range values {
		for entry := range strings.SplitSeq(value, ",") {
			candidate, params, err := mime.ParseMediaType(strings.TrimSpace(entry))
			if err != nil {
				continue
			}
			rank := -1
			switch candidate {
			case "*/*":
				rank = 0
			case family + "/*":
				rank = 1
			case mediaType:
				rank = 2
			}
			q := 1.0
			if raw, ok := params["q"]; ok {
				q, err = strconv.ParseFloat(raw, 64)
				if err != nil || !(q >= 0 && q <= 1) {
					continue
				}
			}
			if rank >= 0 && (rank > best || (rank == best && q > quality)) {
				best, quality = rank, q
			}
		}
	}
	return quality > 0
}

func (c *mirrorCache) serveBlob(w http.ResponseWriter, r *http.Request, repo resolvedRepository, digest string) {
	key := cacheKey{repositoryScope(repo), digest}
	defer c.blobs.pin(key)()
	f, err := c.blobs.open(r.Context(), key)
	if err == nil {
		serveVerifiedBlob(w, r, digest, f)
		return
	}
	if !errors.Is(err, os.ErrNotExist) {
		registryError(w, http.StatusInternalServerError, "UNKNOWN", "cannot open verified blob")
		return
	}
	flight := c.joinBlobFlight(repo, key)
	if earlyBlobGET(r) {
		c.serveColdBlob(w, r, key, flight)
		return
	}
	if err := flight.wait(r.Context()); err != nil {
		if r.Context().Err() == nil {
			fetchError(w, "blobs", err)
		}
		return
	}
	c.servePublishedBlob(w, r, key)
}

func (c *mirrorCache) joinBlobFlight(repo resolvedRepository, key cacheKey) *blobFlight {
	c.blobMu.Lock()
	defer c.blobMu.Unlock()
	if flight := c.blobTasks[key]; flight != nil {
		return flight
	}
	flight := newBlobFlight()
	c.blobTasks[key] = flight
	go func() {
		unpin := c.blobs.pin(key)
		defer unpin()
		err := c.downloadBlob(repo, key, flight)
		// Staging is closed and quotas are released. Retire the flight before
		// notifying waiters so an immediate retry cannot join a failed flight.
		c.blobMu.Lock()
		delete(c.blobTasks, key)
		flight.finish(err)
		c.blobMu.Unlock()
	}()
	return flight
}

func (c *mirrorCache) servePublishedBlob(w http.ResponseWriter, r *http.Request, key cacheKey) {
	f, err := c.blobs.open(r.Context(), key)
	if err != nil {
		registryError(w, http.StatusInternalServerError, "UNKNOWN", "cannot open verified blob")
		return
	}
	serveVerifiedBlob(w, r, key.reference, f)
}

func setBlobHeaders(w http.ResponseWriter, digest string) {
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("ETag", `"`+digest+`"`)
	w.Header().Set("Content-Type", "application/octet-stream")
}

func serveVerifiedBlob(w http.ResponseWriter, r *http.Request, digest string, f *os.File) {
	defer f.Close()
	setBlobHeaders(w, digest)
	http.ServeContent(w, r, digest, time.Time{}, f)
}

func (c *mirrorCache) downloadBlob(repo resolvedRepository, key cacheKey, flight *blobFlight) (result error) {
	ctx, cancel := context.WithTimeout(c.ctx, 10*time.Minute)
	defer cancel()
	f, err := c.blobs.open(ctx, key)
	if err == nil {
		return cacheStorageError(f.Close())
	}
	if !errors.Is(err, os.ErrNotExist) {
		return cacheStorageError(err)
	}
	usage, err := c.quota.acquire(repo.logicalRegistry)
	if err != nil {
		return err
	}
	defer c.quota.release(repo.logicalRegistry, usage)

	expected, known, err := c.manifests.blobSize(ctx, key)
	if err != nil {
		return cacheStorageError(err)
	}
	source := publicSource{repo: repo.source, transport: c.transport}
	resp, err := source.get(ctx, "blobs", key.reference, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if known && resp.ContentLength >= 0 && expected != resp.ContentLength {
		return errors.New("upstream blob length does not match the Manifest")
	}
	if !known && resp.ContentLength >= 0 {
		expected, known = resp.ContentLength, true
	}
	if known && expected > maxBlobBytes {
		return errors.New("upstream blob length above single-blob limit")
	}
	limits := c.quota.limits
	limit := min(int64(maxBlobBytes), limits.blobBytes, limits.stagingBytes, limits.upstreamStagingBytes)
	if known {
		if expected > limit {
			return errCacheFull
		}
		limit = expected
	}
	if err := c.quota.reserve(usage, limit); err != nil {
		return err
	}
	defer c.quota.releaseStaging(usage, limit)

	file, err := os.CreateTemp(c.staging, "blob-")
	if err != nil {
		return cacheStorageError(err)
	}
	defer os.Remove(file.Name())
	// Close the shared staging reader before removing the stage and releasing
	// its reservation. No downstream network write holds that descriptor.
	defer func() {
		if result != nil {
			log.Printf("mirror staged blob failed digest=%s error_type=%T", key.reference, result)
		}
		flight.closeStaging()
	}()
	var progress io.Writer
	if known && expected > 1 {
		reader, err := os.Open(file.Name())
		if err != nil {
			_ = file.Close()
			return cacheStorageError(err)
		}
		flight.startStreaming(reader, expected)
		progress = blobProgress{flight}
	}
	n, err := copyVerifiedBlob(file, resp.Body, key.reference, limit, known, progress)
	if err != nil {
		_ = file.Close()
		return err
	}
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return cacheStorageError(err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.blobs.publish(ctx, key, file.Name(), n); err != nil {
		return cacheStorageError(err)
	}
	log.Printf("mirror blob published digest=%s bytes=%d", key.reference, n)
	return nil
}

type blobProgress struct{ flight *blobFlight }

func (p blobProgress) Write(data []byte) (int, error) {
	p.flight.advance(len(data))
	return len(data), nil
}

func copyVerifiedBlob(file *os.File, body io.Reader, digest string, limit int64, exact bool, progress io.Writer) (int64, error) {
	h := sha256.New()
	writers := [3]io.Writer{file, h, progress}
	count := 2
	if progress != nil {
		count++
	}
	n, err := io.Copy(io.MultiWriter(writers[:count]...), io.LimitReader(body, limit))
	if err != nil {
		var pathError *os.PathError
		if errors.As(err, &pathError) {
			return 0, cacheStorageError(err)
		}
		return 0, err
	}
	if exact && n != limit {
		return 0, errors.New("upstream blob shorter than expected")
	}
	// Check EOF without writing an extra byte past the staging reservation.
	if n == limit {
		var tail [1]byte
		if _, err := io.ReadFull(body, tail[:]); err != io.EOF {
			if err != nil {
				return 0, err
			}
			if !exact && limit < maxBlobBytes {
				return 0, errCacheFull
			}
			return 0, errors.New("upstream blob longer than allowed")
		}
	}
	if "sha256:"+hex.EncodeToString(h.Sum(nil)) != digest {
		return 0, errors.New("upstream blob digest mismatch")
	}
	return n, nil
}

func fetchError(w http.ResponseWriter, kind string, err error) {
	var upstream *upstreamStatus
	var transport *registry.Error
	status := 0
	if errors.As(err, &upstream) {
		status = upstream.status
	} else if errors.As(err, &transport) {
		status = transport.StatusCode
	}
	switch {
	case errors.Is(err, errBusy):
		registryError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "global or upstream cold fetch limit reached")
	case errors.Is(err, errCacheFull):
		registryError(w, http.StatusInsufficientStorage, "UNAVAILABLE", "Mirror cache quota exhausted")
	case errors.Is(err, errCacheStorage):
		log.Printf("mirror cache failed kind=%s: %v", kind, err)
		registryError(w, http.StatusInternalServerError, "UNKNOWN", "local Mirror cache failed")
	case errors.Is(err, errPublicURL), errors.Is(err, errPublicIP), errors.Is(err, errPrivateDNS):
		log.Printf("mirror egress policy rejected kind=%s", kind)
		registryError(w, http.StatusBadGateway, "UNAVAILABLE", "upstream rejected by egress policy")
	case status == http.StatusNotFound:
		code := "MANIFEST_UNKNOWN"
		if kind == "blobs" {
			code = "BLOB_UNKNOWN"
		}
		registryError(w, status, code, "content not found upstream")
	case status == http.StatusNotAcceptable && kind == "manifests":
		registryError(w, status, "MANIFEST_UNKNOWN", "upstream manifest media type not accepted")
	default:
		// Upstream errors may contain signed URLs or token response bodies.
		log.Printf("mirror fetch failed kind=%s upstream_status=%d error_type=%T", kind, status, err)
		registryError(w, http.StatusBadGateway, "UNAVAILABLE", "upstream fetch or verification failed")
	}
}

func registryError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": message}}})
}
