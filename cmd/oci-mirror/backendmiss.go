package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"
)

// The backend must authorize the exact resource before looking it up. These
// corroborating requests observe a point in time; they do not serialize pushes.
func trustedBackendMiss(resp *http.Response, read readRequest, transport http.RoundTripper) bool {
	if !backendJSON404(resp) || resp.Request == nil {
		return false
	}
	original := resp.Request
	var method string
	switch original.Method {
	case http.MethodGet:
		miss := backendTypedMiss(resp, read)
		if miss == notBackendMiss {
			return false
		}
		// Zot v2.1.15 GetManifest also maps ErrRepoBadVersion to 404
		// NAME_UNKNOWN. Its CheckManifest returns 500, disambiguating it.
		if read.kind != "manifests" || miss != zotNameMiss {
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

	probe := &http.Request{
		Method: method,
		URL:    original.URL,
		Host:   original.Host,
		Header: original.Header.Clone(),
	}
	// Keep the backend-facing authentication/session and negotiation context,
	// but perform a fresh, unconditional request without a body.
	for _, key := range []string{
		"Range", "If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "If-Range",
		"Content-Length", "Content-Type", "Content-Encoding", "Transfer-Encoding", "Trailer", "Expect",
	} {
		probe.Header.Del(key)
	}
	confirmed, err := transport.RoundTrip(probe.WithContext(original.Context()))
	if err != nil {
		return false
	}
	defer confirmed.Body.Close()
	return backendJSON404(confirmed) && (method == http.MethodHead || backendTypedMiss(confirmed, read) != notBackendMiss)
}

func backendJSON404(resp *http.Response) bool {
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

type backendBufferedBody struct {
	reader *bufio.Reader
	io.Closer
	peekErr error
}

func (b *backendBufferedBody) Read(p []byte) (int, error) {
	if b.reader.Buffered() == 0 && b.peekErr != nil {
		err := b.peekErr
		b.peekErr = nil
		return 0, err
	}
	return b.reader.Read(p)
}

type backendMissKind uint8

const (
	notBackendMiss backendMissKind = iota
	exactBackendMiss
	zotNameMiss
)

// backendTypedMiss inspects a JSON 404 only for known detail shapes tied to
// the original backend repository or reference, never its upstream alias.
func backendTypedMiss(resp *http.Response, read readRequest) backendMissKind {
	const maxErrorBytes = 16 << 10
	if resp.Body == nil || resp.ContentLength > maxErrorBytes {
		return notBackendMiss
	}
	// Peek retains the bytes but consumes the pending read error if the
	// buffer is short. Replay that error after forwarding the buffered bytes.
	buffered := &backendBufferedBody{reader: bufio.NewReaderSize(resp.Body, maxErrorBytes+1), Closer: resp.Body}
	resp.Body = buffered
	data, err := buffered.reader.Peek(maxErrorBytes + 1)
	buffered.peekErr = err
	if err != io.EOF || !utf8.Valid(data) {
		return notBackendMiss
	}
	// A blank or mixed-error 404 is not evidence.
	envelope := backendJSONObject(data)
	var entries []json.RawMessage
	if json.Unmarshal(envelope["errors"], &entries) != nil || len(entries) != 1 {
		return notBackendMiss
	}
	entry := backendJSONObject(entries[0])
	var code string
	if json.Unmarshal(entry["code"], &code) != nil {
		return notBackendMiss
	}
	rawDetail := entry["detail"]
	repo, ref := read.repository.downstream, read.reference
	if len(rawDetail) != 0 && rawDetail[0] == '"' {
		// Distribution 3.1.2 serializes error details as strings. Match
		// only its missing-resource formats, not negotiation failures.
		var detail string
		if json.Unmarshal(rawDetail, &detail) != nil {
			return notBackendMiss
		}
		switch {
		case code == "NAME_UNKNOWN":
			if name, ok := strings.CutPrefix(detail, "unknown repository name="); ok && name == repo {
				return exactBackendMiss
			}
		case code == "MANIFEST_UNKNOWN" && read.kind == "manifests":
			if tag, ok := strings.CutPrefix(detail, "unknown tag="); ok && tag == ref && tagPattern.MatchString(ref) {
				return exactBackendMiss
			}
			if revision, ok := strings.CutPrefix(detail, "unknown manifest name="); ok {
				name, digest, ok := strings.Cut(revision, " revision=")
				if ok && name == repo && digest == ref && digestPattern.MatchString(ref) {
					return exactBackendMiss
				}
			}
		case code == "BLOB_UNKNOWN" && read.kind == "blobs":
			if detail == ref {
				return exactBackendMiss
			}
		}
		return notBackendMiss
	}
	detail := backendJSONObject(rawDetail)
	switch {
	case code == "NAME_UNKNOWN":
		if backendStringMatches(detail["name"], repo) {
			return zotNameMiss
		}
		if len(detail) == 1 && backendStringMatches(detail["Name"], repo) {
			return exactBackendMiss
		}
	case code == "MANIFEST_UNKNOWN" && read.kind == "manifests":
		if backendStringMatches(detail["reference"], ref) {
			return exactBackendMiss
		}
		// Distribution serializes ErrTagUnknown as {Tag}, or a named tag
		// error as {Name,Tag}. A missing revision only proves a digest
		// request missed, not that a tag resolving to that digest missed.
		nameMatches := backendStringMatches(detail["Name"], repo)
		switch {
		case (len(detail) == 1 || len(detail) == 2 && nameMatches) && backendStringMatches(detail["Tag"], ref) && tagPattern.MatchString(ref):
			return exactBackendMiss
		case len(detail) == 2 && nameMatches && backendStringMatches(detail["Revision"], ref) && digestPattern.MatchString(ref):
			return exactBackendMiss
		}
	case code == "BLOB_UNKNOWN" && read.kind == "blobs":
		if backendStringMatches(detail["digest"], ref) {
			return exactBackendMiss
		}
	}
	return notBackendMiss
}

func backendStringMatches(data json.RawMessage, want string) bool {
	var got string
	return want != "" && json.Unmarshal(data, &got) == nil && got == want
}

// Decode with exact case-sensitive field names and reject repeated keys.
func backendJSONObject(data []byte) map[string]json.RawMessage {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil
		}
		key := token.(string)
		if _, exists := fields[key]; exists {
			return nil
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil
		}
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil
	}
	return fields
}
