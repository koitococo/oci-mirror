package main

import (
	"errors"
	"net/http"
	"net/netip"
	"strings"

	"github.com/distribution/reference"
	"github.com/google/go-containerregistry/pkg/name"
)

const (
	dockerHub         = "docker.io"
	dockerHubLegacy   = "index.docker.io"
	dockerHubEndpoint = "registry-1.docker.io"
)

type resolvedRepository struct {
	downstream      string // Zot and its authorization always see precisely this name.
	logicalRegistry string
	source          name.Repository
}

func registryDNS(host string) bool {
	_, err := netip.ParseAddr(host)
	return err != nil && len(host) <= 253 && dnsPattern.MatchString(host)
}

func logicalRegistry(host string) string {
	switch host {
	case dockerHubLegacy, dockerHubEndpoint:
		return dockerHub
	default:
		return host
	}
}

func registryFlag(set map[string]struct{}) func(string) error {
	return func(host string) error {
		host = strings.ToLower(host)
		if !registryDNS(host) {
			return errors.New("registry policy requires a DNS hostname without port or IP literal")
		}
		set[logicalRegistry(host)] = struct{}{}
		return nil
	}
}

func resolveRepository(raw string) (resolvedRepository, error) {
	if raw != strings.ToLower(raw) || strings.ContainsAny(raw, ":@") {
		return resolvedRepository{}, errors.New("expected a lowercase OCI repository without port, tag or digest")
	}
	named, err := reference.ParseNormalizedNamed(raw)
	if err != nil {
		return resolvedRepository{}, err
	}
	host := reference.Domain(named)
	if !registryDNS(host) {
		return resolvedRepository{}, errors.New("upstream registry must be a DNS hostname without port or IP literal")
	}
	logical := logicalRegistry(host)
	if logical == dockerHub {
		if host == dockerHubEndpoint {
			// The Docker parser knows docker.io, not its wire endpoint alias.
			named, err = reference.ParseNormalizedNamed(dockerHub + "/" + reference.Path(named))
			if err != nil {
				return resolvedRepository{}, err
			}
		}
		host = dockerHubEndpoint
	}
	upstream, err := name.NewRepository(host+"/"+reference.Path(named), name.StrictValidation)
	if err != nil {
		return resolvedRepository{}, err
	}
	return resolvedRepository{downstream: raw, logicalRegistry: logical, source: upstream}, nil
}

type fallbackPolicy struct {
	allowed, denied map[string]struct{}
}

func (p fallbackPolicy) rejection(repo resolvedRepository) string {
	if _, denied := p.denied[repo.logicalRegistry]; denied {
		return "registry_denied"
	}
	if _, allowed := p.allowed[repo.logicalRegistry]; len(p.allowed) != 0 && !allowed {
		return "registry_not_allowed"
	}
	return ""
}

// classify runs only after a candidate backend 404. Normalization chooses
// the upstream without changing the backend's repository name or authority.
func (p fallbackPolicy) classify(r *http.Request) (readRequest, string) {
	if r.URL.RawQuery != "" || r.URL.ForceQuery || r.URL.EscapedPath() != r.URL.Path {
		return readRequest{}, ""
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/v2/")
	if !ok {
		return readRequest{}, ""
	}
	refSep := strings.LastIndexByte(path, '/')
	if refSep < 0 {
		return readRequest{}, ""
	}
	kindSep := strings.LastIndexByte(path[:refSep], '/')
	if kindSep < 0 {
		return readRequest{}, ""
	}
	kind, ref := path[kindSep+1:refSep], path[refSep+1:]
	switch kind {
	case "manifests":
		if (r.Method != http.MethodGet && r.Method != http.MethodHead) ||
			(!tagPattern.MatchString(ref) && !digestPattern.MatchString(ref)) {
			return readRequest{}, ""
		}
	case "blobs":
		if r.Method != http.MethodGet || !digestPattern.MatchString(ref) {
			return readRequest{}, ""
		}
	default:
		return readRequest{}, ""
	}
	repo, err := resolveRepository(path[:kindSep])
	read := readRequest{kind: kind, reference: ref, repository: repo}
	if err != nil {
		return read, "invalid_repository"
	}
	return read, p.rejection(repo)
}
