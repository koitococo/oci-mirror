package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// The operator mounts this exact configuration and certificate read-only for
// both processes. Startup checks the challenge, not Zot's in-memory policies;
// enabling hot reload or inserting an intermediary breaks this trust contract.
func loadZotAuth(path, issuer string) (*bearerAuth, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(data) {
		return nil, errors.New("Zot config must be UTF-8 JSON")
	}
	root, err := zotJSONObject(data)
	if err != nil {
		return nil, fmt.Errorf("Zot config: %w", err)
	}
	if zotConfigured(root["cluster"]) {
		return nil, errors.New("dedicated Zot must not configure cluster")
	}
	if zotConfigured(root["extensions"]) {
		extensions, err := zotJSONObject(root["extensions"])
		if err != nil {
			return nil, fmt.Errorf("Zot extensions: %w", err)
		}
		if zotConfigured(extensions["sync"]) {
			return nil, errors.New("dedicated Zot must not configure extensions.sync")
		}
	}
	httpConfig, err := zotJSONObject(root["http"])
	if err != nil {
		return nil, fmt.Errorf("Zot http: %w", err)
	}
	if zotConfigured(httpConfig["accesscontrol"]) {
		return nil, errors.New("dedicated Zot must not configure http.accessControl")
	}
	authConfig, err := zotJSONObject(httpConfig["auth"])
	if err != nil {
		return nil, fmt.Errorf("Zot http.auth: %w", err)
	}
	if len(authConfig) != 1 || !zotConfigured(authConfig["bearer"]) {
		return nil, errors.New("Zot http.auth must contain only bearer")
	}
	bearer, err := zotJSONObject(authConfig["bearer"])
	if err != nil {
		return nil, fmt.Errorf("Zot http.auth.bearer: %w", err)
	}
	if len(bearer) != 3 {
		return nil, errors.New("Zot http.auth.bearer must contain only realm, service and cert")
	}
	var realm, service, cert string
	for key, dst := range map[string]*string{"realm": &realm, "service": &service, "cert": &cert} {
		if json.Unmarshal(bearer[key], dst) != nil || *dst == "" {
			return nil, fmt.Errorf("Zot http.auth.bearer.%s must be a nonempty string", key)
		}
	}
	if !filepath.IsAbs(cert) {
		return nil, errors.New("Zot http.auth.bearer.cert must be an absolute shared path")
	}
	return newBearerAuth(cert, issuer, service, realm)
}

func zotConfigured(value json.RawMessage) bool {
	return len(value) != 0 && !bytes.Equal(value, []byte("null"))
}

// Zot v2.1.15 loads case-insensitive Viper keys with "::" as the nesting
// delimiter. Reject duplicate spellings and delimiter aliases rather than
// guessing which value Zot would use. Unrelated subtrees stay opaque.
func zotJSONObject(data []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, errors.New("expected a JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key := strings.ReplaceAll(strings.ToLower(token.(string)), "ſ", "s")
		if _, exists := fields[key]; exists || strings.Contains(key, "::") {
			return nil, fmt.Errorf("ambiguous JSON key %q", token)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("unexpected data after JSON object")
	}
	return fields, nil
}

// The supplied transport is the dedicated, proxy-disabled Zot transport. A
// single RoundTrip cannot follow redirects or pick up client credentials.
func verifyZot(ctx context.Context, origin *url.URL, auth *bearerAuth, transport http.RoundTripper) error {
	addr, err := netip.ParseAddr(origin.Hostname())
	if err != nil || !addr.IsLoopback() || origin.Scheme != "http" || origin.User != nil {
		return errors.New("Zot verification requires a direct loopback HTTP origin")
	}
	endpoint := url.URL{Scheme: origin.Scheme, Host: origin.Host, Path: "/v2/"}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return err
	}
	resp, err := transport.RoundTrip(req)
	if err != nil {
		return fmt.Errorf("probe Zot /v2/: %w", err)
	}
	defer resp.Body.Close()
	challenge := resp.Header.Values("WWW-Authenticate")
	if resp.StatusCode != http.StatusUnauthorized || len(challenge) != 1 || challenge[0] != auth.challengeHeader("") {
		return fmt.Errorf("Zot /v2/ did not return the configured unscoped Bearer challenge (status %d)", resp.StatusCode)
	}
	return nil
}

// The caller has checked exact pull scope and supplies the original, direct Zot
// response. These probes observe a point in time; neither serializes a
// concurrent push. The original result stands if either observation disagrees.
func trustedZotMiss(resp *http.Response, read readRequest, transport http.RoundTripper) bool {
	if !zotJSON404(resp) || resp.Request == nil {
		return false
	}
	method := resp.Request.Method
	switch method {
	case http.MethodGet:
		code := zotTypedMiss(resp, read)
		if code == "" {
			return false
		}
		// Only GetManifest maps ErrRepoBadVersion to 404 NAME_UNKNOWN.
		// CheckManifest returns 500 for that error; disambiguate after GET.
		if read.kind != "manifests" || code != "NAME_UNKNOWN" {
			return true
		}
		method = http.MethodHead
	case http.MethodHead:
		if read.kind != "manifests" {
			return false
		}
		// HEAD has no wire body, so corroborate with the typed GET result.
		method = http.MethodGet
	default:
		return false
	}

	original := resp.Request
	probe := &http.Request{
		Method: method,
		URL:    original.URL,
		Host:   original.Host,
		Header: make(http.Header),
	}
	for _, key := range []string{"Authorization", "Accept"} {
		for _, value := range original.Header.Values(key) {
			probe.Header.Add(key, value)
		}
	}
	confirmed, err := transport.RoundTrip(probe.WithContext(original.Context()))
	if err != nil {
		return false
	}
	defer confirmed.Body.Close()
	if !zotJSON404(confirmed) {
		return false
	}
	return method == http.MethodHead || zotTypedMiss(confirmed, read) != ""
}

func zotJSON404(resp *http.Response) bool {
	if resp.StatusCode != http.StatusNotFound || len(resp.Header.Values("WWW-Authenticate")) != 0 {
		return false
	}
	types := resp.Header.Values("Content-Type")
	if len(types) != 1 {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(types[0])
	return err == nil && mediaType == "application/json"
}

type zotBufferedBody struct {
	*bufio.Reader
	io.Closer
}

// zotTypedMiss inspects a previously validated JSON 404 and returns its code
// only if the detail identifies the exact original repository or reference.
func zotTypedMiss(resp *http.Response, read readRequest) string {
	if resp.Body == nil {
		return ""
	}
	const maxErrorBytes = 16 << 10
	if resp.ContentLength > maxErrorBytes {
		return ""
	}
	// Peek leaves every byte (and any terminal read error) available to the
	// reverse proxy, and Close still reaches the original transport body.
	buffered := bufio.NewReaderSize(resp.Body, maxErrorBytes+1)
	resp.Body = &zotBufferedBody{buffered, resp.Body}
	data, err := buffered.Peek(maxErrorBytes + 1)
	if err != io.EOF || !utf8.Valid(data) {
		return ""
	}
	// The pinned GetManifest/GetBlob handlers write one Distribution error
	// entry on a typed miss. A blank or mixed-error 404 is not evidence.
	envelope, err := zotJSONObject(data)
	if err != nil {
		return ""
	}
	var entries []json.RawMessage
	if json.Unmarshal(envelope["errors"], &entries) != nil || len(entries) != 1 {
		return ""
	}
	entry, err := zotJSONObject(entries[0])
	if err != nil {
		return ""
	}
	var code string
	if json.Unmarshal(entry["code"], &code) != nil {
		return ""
	}
	var key, want string
	switch {
	case code == "NAME_UNKNOWN":
		key, want = "name", read.repository.downstream
	case code == "MANIFEST_UNKNOWN" && read.kind == "manifests":
		key, want = "reference", read.reference
	case code == "BLOB_UNKNOWN" && read.kind == "blobs":
		key, want = "digest", read.reference
	default:
		return ""
	}
	detail, err := zotJSONObject(entry["detail"])
	if err != nil {
		return ""
	}
	var got string
	if want == "" || json.Unmarshal(detail[key], &got) != nil || got != want {
		return ""
	}
	return code
}
