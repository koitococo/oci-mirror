package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"os"
	"syscall"
)

const requestIDHeader = "X-Request-ID"

type logRequestIDKey struct{}

func newLogID() string { return rand.Text() }

func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, logRequestIDKey{}, id)
}

func requestID(ctx context.Context) string {
	id, _ := ctx.Value(logRequestIDKey{}).(string)
	return id
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
	case errors.Is(err, errPublicURL), errors.Is(err, errPublicIP), errors.Is(err, errPrivateDNS):
		return "egress_denied"
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
	case errors.As(err, &network) && network.Timeout():
		return "timeout"
	case errors.Is(err, errCacheStorage):
		return "local_cache_error"
	default:
		return "io_error"
	}
}
