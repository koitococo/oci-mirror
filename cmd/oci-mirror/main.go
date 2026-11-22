package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"
)

var (
	tagPattern    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

type readRequest struct {
	kind, reference string
	repository      resolvedRepository
}

type backendMiss struct{ read readRequest }

func (*backendMiss) Error() string { return "backend reported a local miss" }

func newBackendTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 30 * time.Second
	return transport
}

func overlay(backend *url.URL, policy fallbackPolicy, cache *mirrorCache, transport http.RoundTripper) http.Handler {
	return &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(backend)
			r.Out.Host = r.In.Host
			r.Out.URL.RawQuery = r.In.URL.RawQuery
			r.SetXForwarded()
		},
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode != http.StatusNotFound || resp.Request == nil {
				return nil
			}
			read, ok := policy.classify(resp.Request)
			if !ok {
				return nil
			}
			if trustedBackendMiss(resp, read, transport) {
				return &backendMiss{read}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			var miss *backendMiss
			if errors.As(err, &miss) {
				cache.serve(w, r, miss.read)
				return
			}
			log.Printf("backend transport failed: %v", err)
			registryError(w, http.StatusBadGateway, "UNAVAILABLE", "registry backend transport failed")
		},
	}
}

func run() error {
	listen := flag.String("listen", "127.0.0.1:15000", "loopback listen address")
	backendURL := flag.String("backend", "", "local-only Registry V2 HTTP or HTTPS origin (required)")
	cacheDir := flag.String("cache-dir", "", "directory for the persistent Mirror cache (required)")
	limits := cacheLimitFlags()
	upstreamLimit := upstreamLimitFlags()
	policy := fallbackPolicy{
		allowed: make(map[string]struct{}),
		denied:  make(map[string]struct{}),
	}
	flag.Func("allow-registry", "allow fallback for this logical registry hostname (repeatable; empty allows all non-denied registries)", registryFlag(policy.allowed))
	flag.Func("deny-registry", "deny fallback for this logical registry hostname (repeatable; takes precedence)", registryFlag(policy.denied))
	privateDNSHosts := make(map[string]struct{})
	flag.Func("private-dns-host", "exact upstream DNS hostname allowed to resolve to RFC 1918 or IPv6 ULA addresses (repeatable)", func(host string) error {
		if _, err := netip.ParseAddr(host); err == nil {
			return errors.New("must name a DNS host, not an IP literal")
		}
		if err := checkUpstreamHost(host); err != nil {
			return err
		}
		privateDNSHosts[strings.ToLower(host)] = struct{}{}
		return nil
	})
	flag.Parse()

	if addr, err := netip.ParseAddrPort(*listen); err != nil || !addr.Addr().IsLoopback() {
		return errors.New("-listen must be a numeric loopback address with port")
	}
	backend, err := url.Parse(*backendURL)
	if err != nil || (backend.Scheme != "http" && backend.Scheme != "https") || backend.Hostname() == "" ||
		backend.User != nil || (backend.Path != "" && backend.Path != "/") ||
		backend.RawQuery != "" || backend.ForceQuery || backend.Fragment != "" {
		return errors.New("-backend must be a Registry V2 HTTP or HTTPS origin without credentials, path, query or fragment")
	}
	backend.Path, backend.RawPath = "", ""
	if backend.Host == *listen {
		return errors.New("-backend and -listen must not use the same address")
	}
	if *cacheDir == "" {
		return errors.New("-cache-dir is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	transport := newBackendTransport()
	defer transport.CloseIdleConnections()
	cache, err := newMirrorCache(ctx, *cacheDir, newPublicTransport(privateDNSHosts), *limits, *upstreamLimit)
	if err != nil {
		return fmt.Errorf("open cache: %w", err)
	}
	defer func() {
		if err := cache.close(); err != nil {
			log.Printf("close cache: %v", err)
		}
	}()

	server := &http.Server{Addr: *listen, Handler: overlay(backend, policy, cache, transport), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	log.Printf("mirror listen=%s backend=%s", *listen, backend.Host)
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
