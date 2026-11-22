package main

import (
	"container/list"
	"context"
	"fmt"
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

type upstreamPool struct {
	ctx    context.Context
	base   http.RoundTripper
	limits upstreamLimits

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
	scope  string
	active int
	client *http.Client
	auth   *upstreamHandshake // last successful handshake
	flight *upstreamHandshake // initialization or refresh in progress
}

type upstreamHandshake struct {
	ready     chan struct{}
	transport http.RoundTripper
	err       error
}

func newUpstreamPool(ctx context.Context, base http.RoundTripper, limits upstreamLimits) (*upstreamPool, error) {
	// rate.Inf is a sentinel (not IEEE infinity), so exclude it too.
	if limits.registries <= 0 || limits.scopes <= 0 || limits.burst <= 0 ||
		!(limits.requestsPerSecond > 0 && limits.requestsPerSecond < float64(rate.Inf)) {
		return nil, fmt.Errorf("upstream limits must be finite and positive")
	}
	return &upstreamPool{ctx: ctx, base: base, limits: limits, registries: make(map[string]*list.Element)}, nil
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
			return nil, nil, nil, fmt.Errorf("upstream registry context capacity exhausted: %w", errBusy)
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
			return nil, nil, nil, fmt.Errorf("upstream pull scope context capacity exhausted: %w", errBusy)
		}
		s := &upstreamScope{scope: scope}
		s.client = &http.Client{
			Transport:     upstreamResourceTransport{pool: p, reg: repo.Registry, registry: r, scope: s},
			CheckRedirect: upstreamRedirect,
		}
		e = r.lru.PushFront(s)
		r.scopes[scope] = e
		p.startHandshake(repo.Registry, r, s)
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

// startHandshake requires p.mu. The same flight serves all waiters for this scope.
func (p *upstreamPool) startHandshake(reg name.Registry, r *upstreamRegistry, s *upstreamScope) *upstreamHandshake {
	handshake := &upstreamHandshake{ready: make(chan struct{})}
	s.flight = handshake
	r.pending++
	go p.handshake(reg, r, s, handshake)
	return handshake
}

func (p *upstreamPool) handshake(reg name.Registry, r *upstreamRegistry, s *upstreamScope, handshake *upstreamHandshake) {
	// A canceled waiter must not cancel a shared handshake.
	ctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
	base := upstreamRateTransport{base: p.base, limiter: r.limiter}
	handshake.transport, handshake.err = registry.NewWithContext(ctx, reg, authn.Anonymous, base, []string{s.scope})
	cancel()

	p.mu.Lock()
	defer p.mu.Unlock()
	r.pending--
	s.flight = nil
	if handshake.err == nil {
		s.auth = handshake
	} else if s.auth == nil {
		r.lru.Remove(r.scopes[s.scope])
		delete(r.scopes, s.scope)
	}
	close(handshake.ready)
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
	if err != nil || !challenged || !strings.EqualFold(req.URL.Host, t.reg.RegistryStr()) {
		return resp, err
	}
	resp.Body.Close()

	t.pool.mu.Lock()
	if t.scope.flight != nil {
		handshake = t.scope.flight
	} else if t.scope.auth == handshake {
		handshake = t.pool.startHandshake(t.reg, t.registry, t.scope)
	} else {
		handshake = t.scope.auth
	}
	t.pool.mu.Unlock()
	if err := t.pool.wait(req.Context(), handshake); err != nil {
		return nil, err
	}
	resp, _, err = sendUpstreamResource(req, handshake)
	return resp, err
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
		return nil, err
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
