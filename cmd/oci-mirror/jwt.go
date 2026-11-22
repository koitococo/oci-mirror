package main

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type bearerAuth struct {
	key             *rsa.PublicKey
	parser          *jwt.Parser
	realm, audience string
}

type registryClaims struct {
	jwt.RegisteredClaims
	Access []registryAccess `json:"access"`
}

type registryAccess struct {
	Type      string           `json:"type"`
	Name      string           `json:"name"`
	Actions   []string         `json:"actions"`
	ExpiresAt *jwt.NumericDate `json:"exp,omitempty"`
}

func newBearerAuth(certPath, issuer, audience, realm string) (*bearerAuth, error) {
	if strings.TrimSpace(issuer) == "" || strings.TrimSpace(audience) == "" {
		return nil, errors.New("JWT issuer and audience are required")
	}
	if strings.IndexFunc(audience, unsafeChallengeChar) >= 0 {
		return nil, errors.New("JWT audience is not a safe Bearer service value")
	}
	parsed, err := url.Parse(realm)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.Fragment != "" || parsed.String() != realm || strings.HasSuffix(parsed.Host, ":") ||
		strings.ContainsAny(realm, " #") || strings.IndexFunc(realm, unsafeChallengeChar) >= 0 {
		return nil, errors.New("Bearer realm must be a canonical absolute HTTPS URL without credentials or fragment")
	}

	data, err := os.ReadFile(certPath)
	if err != nil {
		return nil, errors.New("cannot read the JWT signing certificate")
	}
	data = bytes.TrimSpace(data)
	block, rest := pem.Decode(data)
	if !bytes.HasPrefix(data, []byte("-----BEGIN CERTIFICATE-----")) ||
		block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("JWT signing certificate must be a single CERTIFICATE PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, errors.New("cannot parse the JWT signing certificate")
	}
	// As with Zot's traditional Bearer auth, the certificate pins the signing
	// key; it is not a TLS certificate or a trust-chain assertion.
	key, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("JWT signing certificate must contain an RSA public key")
	}

	return &bearerAuth{
		key: key,
		parser: jwt.NewParser(
			jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}),
			jwt.WithExpirationRequired(),
			jwt.WithIssuer(issuer),
			jwt.WithAudience(audience),
			jwt.WithIssuedAt(), // Zot also checks iat when present.
		),
		realm:    realm,
		audience: audience,
	}, nil
}

var errInvalidBearer = errors.New("valid Bearer authentication is required")

func (a *bearerAuth) authenticate(r *http.Request) (*registryClaims, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return nil, errInvalidBearer
	}
	// Zot strips exactly one ASCII space after the scheme. Require the same
	// framing here so the accepted Authorization header can pass through intact.
	scheme, signed, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || signed == "" || strings.ContainsAny(signed, ", \t\r\n") {
		return nil, errInvalidBearer
	}

	claims := new(registryClaims)
	token, err := a.parser.ParseWithClaims(signed, claims, func(*jwt.Token) (any, error) {
		return a.key, nil
	})
	if err != nil || !token.Valid || strings.TrimSpace(claims.Subject) == "" {
		// JWT parse errors can include untrusted token fields, so do not return them.
		return nil, errInvalidBearer
	}
	return claims, nil
}

func (c *registryClaims) canPull(repository string) bool {
	for _, access := range c.Access {
		if access.Type == "repository" && access.Name == repository && slices.Contains(access.Actions, "pull") &&
			(access.ExpiresAt == nil || !access.ExpiresAt.Time.Before(time.Now())) {
			return true
		}
	}
	return false
}

const (
	challengeRepositoryComponent = `[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*`
	challengeTag                 = `[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}`
	challengeDigest              = `[a-z0-9]+(?:[+._-][a-z0-9]+)*:[a-zA-Z0-9=_-]+`
)

var challengeRepositoryRoute = regexp.MustCompile(`^/v2/(` + challengeRepositoryComponent +
	`(?:/` + challengeRepositoryComponent + `)*)/(?:manifests/(?:` + challengeTag + `|` + challengeDigest +
	`)|(?:blobs|referrers)/` + challengeDigest + `|blobs/uploads/[a-zA-Z0-9_-]*|tags/list)$`)

func (a *bearerAuth) writeChallenge(w http.ResponseWriter, r *http.Request) {
	var scope string
	if path := r.URL.Path; r.URL.EscapedPath() == path {
		if match := challengeRepositoryRoute.FindStringSubmatchIndex(path); match != nil {
			action := "pull"
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				action = "push"
			}
			scope = "repository:" + path[match[2]:match[3]] + ":" + action
		}
	}
	w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
	w.Header().Set("WWW-Authenticate", a.challengeHeader(scope))
	registryError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
}

func (a *bearerAuth) challengeHeader(scope string) string {
	if strings.IndexFunc(scope, unsafeChallengeChar) >= 0 {
		scope = ""
	}
	return `Bearer realm="` + a.realm + `",service="` + a.audience + `",scope="` + scope + `"`
}

func unsafeChallengeChar(r rune) bool {
	return r < ' ' || r > '~' || r == '"' || r == '\\'
}
