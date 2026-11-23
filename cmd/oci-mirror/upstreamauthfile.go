package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/distribution/reference"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

type upstreamAuthScope struct {
	host string
	path string
}

type upstreamAuthEntry struct {
	auth     authn.Authenticator
	priority int
}

type upstreamAuthFile map[upstreamAuthScope]upstreamAuthEntry

var upstreamAuthHost = regexp.MustCompile("^" + reference.DomainRegexp.String() + "$")

func loadUpstreamAuthFile(path string) upstreamAuthFile {
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("upstream_authfile_unreadable")
		return nil
	}
	var config *struct {
		Auths       map[string]json.RawMessage `json:"auths"`
		CredsStore  json.RawMessage            `json:"credsStore"`
		CredHelpers json.RawMessage            `json:"credHelpers"`
	}
	if json.Unmarshal(data, &config) != nil || config == nil {
		slog.Warn("upstream_authfile_invalid")
		return nil
	}
	var store string
	var helpers map[string]json.RawMessage
	if (config.CredsStore != nil && json.Unmarshal(config.CredsStore, &store) != nil) ||
		(config.CredHelpers != nil && json.Unmarshal(config.CredHelpers, &helpers) != nil) ||
		store != "" || len(helpers) != 0 {
		slog.Warn("upstream_authfile_helpers_ignored")
	}

	// Sort before normalizing so equivalent spellings at the same priority
	// have a predictable winner, independent of map iteration order.
	keys := make([]string, 0, len(config.Auths))
	for key := range config.Auths {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	entries := make(upstreamAuthFile, len(keys))
	for _, key := range keys {
		scope, priority, ok := parseUpstreamAuthScope(key)
		var configEntry authn.AuthConfig
		if !ok || json.Unmarshal(config.Auths[key], &configEntry) != nil {
			slog.Warn("upstream_authfile_entry_invalid")
			continue
		}
		if configEntry.IdentityToken == "" && configEntry.RegistryToken == "" &&
			(configEntry.Username == "" || configEntry.Password == "") {
			// Empty entries are normal for registries using external helpers.
			if configEntry.Auth != "" || configEntry.Username != "" || configEntry.Password != "" {
				slog.Warn("upstream_authfile_entry_invalid")
			}
			continue
		}
		if previous, exists := entries[scope]; !exists || priority < previous.priority {
			entries[scope] = upstreamAuthEntry{authn.FromConfig(configEntry), priority}
		}
	}
	return entries
}

func (p *upstreamPool) reloadAuthFile(path string) {
	file := loadUpstreamAuthFile(path)
	if file == nil {
		// A read or parse failure leaves the last valid snapshot in use.
		return
	}
	p.mu.Lock()
	p.auth.file = file
	// Existing leases can finish on their old scope. Drop cached
	// credentials and tokens for new leases without resetting rate limits.
	for _, entry := range p.registries {
		r := entry.Value.(*upstreamRegistry)
		clear(r.scopes)
		r.lru.Init()
	}
	p.mu.Unlock()
	slog.Info("upstream_authfile_reloaded")
}

func parseUpstreamAuthScope(key string) (upstreamAuthScope, int, bool) {
	scheme, address, urlKey := strings.Cut(key, "://")
	if urlKey {
		if scheme != "https" && scheme != "http" {
			return upstreamAuthScope{}, 0, false
		}
		key = address
	}
	host, path, _ := strings.Cut(key, "/")
	host = strings.ToLower(host)
	if !upstreamAuthHost.MatchString(host) {
		return upstreamAuthScope{}, 0, false
	}

	priority := 0
	switch host {
	case dockerHubLegacy:
		priority = 1
	case dockerHubEndpoint:
		priority = 2
	}
	if urlKey {
		// Docker's legacy Hub key is the sole URL whose nonempty path
		// denotes a host-wide credential, rather than a repository scope.
		if scheme == "https" && host == dockerHubLegacy && path == "v1/" {
			return upstreamAuthScope{host: dockerHub}, 3, true
		}
		priority += 4
	}
	if path != "" {
		ref, err := reference.ParseNamed("registry.invalid/" + path)
		if err != nil || !reference.IsNameOnly(ref) {
			return upstreamAuthScope{}, 0, false
		}
	}
	return upstreamAuthScope{host: logicalRegistry(host), path: path}, priority, true
}

func (f upstreamAuthFile) resolve(repo name.Repository) authn.Authenticator {
	if len(f) == 0 {
		return authn.Anonymous
	}
	scope := upstreamAuthScope{host: logicalRegistry(strings.ToLower(repo.RegistryStr())), path: repo.RepositoryStr()}
	for {
		if entry, ok := f[scope]; ok {
			return entry.auth
		}
		if end := strings.LastIndexByte(scope.path, '/'); end >= 0 {
			scope.path = scope.path[:end]
		} else if scope.path != "" {
			scope.path = ""
		} else {
			return authn.Anonymous
		}
	}
}
