package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
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
	upstream  *upstreamPool
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

func newMirrorCache(ctx context.Context, dir string, transport http.RoundTripper, limits cacheLimits, upstreamLimit upstreamLimits) (*mirrorCache, error) {
	if err := limits.validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	upstream, err := newUpstreamPool(ctx, transport, upstreamLimit)
	if err != nil {
		cancel()
		return nil, err
	}
	store, err := openCacheStorage(ctx, dir, limits)
	if err != nil {
		cancel()
		return nil, err
	}
	return &mirrorCache{
		cacheStorage: store,
		ctx:          ctx, cancel: cancel, upstream: upstream,
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
		setRequestCache(r.Context(), "error")
		manifestRequestFailed(w, r, repo, ref, diagnoseAt("manifest_lookup", cacheStorageError(err)))
		return
	}
	if m == nil {
		setRequestCache(r.Context(), "miss")
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
			manifestRequestFailed(w, r, repo, ref, err)
			return
		}
	} else {
		setRequestCache(r.Context(), "hit")
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
		return nil, diagnoseAt("quota_acquire", err)
	}
	defer c.quota.release(repo.logicalRegistry, usage)
	client, release, err := c.upstream.acquire(ctx, repo.source)
	if err != nil {
		return nil, diagnoseAt("upstream_acquire", err)
	}
	defer release()
	ref := key.reference
	resp, err := getUpstream(ctx, client, repo.source, "manifests", ref, strings.Join(allowed, ", "))
	if err != nil {
		return nil, diagnoseAt("upstream_get", err)
	}
	defer resp.Body.Close()
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, manifestValidationFailure(resp, "invalid_content_type", err, "content_type_values", len(resp.Header.Values("Content-Type")))
	}
	if !slices.Contains(manifestTypes, mediaType) {
		return nil, manifestValidationFailure(resp, "unsupported_media_type", errors.New("unsupported upstream manifest media type"), "media_type", boundedMediaType(mediaType))
	}
	if !slices.Contains(allowed, mediaType) {
		return nil, manifestValidationFailure(resp, "upstream_media_type_not_accepted", &upstreamStatus{http.StatusNotAcceptable}, "media_type", mediaType)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		fields := []any{"bytes_read", len(body)}
		if resp.ContentLength >= 0 {
			fields = append(fields, "bytes_expected", resp.ContentLength)
		}
		return nil, &diagnosticError{stage: "manifest_body_read", upstreamStatus: resp.StatusCode, err: err, fields: appendUpstreamHost(fields, resp.Request)}
	}
	if len(body) > maxManifestBytes {
		return nil, manifestValidationFailure(resp, "manifest_too_large", errors.New("upstream manifest exceeded size limit"),
			"bytes_read", len(body), "bytes_limit", maxManifestBytes)
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(body))
	declared := resp.Header.Get("Docker-Content-Digest")
	if (digestPattern.MatchString(ref) && ref != digest) || (declared != "" && declared != digest) {
		fields := []any{"computed_digest", digest}
		if declared != "" {
			if digestPattern.MatchString(declared) {
				fields = append(fields, "declared_digest", declared)
			} else {
				fields = append(fields, "invalid_declared_digest", true)
			}
		}
		return nil, manifestValidationFailure(resp, "digest_mismatch", errors.New("upstream manifest digest mismatch"), fields...)
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
	if err := json.Unmarshal(body, &metadata); err != nil {
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			return nil, manifestValidationFailure(resp, "invalid_json", err, "json_offset", syntax.Offset)
		}
		return nil, manifestValidationFailure(resp, "invalid_json", err)
	}
	if metadata.SchemaVersion != 2 {
		return nil, manifestValidationFailure(resp, "unsupported_schema_version", errors.New("unsupported upstream manifest schema version"), "schema_version", metadata.SchemaVersion)
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
		return nil, diagnoseAt("manifest_publish", cacheStorageError(err))
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
		setRequestCache(r.Context(), "hit")
		serveVerifiedBlob(w, r, digest, f)
		return
	}
	if !errors.Is(err, os.ErrNotExist) {
		setRequestCache(r.Context(), "error")
		blobRequestFailed(r.Context(), key, nil, "published_open", err)
		registryError(w, http.StatusInternalServerError, "UNKNOWN", "cannot open verified blob")
		return
	}
	setRequestCache(r.Context(), "miss")
	flight := c.joinBlobFlight(repo, key)
	if earlyBlobGET(r) {
		c.serveColdBlob(w, r, key, flight)
		return
	}
	if err := flight.wait(r.Context()); err != nil {
		if r.Context().Err() == nil {
			blobRequestFailed(r.Context(), key, flight, "", err)
			writeFetchError(w, "blobs", err)
		}
		return
	}
	c.servePublishedBlob(w, r, key, flight)
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
		expected, err := c.downloadBlob(repo, key, flight)
		if err != nil {
			failure := blobFailure(err)
			attrs := []any{"flight_id", flight.id, "repository", key.downstream, "digest", key.reference,
				"logical_registry", repo.logicalRegistry, "upstream_registry", repo.source.RegistryStr(),
				"stage", failure.stage, "reason", failure.reason}
			attrs = appendDiagnosticFields(attrs, err)
			if expected >= 0 {
				attrs = append(attrs, "bytes_expected", expected)
			}
			if failure.offset >= 0 {
				attrs = append(attrs, "offset", failure.offset)
			}
			if failure.upstreamStatus != 0 {
				attrs = append(attrs, "upstream_status", failure.upstreamStatus)
			}
			if stage, reason := diagnosticDetails(err, ""); stage != "" && stage != failure.stage {
				attrs = append(attrs, "error_stage", stage)
				if reason != "io_error" && reason != failure.reason {
					attrs = append(attrs, "error_reason", reason)
				}
			}
			if limit := quotaDimension(err); limit != "" {
				attrs = append(attrs, "limit", limit)
			}
			level := slog.LevelWarn
			switch {
			case errors.Is(err, context.Canceled), fetchUpstreamStatus(err) == http.StatusNotFound:
				level = slog.LevelDebug
			case errors.Is(err, errBusy), errors.Is(err, errCacheFull):
				level = slog.LevelInfo
			}
			slog.Log(c.ctx, level, "blob_download_failed", attrs...)
		}
		// Staging is closed and quotas are released. Retire the flight before
		// notifying waiters so an immediate retry cannot join a failed flight.
		c.blobMu.Lock()
		delete(c.blobTasks, key)
		flight.finish(err)
		c.blobMu.Unlock()
	}()
	return flight
}

func (c *mirrorCache) servePublishedBlob(w http.ResponseWriter, r *http.Request, key cacheKey, flight *blobFlight) {
	f, err := c.blobs.open(r.Context(), key)
	if err != nil {
		blobRequestFailed(r.Context(), key, flight, "published_open", err)
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

func (c *mirrorCache) downloadBlob(repo resolvedRepository, key cacheKey, flight *blobFlight) (expected int64, result error) {
	ctx, cancel := context.WithTimeout(c.ctx, 10*time.Minute)
	defer cancel()
	stage, reason := "cache_open", ""
	expected = -1
	offset, status := int64(-1), 0
	defer func() {
		if result == nil {
			return
		}
		var failure *blobTransferFailure
		if errors.As(result, &failure) {
			return
		}
		stage, classified := diagnosticDetails(result, stage)
		if reason == "" {
			reason = classified
		}
		if upstreamStatus := fetchUpstreamStatus(result); upstreamStatus != 0 {
			status = upstreamStatus
		}
		result = &blobTransferFailure{stage: stage, reason: reason, offset: offset, upstreamStatus: status, err: result}
	}()
	f, err := c.blobs.open(ctx, key)
	if err == nil {
		stage = "cache_close"
		return expected, cacheStorageError(f.Close())
	}
	if !errors.Is(err, os.ErrNotExist) {
		return expected, cacheStorageError(err)
	}
	stage = "quota_acquire"
	usage, err := c.quota.acquire(repo.logicalRegistry)
	if err != nil {
		return expected, err
	}
	defer c.quota.release(repo.logicalRegistry, usage)

	stage = "manifest_lookup"
	size, known, err := c.manifests.blobSize(ctx, key)
	if err != nil {
		return expected, cacheStorageError(err)
	}
	if known {
		expected = size
	}
	stage = "upstream_acquire"
	client, release, err := c.upstream.acquire(ctx, repo.source)
	if err != nil {
		return expected, err
	}
	defer release()
	stage = "upstream_get"
	resp, err := getUpstream(ctx, client, repo.source, "blobs", key.reference, "")
	if err != nil {
		return expected, err
	}
	status = resp.StatusCode
	defer func() {
		if resp != nil {
			_ = resp.Body.Close()
		}
	}()
	stage = "validate_length"
	if known && resp.ContentLength >= 0 && expected != resp.ContentLength {
		return expected, diagnoseFailure(stage, "length_mismatch",
			errors.New("upstream blob length does not match the Manifest"), appendUpstreamHost([]any{"bytes_actual", resp.ContentLength}, resp.Request)...)
	}
	if !known && resp.ContentLength >= 0 {
		expected, known = resp.ContentLength, true
	}
	if known && expected > maxBlobBytes {
		reason = "blob_too_large"
		return expected, errors.New("upstream blob length above single-blob limit")
	}
	limit, limitDimension := blobDownloadLimit(c.quota.limits)
	stage = "quota_reserve"
	if known {
		if expected > limit {
			return expected, quotaLimit(errCacheFull, limitDimension)
		}
		limit = expected
	}
	if err := c.quota.reserve(usage, limit); err != nil {
		return expected, err
	}
	defer c.quota.releaseStaging(usage, limit)
	stage = "staging_create"

	file, err := os.CreateTemp(c.staging, "blob-")
	if err != nil {
		return expected, cacheStorageError(err)
	}
	defer os.Remove(file.Name())
	// Close the shared staging reader before removing the stage and releasing
	// its reservation. No downstream network write holds that descriptor.
	defer flight.closeStaging()
	var progress io.Writer
	if known && expected > 1 {
		stage = "staging_open"
		reader, err := os.Open(file.Name())
		if err != nil {
			_ = file.Close()
			return expected, cacheStorageError(err)
		}
		flight.startStreaming(reader, expected)
		progress = blobProgress{flight}
	}
	initial := resp
	resp = nil // The transfer owns the initial body, including on a resume.
	stage = "transfer"
	n, err := copyVerifiedBlob(file, initial, client, key.reference, limit, known, progress)
	offset = n
	if err != nil {
		_ = file.Close()
		if errors.Is(err, errCacheFull) {
			err = quotaLimit(err, limitDimension)
		}
		return expected, err
	}
	stage = "staging_sync_close"
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return expected, cacheStorageError(err)
	}
	stage = "before_publish"
	if err := ctx.Err(); err != nil {
		return expected, err
	}
	stage = "publish"
	if err := c.blobs.publish(ctx, key, file.Name(), n); err != nil {
		return expected, cacheStorageError(err)
	}
	log.Printf("mirror blob published digest=%s bytes=%d", key.reference, n)
	return expected, nil
}

func blobDownloadLimit(l cacheLimits) (int64, string) {
	limit, dimension := int64(maxBlobBytes), "max_blob_bytes"
	for _, candidate := range [...]struct {
		bytes int64
		name  string
	}{
		{l.blobBytes, "cache-blob-bytes"},
		{l.stagingBytes, "cache-staging-bytes"},
		{l.upstreamStagingBytes, "cache-upstream-staging-bytes"},
	} {
		if candidate.bytes < limit {
			limit, dimension = candidate.bytes, candidate.name
		}
	}
	return limit, dimension
}

type blobProgress struct{ flight *blobFlight }

func (p blobProgress) Write(data []byte) (int, error) {
	p.flight.advance(len(data))
	return len(data), nil
}

func boundedMediaType(mediaType string) string {
	if len(mediaType) > 128 {
		return mediaType[:128]
	}
	return mediaType
}

func manifestValidationFailure(resp *http.Response, reason string, err error, fields ...any) error {
	return &diagnosticError{stage: "manifest_validate", reason: reason, upstreamStatus: resp.StatusCode, err: err, fields: appendUpstreamHost(fields, resp.Request)}
}

func manifestRequestFailed(w http.ResponseWriter, r *http.Request, repo resolvedRepository, ref string, err error) {
	status, code, message := fetchFailureResponse("manifests", err)
	stage, reason := diagnosticDetails(err, "manifest_fetch")
	ctx := r.Context()
	attrs := []any{"request_id", requestID(ctx), "repository", repo.downstream, "reference", ref,
		"logical_registry", repo.logicalRegistry, "upstream_registry", repo.source.RegistryStr(),
		"stage", stage, "reason", reason, "response_status", status}
	attrs = appendDiagnosticFields(attrs, err)
	if upstreamStatus := diagnosticUpstreamStatus(err); upstreamStatus != 0 {
		attrs = append(attrs, "upstream_status", upstreamStatus)
	}
	if limit := quotaDimension(err); limit != "" {
		attrs = append(attrs, "limit", limit)
	}
	level := slog.LevelWarn
	switch {
	case ctx.Err() != nil, status == http.StatusNotFound, status == http.StatusNotAcceptable:
		level = slog.LevelDebug
	case errors.Is(err, errBusy), errors.Is(err, errCacheFull):
		level = slog.LevelInfo
	}
	slog.Log(ctx, level, "manifest_request_failed", attrs...)
	registryError(w, status, code, message)
}

func writeFetchError(w http.ResponseWriter, kind string, err error) {
	status, code, message := fetchFailureResponse(kind, err)
	registryError(w, status, code, message)
}

func fetchUpstreamStatus(err error) int {
	var upstream *upstreamStatus
	var transport *registry.Error
	if errors.As(err, &upstream) {
		return upstream.status
	}
	if errors.As(err, &transport) {
		return transport.StatusCode
	}
	return 0
}

func fetchFailureResponse(kind string, err error) (status int, code, message string) {
	upstreamStatus := fetchUpstreamStatus(err)
	switch {
	case errors.Is(err, errBusy):
		return http.StatusServiceUnavailable, "UNAVAILABLE", "global or upstream cold fetch limit reached"
	case errors.Is(err, errCacheFull):
		return http.StatusInsufficientStorage, "UNAVAILABLE", "Mirror cache quota exhausted"
	case errors.Is(err, errCacheStorage):
		return http.StatusInternalServerError, "UNKNOWN", "local Mirror cache failed"
	case errors.Is(err, errPublicURL), errors.Is(err, errPublicIP), errors.Is(err, errPrivateDNS):
		return http.StatusBadGateway, "UNAVAILABLE", "upstream rejected by egress policy"
	case upstreamStatus == http.StatusNotFound:
		code := "MANIFEST_UNKNOWN"
		if kind == "blobs" {
			code = "BLOB_UNKNOWN"
		}
		return upstreamStatus, code, "content not found upstream"
	case upstreamStatus == http.StatusNotAcceptable && kind == "manifests":
		return upstreamStatus, "MANIFEST_UNKNOWN", "upstream manifest media type not accepted"
	default:
		return http.StatusBadGateway, "UNAVAILABLE", "upstream fetch or verification failed"
	}
}

func registryError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": message}}})
}
