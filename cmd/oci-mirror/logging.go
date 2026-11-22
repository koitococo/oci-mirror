package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"syscall"

	registry "github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"modernc.org/sqlite"
)

const requestIDHeader = "X-Request-ID"

type requestLogKey struct{}

type requestLog struct {
	id    string
	cache string
}

func newLogID() string { return rand.Text() }

func setRequestCache(ctx context.Context, result string) {
	if entry, _ := ctx.Value(requestLogKey{}).(*requestLog); entry != nil {
		entry.cache = result
	}
}

func requestID(ctx context.Context) string {
	if entry, _ := ctx.Value(requestLogKey{}).(*requestLog); entry != nil {
		return entry.id
	}
	return ""
}

type diagnosticError struct {
	stage, reason  string
	upstreamStatus int
	err            error
	fields         []any
}

func (e *diagnosticError) Error() string { return e.err.Error() }
func (e *diagnosticError) Unwrap() error { return e.err }

func diagnoseFailure(stage, reason string, err error, fields ...any) error {
	return &diagnosticError{stage: stage, reason: reason, err: err, fields: fields}
}

func diagnoseAt(stage string, err error) error {
	var diagnostic *diagnosticError
	if err == nil || errors.As(err, &diagnostic) {
		return err
	}
	return diagnoseFailure(stage, "", err)
}

func diagnosticDetails(err error, stage string) (string, string) {
	var diagnostic *diagnosticError
	if errors.As(err, &diagnostic) {
		stage = diagnostic.stage
		if diagnostic.reason != "" {
			return stage, diagnostic.reason
		}
	}
	return stage, logErrorReason(err)
}

func quotaDimension(err error) string {
	var limit *quotaLimitError
	if errors.As(err, &limit) {
		return limit.dimension
	}
	return ""
}

// Diagnostic status is the observed origin status, not a status synthesized
// for a client when an otherwise successful upstream response is rejected.
func diagnosticUpstreamStatus(err error) int {
	var diagnostic *diagnosticError
	if errors.As(err, &diagnostic) && diagnostic.upstreamStatus != 0 {
		return diagnostic.upstreamStatus
	}
	return fetchUpstreamStatus(err)
}

// Only validated resource identifiers and selected numeric/header-derived
// fields belong here; never attach raw errors, URLs, or response bodies.
func appendDiagnosticFields(attrs []any, err error) []any {
	var diagnostic *diagnosticError
	if errors.As(err, &diagnostic) {
		attrs = append(attrs, diagnostic.fields...)
	}
	var remote *registry.Error
	var transport *url.Error
	switch {
	case errors.As(err, &remote):
		attrs = appendUpstreamHost(attrs, remote.Request)
	case errors.As(err, &transport):
		if u, parseErr := url.Parse(transport.URL); parseErr == nil {
			attrs = appendHost(attrs, u)
		}
	}
	var local *sqlite.Error
	if errors.As(err, &local) {
		attrs = append(attrs, "sqlite_code", local.Code())
	}
	return attrs
}

func appendUpstreamHost(attrs []any, req *http.Request) []any {
	if req == nil {
		return attrs
	}
	return appendHost(attrs, req.URL)
}

func appendHost(attrs []any, u *url.URL) []any {
	if u != nil {
		host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
		if ip, err := netip.ParseAddr(host); err == nil && ip.Zone() == "" {
			return append(attrs, "upstream_host", ip.String())
		}
		if registryDNS(host) {
			return append(attrs, "upstream_host", host)
		}
	}
	return attrs
}

// Never derive log fields from err.Error: network errors can contain URLs or
// response data, and local errors can contain filesystem paths.
func logErrorReason(err error) string {
	var dns *net.DNSError
	var cert *tls.CertificateVerificationError
	var network net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		return "unexpected_eof"
	case errors.Is(err, io.ErrShortWrite):
		return "short_write"
	case errors.Is(err, errBusy):
		return "busy"
	case errors.Is(err, errCacheFull):
		return "cache_quota_exhausted"
	case errors.Is(err, errPublicURL):
		return "egress_invalid_url"
	case errors.Is(err, errPublicIP):
		return "egress_nonpublic_ip"
	case errors.Is(err, errPrivateDNS):
		return "egress_private_dns"
	case errors.As(err, &dns):
		return "dns_failed"
	case errors.As(err, &cert):
		return "tls_verification_failed"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return "connection_closed"
	case errors.Is(err, os.ErrPermission):
		return "permission_denied"
	case errors.Is(err, os.ErrNotExist):
		return "not_found"
	case errors.Is(err, syscall.ENOSPC):
		return "no_space"
	case errors.Is(err, syscall.EIO):
		return "device_io_error"
	case errors.As(err, &network) && network.Timeout():
		return "timeout"
	case fetchUpstreamStatus(err) != 0:
		return "upstream_http_status"
	case errors.Is(err, errCacheStorage):
		return "local_cache_error"
	default:
		return "io_error"
	}
}
