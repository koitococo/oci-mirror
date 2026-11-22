package main

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type upstreamAuthRoundTripper func(*http.Request) (*http.Response, error)

func (f upstreamAuthRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestUpstreamAuth(t *testing.T) {
	const (
		legacyAuth = `{"auths":{"https://index.docker.io/v1/":{"auth":"dXNlcjpwYXNz"}}}`
		oauthAuth  = `{"auths":{"reg.example":{"identitytoken":"fixture-refresh"}}}`
		hub        = "https://registry-1.docker.io"
		hubToken   = "https://auth.docker.io/token"
		hubPull    = hub + "/v2/library/alpine/manifests/latest"
		private    = "https://reg.example"
		realm      = "https://auth.example/token"
		basic      = "Basic dXNlcjpwYXNz"
		scope      = "repository:library/alpine:pull"
	)
	type step struct {
		method, url, auth, scope string
		status                   int
		headers                  http.Header
		body                     string
	}
	challenge := func(host, tokenURL string) step {
		return step{"GET", host + "/v2/", "", "", 401, http.Header{
			"Www-Authenticate": {`Bearer realm="` + tokenURL + `",service="registry"`},
		}, ""}
	}
	for _, tc := range []struct {
		name, repo, authfile string
		realms               map[string]string
		steps                []step
		warning              string
	}{
		{
			name: "Docker Hub legacy credential", repo: "docker.io/library/alpine", authfile: legacyAuth,
			steps: []step{
				challenge(hub, hubToken),
				{"GET", hubToken, basic, scope, 200, nil, `{"token":"private"}`},
				{"GET", hubPull, "Bearer private", "", 200, nil, "manifest"},
			},
		},
		{
			name: "token rejection falls back", repo: "docker.io/library/alpine", authfile: legacyAuth,
			warning: "credentials_rejected",
			steps: []step{
				challenge(hub, hubToken),
				{"GET", hubToken, basic, scope, 401, nil, ""},
				challenge(hub, hubToken),
				{"GET", hubToken, "", scope, 200, nil, `{"token":"public"}`},
				{"GET", hubPull, "Bearer public", "", 200, nil, "manifest"},
			},
		},
		{
			name: "resource challenge refreshes before falling back", repo: "docker.io/library/alpine", authfile: legacyAuth,
			warning: "credentials_rejected",
			steps: []step{
				challenge(hub, hubToken),
				{"GET", hubToken, basic, scope, 200, nil, `{"token":"expired"}`},
				{"GET", hubPull, "Bearer expired", "", 401, challenge(hub, hubToken).headers, ""},
				challenge(hub, hubToken),
				{"GET", hubToken, basic, scope, 200, nil, `{"token":"denied"}`},
				{"GET", hubPull, "Bearer denied", "", 403, nil, ""},
				challenge(hub, hubToken),
				{"GET", hubToken, "", scope, 200, nil, `{"token":"public"}`},
				{"GET", hubPull, "Bearer public", "", 200, nil, "manifest"},
			},
		},
		{
			name: "untrusted redirect cannot receive OAuth body", repo: "reg.example/team/image", authfile: oauthAuth,
			warning: "untrusted_token_host",
			realms:  map[string]string{"reg.example": "auth.example"},
			steps: []step{
				challenge(private, realm),
				{"POST", realm, "", "repository:team/image:pull", 307, http.Header{
					"Location": {"https://untrusted.example/token"},
				}, ""},
				// The only allowed next network request is the anonymous handshake's ping.
				challenge(private, realm),
				{"GET", realm, "", "repository:team/image:pull", 200, nil, `{"token":"public"}`},
				{"GET", private + "/v2/team/image/manifests/latest", "Bearer public", "", 200, nil, "manifest"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			t.Cleanup(func() { slog.SetDefault(previous) })

			path := filepath.Join(t.TempDir(), "auth.json")
			if err := os.WriteFile(path, []byte(tc.authfile), 0o600); err != nil {
				t.Fatal(err)
			}
			resolved, err := resolveRepository(tc.repo)
			if err != nil {
				t.Fatal(err)
			}
			next := 0
			base := upstreamAuthRoundTripper(func(req *http.Request) (*http.Response, error) {
				if req.Body != nil {
					defer req.Body.Close()
				}
				url := req.URL.Scheme + "://" + req.URL.Host + req.URL.Path
				if next == len(tc.steps) {
					return nil, fmt.Errorf("unexpected %s %s after end of script", req.Method, url)
				}
				want := tc.steps[next]
				next++
				if req.Method != want.method || url != want.url || req.Header.Get("Authorization") != want.auth {
					return nil, fmt.Errorf("step %d: got %s %s, want %s %s (authorization matches: %t)",
						next, req.Method, url, want.method, want.url, req.Header.Get("Authorization") == want.auth)
				}
				scope := req.URL.Query().Get("scope")
				if req.Method == http.MethodPost {
					if err := req.ParseForm(); err != nil {
						return nil, err
					}
					if req.PostForm.Get("grant_type") != "refresh_token" || req.PostForm.Get("refresh_token") != "fixture-refresh" {
						return nil, fmt.Errorf("step %d: missing expected OAuth refresh credentials", next)
					}
					scope = req.PostForm.Get("scope")
				}
				if scope != want.scope {
					return nil, fmt.Errorf("step %d: scope %q, want %q", next, scope, want.scope)
				}
				return &http.Response{StatusCode: want.status, Header: want.headers.Clone(), Body: io.NopCloser(strings.NewReader(want.body)), Request: req}, nil
			})
			ctx := t.Context()
			pool, err := newUpstreamPool(ctx, base, upstreamLimits{registries: 4, scopes: 4, burst: 20, requestsPerSecond: 100}, upstreamAuth{file: loadUpstreamAuthFile(path), realms: tc.realms})
			if err != nil {
				t.Fatal(err)
			}
			client, release, err := pool.acquire(ctx, resolved.source)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			resp, err := getUpstream(ctx, client, resolved.source, "manifests", "latest", "")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK || err != nil || string(body) != "manifest" {
				t.Fatalf("pull got status %d, body %q, error %v", resp.StatusCode, body, err)
			}
			if next != len(tc.steps) {
				t.Fatalf("observed %d requests, want %d", next, len(tc.steps))
			}
			logged := logs.String()
			if tc.warning == "" {
				if strings.Contains(logged, "upstream_auth_anonymous_fallback") {
					t.Error("unexpected anonymous fallback warning")
				}
			} else if !strings.Contains(logged, "level=WARN msg=upstream_auth_anonymous_fallback") ||
				!strings.Contains(logged, "reason="+tc.warning) {
				t.Errorf("missing anonymous fallback warning with reason %q", tc.warning)
			}
			for _, secret := range []string{"fixture-refresh", "dXNlcjpwYXNz", "user:pass"} {
				if strings.Contains(logged, secret) {
					t.Error("upstream auth log contains fixture credentials")
				}
			}
		})
	}
}
