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
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	registry "github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"golang.org/x/sync/singleflight"
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
	errBusy = errors.New("all cold fetch slots are in use")
)

type mirrorCache struct {
	ctx            context.Context
	source         publicSource
	blobs, staging string
	slots          chan struct{}
	blobTasks      singleflight.Group

	mu        sync.Mutex
	manifests map[string]*manifest
	blobSizes map[string]int64
}

type manifest struct {
	mediaType, digest string
	body              []byte
}

func newMirrorCache(ctx context.Context, dir string, source publicSource) (*mirrorCache, error) {
	// Exact repository == upstream in the PoC. Never share publication or
	// in-flight keys with a different repository, even for the same digest.
	key := sha256.Sum256([]byte(source.repo.Name()))
	root := filepath.Join(dir, hex.EncodeToString(key[:]))
	blobs := filepath.Join(root, "blobs")
	if err := os.MkdirAll(blobs, 0700); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp(root, ".staging-")
	if err != nil {
		return nil, err
	}
	return &mirrorCache{
		ctx: ctx, source: source, blobs: blobs, staging: staging, slots: make(chan struct{}, 4),
		manifests: make(map[string]*manifest), blobSizes: make(map[string]int64),
	}, nil
}

func (c *mirrorCache) serve(w http.ResponseWriter, r *http.Request, read readRequest) {
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	if read.kind == "manifests" {
		c.serveManifest(w, r, read.reference)
	} else {
		c.serveBlob(w, r, read.reference)
	}
}

func (c *mirrorCache) serveManifest(w http.ResponseWriter, r *http.Request, ref string) {
	accept := r.Header.Values("Accept")
	c.mu.Lock()
	m := c.manifests[ref]
	c.mu.Unlock()
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
		var err error
		m, err = c.fetchManifest(ctx, ref, allowed)
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

func (c *mirrorCache) fetchManifest(ctx context.Context, ref string, allowed []string) (*manifest, error) {
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	default:
		return nil, errBusy
	}
	resp, err := c.source.get(ctx, "manifests", ref, strings.Join(allowed, ", "))
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

	c.mu.Lock()
	defer c.mu.Unlock()
	if pinned := c.manifests[ref]; pinned != nil {
		return pinned, nil
	}
	m := &manifest{mediaType: mediaType, digest: digest, body: body}
	if existing := c.manifests[digest]; existing != nil {
		m = existing
	}
	c.manifests[digest] = m
	c.manifests[ref] = m
	remember := func(d descriptor) {
		if _, known := c.blobSizes[d.Digest]; !known && d.Size != nil && *d.Size >= 0 && digestPattern.MatchString(d.Digest) {
			c.blobSizes[d.Digest] = *d.Size
		}
	}
	remember(metadata.Config)
	for _, layer := range metadata.Layers {
		remember(layer)
	}
	log.Printf("mirror manifest published digest=%s", digest)
	return m, nil
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

func (c *mirrorCache) serveBlob(w http.ResponseWriter, r *http.Request, digest string) {
	path := filepath.Join(c.blobs, strings.TrimPrefix(digest, "sha256:"))
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		task := c.blobTasks.DoChan(digest, func() (any, error) {
			return nil, c.downloadBlob(digest, path)
		})
		select {
		case <-r.Context().Done():
			return
		case result := <-task:
			if result.Err != nil {
				fetchError(w, "blobs", result.Err)
				return
			}
		}
		f, err = os.Open(path)
	}
	if err != nil {
		registryError(w, http.StatusInternalServerError, "UNKNOWN", "cannot open verified blob")
		return
	}
	defer f.Close()
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("ETag", `"`+digest+`"`)
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, digest, time.Time{}, f)
}

func (c *mirrorCache) downloadBlob(digest, target string) error {
	if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	select {
	case c.slots <- struct{}{}:
		defer func() { <-c.slots }()
	default:
		return errBusy
	}
	ctx, cancel := context.WithTimeout(c.ctx, 10*time.Minute)
	defer cancel()
	resp, err := c.source.get(ctx, "blobs", digest, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	c.mu.Lock()
	expected, known := c.blobSizes[digest]
	c.mu.Unlock()
	if resp.ContentLength > maxBlobBytes || (known && (expected > maxBlobBytes || (resp.ContentLength >= 0 && expected != resp.ContentLength))) {
		return errors.New("upstream blob length invalid or above limit")
	}
	file, err := os.CreateTemp(c.staging, "blob-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	hash := sha256.New()
	limit := int64(maxBlobBytes)
	if known {
		limit = expected
	}
	n, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(resp.Body, limit+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || n > limit || (known && n != expected) || (resp.ContentLength >= 0 && n != resp.ContentLength) {
		return errors.New("blob staging write failed or downloaded length mismatched")
	}
	if "sha256:"+hex.EncodeToString(hash.Sum(nil)) != digest {
		return errors.New("upstream blob digest mismatch")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), target); err != nil {
		return err
	}
	log.Printf("mirror blob published digest=%s bytes=%d", digest, n)
	return nil
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
		registryError(w, http.StatusServiceUnavailable, "UNAVAILABLE", "all cold fetch slots are in use")
	case errors.Is(err, errPublicURL), errors.Is(err, errPublicIP):
		log.Printf("mirror egress policy rejected kind=%s", kind)
		registryError(w, http.StatusBadGateway, "UNAVAILABLE", "upstream rejected by public egress policy")
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
