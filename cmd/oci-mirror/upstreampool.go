package main

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	registry "github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"golang.org/x/time/rate"
)

type upstreamLimits struct {
	registries, scopes, burst int
	requestsPerSecond         float64
}

type upstreamAuth struct {
	file   upstreamAuthFile
	realms map[string]string
}

type upstreamPool struct {
	ctx    context.Context
	base   http.RoundTripper
	limits upstreamLimits
	auth   upstreamAuth

	mu         sync.Mutex
	registries map[string]*list.Element
	lru        list.List
}

type upstreamRegistry struct {
	url     string
	limiter *rate.Limiter
	active  int
	pending int
	scopes  map[string]*list.Element
	lru     list.List
}

type upstreamScope struct {
	scope      string
	credential authn.Authenticator
	active     int
	client     *http.Client
	auth       *upstreamHandshake // last successful handshake
	flight     *upstreamHandshake // initialization or refresh in progress
}

type upstreamHandshake struct {
	ready         chan struct{}
	transport     http.RoundTripper
	authenticated bool
	err           error
}

func newUpstreamPool(ctx context.Context, base http.RoundTripper, limits upstreamLimits, auth upstreamAuth) (*upstreamPool, error) {
	// rate.Inf is a sentinel (not IEEE infinity), so exclude it too.
	if limits.registries <= 0 || limits.scopes <= 0 || limits.burst <= 0 ||
		!(limits.requestsPerSecond > 0 && limits.requestsPerSecond < float64(rate.Inf)) {
		return nil, fmt.Errorf("upstream limits must be finite and positive")
	}
	return &upstreamPool{ctx: ctx, base: base, limits: limits, auth: auth, registries: make(map[string]*list.Element)}, nil
}

func (p *upstreamPool) acquire(ctx context.Context, repo name.Repository) (*http.Client, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if err := p.ctx.Err(); err != nil {
		return nil, nil, err
	}
	r, s, handshake, err := p.lease(repo)
	if err != nil {
		return nil, nil, err
	}
	if err := p.wait(ctx, handshake); err != nil {
		p.release(r, s)
		return nil, nil, err
	}
	return s.client, func() { p.release(r, s) }, nil
}

func (p *upstreamPool) wait(ctx context.Context, handshake *upstreamHandshake) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.ctx.Done():
		return p.ctx.Err()
	case <-handshake.ready:
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := p.ctx.Err(); err != nil {
			return err
		}
		return handshake.err
	}
}

func (p *upstreamPool) lease(repo name.Repository) (*upstreamRegistry, *upstreamScope, *upstreamHandshake, error) {
	registryURL := repo.Registry.Scheme() + "://" + strings.ToLower(repo.RegistryStr())
	scope := repo.Scope("pull")
	p.mu.Lock()
	defer p.mu.Unlock()

	entry := p.registries[registryURL]
	if entry == nil {
		if len(p.registries) == p.limits.registries && !p.evictRegistry() {
			return nil, nil, nil, fmt.Errorf("upstream registry context capacity exhausted: %w", quotaLimit(errBusy, "upstream-registries"))
		}
		r := &upstreamRegistry{
			url:     registryURL,
			limiter: rate.NewLimiter(rate.Limit(p.limits.requestsPerSecond), p.limits.burst),
			scopes:  make(map[string]*list.Element),
		}
		entry = p.lru.PushFront(r)
		p.registries[registryURL] = entry
	}
	r := entry.Value.(*upstreamRegistry)
	e := r.scopes[scope]
	if e == nil {
		if len(r.scopes) == p.limits.scopes && !r.evictScope() {
			return nil, nil, nil, fmt.Errorf("upstream pull scope context capacity exhausted: %w", quotaLimit(errBusy, "upstream-scopes-per-registry"))
		}
		s := &upstreamScope{scope: scope, credential: p.auth.file.resolve(repo)}
		s.client = &http.Client{
			Transport:     upstreamResourceTransport{pool: p, reg: repo.Registry, registry: r, scope: s},
			CheckRedirect: upstreamRedirect,
		}
		e = r.lru.PushFront(s)
		r.scopes[scope] = e
		p.startHandshake(repo.Registry, r, s, 0)
	}
	s := e.Value.(*upstreamScope)
	r.active++
	s.active++
	p.lru.MoveToFront(entry)
	r.lru.MoveToFront(e)
	handshake := s.auth
	if handshake == nil {
		handshake = s.flight
	}
	return r, s, handshake, nil
}

func (p *upstreamPool) release(r *upstreamRegistry, s *upstreamScope) {
	p.mu.Lock()
	r.active--
	s.active--
	p.mu.Unlock()
}

func (p *upstreamPool) evictRegistry() bool {
	for e := p.lru.Back(); e != nil; e = e.Prev() {
		r := e.Value.(*upstreamRegistry)
		// Recreating a partially depleted bucket would give this origin a new burst.
		if r.active != 0 || r.pending != 0 || r.limiter.Tokens() < float64(p.limits.burst) {
			continue
		}
		p.lru.Remove(e)
		delete(p.registries, r.url)
		return true
	}
	return false
}

func (r *upstreamRegistry) evictScope() bool {
	for e := r.lru.Back(); e != nil; e = e.Prev() {
		s := e.Value.(*upstreamScope)
		if s.active != 0 || s.flight != nil {
			continue
		}
		r.lru.Remove(e)
		delete(r.scopes, s.scope)
		return true
	}
	return false
}

const upstreamAuthRefreshStage = "upstream_auth_refresh"

// startHandshake requires p.mu. The same flight serves all waiters for this scope.
func (p *upstreamPool) startHandshake(reg name.Registry, r *upstreamRegistry, s *upstreamScope, rejected int) *upstreamHandshake {
	handshake := &upstreamHandshake{ready: make(chan struct{})}
	credential := s.credential
	if rejected != 0 || (s.auth != nil && !s.auth.authenticated) {
		credential = authn.Anonymous
	}
	s.flight = handshake
	r.pending++
	stage := "upstream_auth_initial"
	if s.auth != nil {
		stage = upstreamAuthRefreshStage
	}
	go p.handshake(reg, r, s, handshake, stage, credential, rejected)
	return handshake
}

func (p *upstreamPool) handshake(reg name.Registry, r *upstreamRegistry, s *upstreamScope, handshake *upstreamHandshake, stage string, credential authn.Authenticator, rejected int) {
	// A canceled waiter must not cancel a shared handshake.
	ctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
	defer cancel()
	base := upstreamRateTransport{base: p.base, limiter: r.limiter}
	var transport http.RoundTripper = base
	if credential != authn.Anonymous {
		host := reg.RegistryStr()
		transport = upstreamCredentialTransport{base: base, host: host, realm: p.auth.realms[logicalRegistry(host)]}
	}
	if rejected != 0 {
		warnAnonymousUpstream(ctx, reg, s, stage, "credentials_rejected", rejected)
	}
	scopes := []string{s.scope}
	handshake.transport, handshake.err = registry.NewWithContext(ctx, reg, credential, transport, scopes)
	if credential != authn.Anonymous {
		status := fetchUpstreamStatus(handshake.err)
		reason := ""
		switch {
		case errors.Is(handshake.err, errUntrustedAuthHost):
			reason = "untrusted_token_host"
		case upstreamAuthRejected(status), status == http.StatusBadRequest:
			// OAuth token endpoints can reject expired identity tokens with 400.
			reason = "credentials_rejected"
		}
		if reason != "" {
			warnAnonymousUpstream(ctx, reg, s, stage, reason, status)
			handshake.transport, handshake.err = registry.NewWithContext(ctx, reg, authn.Anonymous, base, scopes)
		} else {
			handshake.authenticated = handshake.err == nil
		}
	}
	handshake.err = diagnoseAt(stage, handshake.err)

	p.mu.Lock()
	defer p.mu.Unlock()
	r.pending--
	s.flight = nil
	if handshake.err == nil {
		s.auth = handshake
	} else if s.auth == nil {
		// A pre-reload handshake must not remove a replacement scope.
		if e := r.scopes[s.scope]; e != nil && e.Value == s {
			r.lru.Remove(e)
			delete(r.scopes, s.scope)
		}
	}
	close(handshake.ready)
}

func upstreamAuthRejected(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden
}

func warnAnonymousUpstream(ctx context.Context, reg name.Registry, s *upstreamScope, stage, reason string, status int) {
	slog.WarnContext(ctx, "upstream_auth_anonymous_fallback", "upstream_registry", reg.RegistryStr(),
		"scope", s.scope, "stage", stage, "reason", reason, "upstream_status", status)
}

var errUntrustedAuthHost = errors.New("upstream authentication destination is not trusted")

type upstreamCredentialTransport struct {
	base        http.RoundTripper
	host, realm string
}

func (t upstreamCredentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Hostname()
	if req.Context().Value(upstreamResourceChallengeKey{}) == nil {
		// Apply this to every handshake hop, including redirects replaying an OAuth POST body.
		if !strings.EqualFold(host, t.host) && !strings.EqualFold(host, t.realm) &&
			!(t.host == dockerHubEndpoint && strings.EqualFold(host, "auth.docker.io")) {
			return nil, errUntrustedAuthHost
		}
	} else if !strings.EqualFold(host, t.host) {
		req.Header.Del("Authorization")
	}
	return t.base.RoundTrip(req)
}

type upstreamResourceChallengeKey struct{}

type upstreamResourceTransport struct {
	pool     *upstreamPool
	reg      name.Registry
	registry *upstreamRegistry
	scope    *upstreamScope
}

func (t upstreamResourceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.pool.mu.Lock()
	handshake := t.scope.auth
	if t.scope.flight != nil {
		handshake = t.scope.flight
	}
	t.pool.mu.Unlock()
	if err := t.pool.wait(req.Context(), handshake); err != nil {
		return nil, err
	}

	resp, challenged, err := sendUpstreamResource(req, handshake)
	if err != nil || !strings.EqualFold(req.URL.Host, t.reg.RegistryStr()) {
		return resp, err
	}
	if challenged {
		resp.Body.Close()
		handshake, err = t.retryHandshake(req.Context(), handshake, 0)
		if err != nil {
			return nil, diagnoseAt(upstreamAuthRefreshStage, err)
		}
		resp, _, err = sendUpstreamResource(req, handshake)
	}
	if err != nil || !handshake.authenticated || !upstreamAuthRejected(resp.StatusCode) {
		return resp, err
	}
	resp.Body.Close()
	handshake, err = t.retryHandshake(req.Context(), handshake, resp.StatusCode)
	if err != nil {
		return nil, diagnoseAt(upstreamAuthRefreshStage, err)
	}
	resp, _, err = sendUpstreamResource(req, handshake)
	return resp, err
}

func (t upstreamResourceTransport) retryHandshake(ctx context.Context, previous *upstreamHandshake, rejected int) (*upstreamHandshake, error) {
	for {
		t.pool.mu.Lock()
		next := t.scope.flight
		if next == nil {
			next = t.scope.auth
			if (rejected == 0 && next == previous) || (rejected != 0 && next.authenticated) {
				next = t.pool.startHandshake(t.reg, t.registry, t.scope, rejected)
			}
		}
		t.pool.mu.Unlock()
		if err := t.pool.wait(ctx, next); err != nil {
			return nil, err
		}
		if rejected == 0 || !next.authenticated {
			return next, nil
		}
	}
}

func sendUpstreamResource(req *http.Request, handshake *upstreamHandshake) (*http.Response, bool, error) {
	challenged := false
	ctx := context.WithValue(req.Context(), upstreamResourceChallengeKey{}, &challenged)
	// The library mutates the request URL and headers; a retry must start
	// with the original request, not the previous transport's credentials.
	resp, err := handshake.transport.RoundTrip(req.Clone(ctx))
	return resp, challenged, err
}

type upstreamRateTransport struct {
	base    http.RoundTripper
	limiter *rate.Limiter
}

func (t upstreamRateTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.limiter.Wait(req.Context()); err != nil {
		if req.Context().Err() == nil {
			return nil, diagnoseFailure("upstream_rate_wait", "rate_wait_rejected", err)
		}
		return nil, diagnoseAt("upstream_rate_wait", err)
	}
	resp, err := t.base.RoundTrip(req)
	if err == nil && resp.StatusCode == http.StatusUnauthorized && len(resp.Header.Values("WWW-Authenticate")) != 0 {
		if challenged, ok := req.Context().Value(upstreamResourceChallengeKey{}).(*bool); ok {
			// Keep cached library transports immutable. Instead of letting a
			// resource challenge widen scope or mutate auth state, signal the
			// caller to retry through a separately initialized transport.
			*challenged = true
			resp.Header.Del("WWW-Authenticate")
		}
	}
	return resp, err
}
