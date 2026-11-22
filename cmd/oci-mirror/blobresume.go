package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

func copyVerifiedBlob(file *os.File, initial *http.Response, client *http.Client, digest string, limit int64, exact bool, progress io.Writer) (int64, error) {
	body := initial.Body
	defer func() {
		_ = body.Close()
	}()

	// Both reads use the same stage, hash, and progress writer. The offset
	// counted by io.Copy is the prefix all of those writers accepted.
	h := sha256.New()
	writers := [3]io.Writer{file, h, progress}
	count := 2
	if progress != nil {
		count++
	}
	stage := io.MultiWriter(writers[:count]...)
	n, err := io.Copy(stage, io.LimitReader(blobReadSource{body}, limit))
	if readErr, ok := err.(blobReadError); ok {
		err = readErr.error
	} else if err != nil {
		return 0, blobStageWriteError(err)
	}
	if err == nil && exact && n < limit {
		err = errors.New("upstream blob shorter than expected")
	}
	if err != nil {
		etag := blobStrongETag(initial.Header)
		if !exact || n <= 0 || n >= limit || etag == "" || !blobIdentityEncoding(initial) ||
			initial.Request == nil || initial.Request.URL == nil || client == nil {
			return 0, err
		}
		if err := initial.Request.Context().Err(); err != nil {
			return 0, err
		}
		_ = body.Close()
		// The deferred close now belongs to the resumed segment, if any.
		body = http.NoBody
		resume, err := requestBlobSuffix(initial.Request, client, n, etag)
		if err != nil {
			return 0, err
		}
		body = resume.Body
		if !validBlobSuffix(resume, n, limit, etag) {
			return 0, errors.New("upstream blob resume response does not match the requested suffix")
		}
		remaining := limit - n
		copied, err := io.Copy(stage, io.LimitReader(blobReadSource{body}, remaining))
		if readErr, ok := err.(blobReadError); ok {
			return 0, readErr.error
		}
		if err != nil {
			return 0, blobStageWriteError(err)
		}
		if copied != remaining {
			return 0, errors.New("upstream blob resume response shorter than expected")
		}
		n += copied
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

// Tag non-EOF read errors so a failed stage write never causes a Range GET.
type blobReadError struct{ error }

type blobReadSource struct{ io.Reader }

func (r blobReadSource) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err != nil && err != io.EOF {
		err = blobReadError{err}
	}
	return n, err
}

func blobStageWriteError(err error) error {
	var pathError *os.PathError
	if errors.As(err, &pathError) {
		return cacheStorageError(err)
	}
	return err
}

func requestBlobSuffix(initial *http.Request, client *http.Client, offset int64, etag string) (*http.Response, error) {
	// Use the effective URL (possibly a signed CDN URL), but not the old
	// headers. Authentication and egress policy still belong to the transport.
	req, err := http.NewRequestWithContext(initial.Context(), http.MethodGet, initial.URL.String(), nil)
	if err != nil {
		return nil, errors.New("could not construct upstream blob resume request")
	}
	req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
	req.Header.Set("If-Range", etag)
	resumeClient := *client
	resumeClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return resumeClient.Do(req)
}

func validBlobSuffix(resp *http.Response, offset, total int64, etag string) bool {
	if resp.StatusCode != http.StatusPartialContent || blobStrongETag(resp.Header) != etag || !blobIdentityEncoding(resp) {
		return false
	}
	for _, value := range resp.Header.Values("Content-Type") {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "multipart/") {
			return false
		}
	}
	length := total - offset
	if resp.ContentLength >= 0 && resp.ContentLength != length {
		return false
	}
	if values := resp.Header.Values("Content-Length"); len(values) != 0 && (len(values) != 1 || !blobDecimalIs(values[0], length)) {
		return false
	}
	values := resp.Header.Values("Content-Range")
	if len(values) != 1 {
		return false
	}
	interval, ok := strings.CutPrefix(values[0], "bytes ")
	if !ok {
		return false
	}
	start, rest, ok := strings.Cut(interval, "-")
	if !ok || !blobDecimalIs(start, offset) {
		return false
	}
	end, size, ok := strings.Cut(rest, "/")
	return ok && blobDecimalIs(end, total-1) && blobDecimalIs(size, total)
}

func blobDecimalIs(s string, want int64) bool {
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return err == nil && n == want
}

func blobIdentityEncoding(resp *http.Response) bool {
	values := resp.Header.Values("Content-Encoding")
	return !resp.Uncompressed && (len(values) == 0 || len(values) == 1 && strings.EqualFold(values[0], "identity"))
}

func blobStrongETag(header http.Header) string {
	values := header.Values("ETag")
	if len(values) != 1 {
		return ""
	}
	tag := values[0]
	if len(tag) < 2 || tag[0] != '"' || tag[len(tag)-1] != '"' {
		return ""
	}
	// RFC entity-tag: etagc is %x21 / %x23-7E / obs-text.
	for i := 1; i < len(tag)-1; i++ {
		if b := tag[i]; b < 0x21 || b == '"' || b == 0x7f {
			return ""
		}
	}
	return tag
}
