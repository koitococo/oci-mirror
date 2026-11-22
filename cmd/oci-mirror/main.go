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

	"github.com/google/go-containerregistry/pkg/name"
)

var (
	tagPattern    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	errZotMiss    = errors.New("dedicated Zot reported a local miss")
)

type readRequest struct {
	kind, reference string
}

func fallbackRead(r *http.Request, repo string) (readRequest, bool) {
	if repo == "" || r.URL.RawQuery != "" || r.URL.EscapedPath() != r.URL.Path {
		return readRequest{}, false
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/v2/"+repo+"/")
	if !ok {
		return readRequest{}, false
	}
	kind, ref, _ := strings.Cut(path, "/")
	valid := false
	switch kind {
	case "manifests":
		valid = (r.Method == http.MethodHead || r.Method == http.MethodGet) &&
			(tagPattern.MatchString(ref) || digestPattern.MatchString(ref))
	case "blobs":
		valid = r.Method == http.MethodGet && digestPattern.MatchString(ref)
	}
	return readRequest{kind, ref}, valid
}

func overlay(zot *url.URL, repo string, cache *mirrorCache) http.Handler {
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
			if resp.StatusCode == http.StatusNotFound {
				if _, ok := fallbackRead(resp.Request, repo); ok {
					return errZotMiss
				}
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if errors.Is(err, errZotMiss) {
				read, _ := fallbackRead(r, repo)
				cache.serve(w, r, read)
				return
			}
			log.Printf("zot transport failed: %v", err)
			registryError(w, http.StatusBadGateway, "UNAVAILABLE", "Zot transport failed")
		},
	}
}

func run() error {
	listen := flag.String("listen", "127.0.0.1:15000", "loopback listen address")
	zotURL := flag.String("poc-zot", "", "direct loopback Zot HTTP origin with no auth/accessControl/Sync (required)")
	publicRepo := flag.String("poc-public-repo", "", "one exact host/repository eligible for anonymous fallback (empty disables fallback)")
	cacheDir := flag.String("cache-dir", "", "directory for verified Mirror blobs (required when fallback is enabled)")
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
	if *publicRepo != "" {
		host, _, ok := strings.Cut(*publicRepo, "/")
		repo, err := name.NewRepository(*publicRepo, name.StrictValidation)
		if !ok || err != nil || repo.Name() != *publicRepo {
			return errors.New("-poc-public-repo must be a canonical host/repository without port or tag")
		}
		if err := checkUpstreamHost(host); err != nil {
			return fmt.Errorf("-poc-public-repo: %w", err)
		}
		if *cacheDir == "" {
			return errors.New("-cache-dir is required with -poc-public-repo")
		}
		cache, err = newMirrorCache(ctx, *cacheDir, publicSource{repo: repo, transport: newPublicTransport(privateDNSHosts)})
		if err != nil {
			return fmt.Errorf("open cache: %w", err)
		}
		defer os.RemoveAll(cache.staging)
	}

	server := &http.Server{Addr: *listen, Handler: overlay(zot, *publicRepo, cache), ReadHeaderTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.ListenAndServe() }()
	log.Printf("mirror listen=%s zot=%s fallback_repository=%q", *listen, zot.Host, *publicRepo)
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
