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

type claimsKey struct{}

func newZotTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 30 * time.Second
	return transport
}

func overlay(zot *url.URL, policy fallbackPolicy, cache *mirrorCache, auth *bearerAuth, transport *http.Transport) http.Handler {
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(zot)
			r.Out.Host = r.In.Host
			r.Out.URL.RawQuery = r.In.URL.RawQuery
			r.SetXForwarded()
		},
		ModifyResponse: func(resp *http.Response) error {
			if cache == nil || resp.StatusCode != http.StatusNotFound {
				return nil
			}
			claims, ok := resp.Request.Context().Value(claimsKey{}).(*registryClaims)
			if !ok {
				return nil
			}
			if read, ok := policy.classify(resp.Request); ok &&
				claims.canPull(read.repository.downstream) && trustedZotMiss(resp, read, transport) {
				return &zotMiss{read}
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
	if auth == nil {
		return proxy
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (r.URL.Path == "/v2" || strings.HasPrefix(r.URL.Path, "/v2/")) && r.Method != http.MethodOptions {
			claims, err := auth.authenticate(r)
			if err != nil {
				auth.writeChallenge(w, r)
				return
			}
			if cache != nil && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
				r = r.WithContext(context.WithValue(r.Context(), claimsKey{}, claims))
			}
		}
		proxy.ServeHTTP(w, r)
	})
}

func run() error {
	listen := flag.String("listen", "127.0.0.1:15000", "loopback listen address")
	zotURL := flag.String("poc-zot", "", "direct loopback local-only Zot HTTP origin (required)")
	zotConfig := flag.String("zot-config", "", "same immutable JSON config used by a dedicated local-only Zot")
	jwtIssuer := flag.String("jwt-issuer", "", "expected issuer of the Zot Docker Registry Bearer JWT")
	enableFallback := flag.Bool("poc-enable-fallback", false, "enable anonymous upstream reads after a trusted local Zot 404")
	cacheDir := flag.String("cache-dir", "", "directory for the persistent Mirror cache (required when fallback is enabled)")
	limits := cacheLimitFlags()
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
	if (*zotConfig == "") != (*jwtIssuer == "") {
		return errors.New("-zot-config and -jwt-issuer must be supplied together")
	}
	transport := newZotTransport()
	var auth *bearerAuth
	if *zotConfig != "" {
		auth, err = loadZotAuth(*zotConfig, *jwtIssuer)
		if err != nil {
			return err
		}
		probe, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = verifyZot(probe, zot, auth, transport)
		cancel()
		if err != nil {
			return err
		}
	}
	var cache *mirrorCache
	if !*enableFallback && (len(policy.repositories)+len(policy.allowed)+len(policy.denied)+len(privateDNSHosts) != 0) {
		return errors.New("upstream policy options require -poc-enable-fallback")
	}
	if *enableFallback {
		if auth == nil {
			return errors.New("-poc-enable-fallback requires -zot-config and -jwt-issuer")
		}
		if *cacheDir == "" {
			return errors.New("-cache-dir is required with -poc-enable-fallback")
		}
		cache, err = newMirrorCache(ctx, *cacheDir, newPublicTransport(privateDNSHosts), *limits)
		if err != nil {
			return fmt.Errorf("open cache: %w", err)
		}
		defer func() {
			if err := cache.close(); err != nil {
				log.Printf("close cache: %v", err)
			}
		}()
	}

	server := &http.Server{Addr: *listen, Handler: overlay(zot, policy, cache, auth, transport), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	log.Printf("mirror listen=%s zot=%s upstream_fallback=%t", *listen, zot.Host, *enableFallback)
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
