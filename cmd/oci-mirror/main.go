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

type zotMiss struct{ read readRequest }

func (*zotMiss) Error() string { return "dedicated Zot reported a local miss" }

func overlay(zot *url.URL, policy fallbackPolicy, cache *mirrorCache) http.Handler {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 30 * time.Second
	return &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(zot)
			r.Out.Host = r.In.Host
			r.Out.URL.RawQuery = r.In.URL.RawQuery
			r.SetXForwarded()
		},
		ModifyResponse: func(resp *http.Response) error {
			// -poc-zot opts into this contract: the dedicated, direct Zot has
			// no Sync, auth policy or middleware that hides denials as 404.
			if cache != nil && resp.StatusCode == http.StatusNotFound {
				if read, ok := policy.classify(resp.Request); ok {
					return &zotMiss{read}
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			var miss *zotMiss
			if errors.As(err, &miss) {
				cache.serve(w, r, miss.read)
				return
			}
			log.Printf("zot transport failed: %v", err)
			registryError(w, http.StatusBadGateway, "UNAVAILABLE", "Zot transport failed")
		},
	}
}

func run() error {
	listen := flag.String("listen", "127.0.0.1:15000", "loopback listen address")
	zotURL := flag.String("poc-zot", "", "direct loopback local-only Zot HTTP origin (required)")
	enableFallback := flag.Bool("poc-enable-fallback", false, "enable anonymous upstream reads after a trusted local Zot 404")
	cacheDir := flag.String("cache-dir", "", "directory for verified Mirror blobs (required when fallback is enabled)")
	policy := fallbackPolicy{
		repositories: make(map[string]struct{}),
		allowed:      make(map[string]struct{}),
		denied:       make(map[string]struct{}),
	}
	flag.Func("poc-public-repo", "restrict fallback to this exact downstream repository, including short names (repeatable)", func(repo string) error {
		if _, _, err := resolveRepository(repo); err != nil {
			return err
		}
		policy.repositories[repo] = struct{}{}
		return nil
	})
	flag.Func("poc-allow-registry", "allow fallback for this logical registry hostname (repeatable; empty allows any)", registryFlag(policy.allowed))
	flag.Func("poc-deny-registry", "deny fallback for this logical registry hostname (repeatable; takes precedence)", registryFlag(policy.denied))
	privateDNSHosts := make(map[string]struct{})
	flag.Func("poc-private-dns-host", "exact DNS hostname allowed to resolve to RFC 1918 or IPv6 ULA addresses (repeatable)", func(host string) error {
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
	zot, err := url.Parse(*zotURL)
	if err != nil || zot.Scheme != "http" || zot.User != nil || (zot.Path != "" && zot.Path != "/") || zot.RawQuery != "" || zot.ForceQuery || zot.Fragment != "" {
		return errors.New("-poc-zot must be a direct loopback HTTP origin")
	}
	if addr, err := netip.ParseAddrPort(zot.Host); err != nil || !addr.Addr().IsLoopback() {
		return errors.New("-poc-zot must use a numeric loopback address with port")
	}
	zot.Path, zot.RawPath = "", ""
	if zot.Host == *listen {
		return errors.New("-poc-zot and -listen must not use the same address")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var cache *mirrorCache
	if !*enableFallback && (len(policy.repositories)+len(policy.allowed)+len(policy.denied)+len(privateDNSHosts) != 0) {
		return errors.New("upstream policy options require -poc-enable-fallback")
	}
	if *enableFallback {
		if *cacheDir == "" {
			return errors.New("-cache-dir is required with -poc-enable-fallback")
		}
		cache, err = newMirrorCache(ctx, *cacheDir, newPublicTransport(privateDNSHosts))
		if err != nil {
			return fmt.Errorf("open cache: %w", err)
		}
		defer os.RemoveAll(cache.staging)
	}

	server := &http.Server{Addr: *listen, Handler: overlay(zot, policy, cache), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	log.Printf("mirror listen=%s zot=%s upstream_fallback=%t", *listen, zot.Host, *enableFallback)
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	}
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
