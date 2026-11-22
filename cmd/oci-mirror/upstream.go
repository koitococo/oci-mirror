package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	registry "github.com/google/go-containerregistry/pkg/v1/remote/transport"
)

type publicSource struct {
	repo      name.Repository
	transport http.RoundTripper
}

func (s publicSource) get(ctx context.Context, kind, ref, accept string) (*http.Response, error) {
	transport, err := registry.NewWithContext(ctx, s.repo.Registry, authn.Anonymous, s.transport, []string{s.repo.Scope("pull")})
	if err != nil {
		return nil, err
	}
	client := http.Client{Transport: transport, CheckRedirect: func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many upstream redirects")
		}
		if !strings.EqualFold(next.URL.Host, via[0].URL.Host) {
			next.Header.Del("Authorization")
		}
		next.Header.Del("Referer")
		return nil
	}}
	u := url.URL{Scheme: "https", Host: s.repo.RegistryStr(), Path: "/v2/" + s.repo.RepositoryStr() + "/" + kind + "/" + ref}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, &upstreamStatus{resp.StatusCode}
	}
	return resp, nil
}

type upstreamStatus struct{ status int }

func (e *upstreamStatus) Error() string { return fmt.Sprintf("upstream HTTP %d", e.status) }

var dnsPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)

func dnsHost(host string) bool {
	_, ipErr := netip.ParseAddr(host)
	return ipErr != nil && len(host) <= 253 && dnsPattern.MatchString(strings.ToLower(host))
}

type publicTransport struct{ transport *http.Transport }

var (
	errPublicURL = errors.New("upstream URL must use public DNS and HTTPS/443")
	errPublicIP  = errors.New("upstream DNS includes a non-public target")
)

func (p publicTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u := req.URL
	if u.Scheme != "https" || u.User != nil || !dnsHost(u.Hostname()) || (u.Port() != "" && u.Port() != "443") {
		return nil, errPublicURL
	}
	return p.transport.RoundTrip(req)
}

func newPublicTransport() http.RoundTripper {
	dialer := net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	base := http.DefaultTransport.(*http.Transport).Clone()
	base.Proxy = nil
	base.DisableCompression = true
	base.ResponseHeaderTimeout = 15 * time.Second
	base.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || port != "443" || !dnsHost(host) {
			return nil, errors.New("invalid upstream dial target")
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host+".")
		if err != nil {
			return nil, err
		}
		for _, ip := range ips {
			if !publicIP(ip.Unmap()) {
				return nil, errPublicIP
			}
		}
		var dialErr error = errors.New("upstream DNS returned no addresses")
		for _, ip := range ips {
			// Dial the exact checked address; TLS still verifies the URL host.
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
			if err == nil {
				return conn, nil
			}
			dialErr = err
		}
		return nil, dialErr
	}
	return publicTransport{base}
}

var publicIPv6 = netip.MustParsePrefix("2000::/3")
var specialNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3fff::/20"),
}

func publicIP(ip netip.Addr) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || (ip.Is6() && !publicIPv6.Contains(ip)) {
		return false
	}
	for _, prefix := range specialNetworks {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}
