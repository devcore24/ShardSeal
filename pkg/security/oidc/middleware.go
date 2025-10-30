package oidc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
)

// Config defines OIDC verification settings for the Admin API.
// Typical minimal config requires Issuer and ClientID, or a JWKSURL + Audience.
type Config struct {
	// Issuer is the OIDC issuer URL (e.g., https://accounts.google.com or https://auth.example.com/realms/dev).
	// When provided, the provider's well-known metadata will be used to discover JWKS and other endpoints.
	Issuer string

	// ClientID is the expected audience/client_id for ID tokens.
	// If Audience is not set, ClientID will be used as the expected audience for verification.
	ClientID string

	// Audience, when set, overrides ClientID as the expected audience value during verification.
	// Useful when validating access tokens issued for a resource API audience.
	Audience string

	// JWKSURL is an optional direct JWKS endpoint URL. When provided without Issuer,
	// verification will use a remote key set fetched from JWKSURL.
	JWKSURL string
}

// Verifier validates Bearer tokens (ID/Access tokens) using OIDC.
// It supports discovery via Issuer or direct JWKS URL.
// By default it expects audience to match ClientID (or Audience when provided).
type Verifier struct {
	verifier *gooidc.IDTokenVerifier
}

// NewVerifier builds a token verifier based on the provided Config.
func NewVerifier(ctx context.Context, cfg Config) (*Verifier, error) {
// Determine audience expectation
expectedAud := cfg.Audience
if expectedAud == "" {
	expectedAud = cfg.ClientID
}

switch {
case cfg.Issuer != "":
	provider, err := gooidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc: provider discovery failed: %w", err)
	}
	ver := provider.Verifier(&gooidc.Config{
		ClientID: expectedAud,
	})
	return &Verifier{verifier: ver}, nil
case cfg.JWKSURL != "":
	ks := gooidc.NewRemoteKeySet(ctx, cfg.JWKSURL)
	// Empty issuer means skip issuer check; if cfg.Issuer is provided it will be enforced.
	ver := gooidc.NewVerifier(cfg.Issuer, ks, &gooidc.Config{
		ClientID: expectedAud,
	})
	return &Verifier{verifier: ver}, nil
default:
	return nil, errors.New("oidc: either Issuer or JWKSURL must be provided")
}
}

// Subject holds verified identity fields extracted from the token.
type Subject struct {
	Subject   string
	Issuer    string
	Audience  string
	ExpiresAt time.Time
	Roles     []string
	Scopes    []string
}

// Verify parses and validates a Bearer token string and returns subject info.
func (v *Verifier) Verify(ctx context.Context, rawToken string) (*Subject, error) {
	if v == nil || v.verifier == nil {
		return nil, errors.New("oidc: verifier not initialized")
	}
	idt, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		return nil, fmt.Errorf("oidc: token verification failed: %w", err)
	}
	var claims struct {
		Exp         int64             `json:"exp"`
		Sub         string            `json:"sub"`
		Iss         string            `json:"iss"`
		Aud         any               `json:"aud"` // string or []string
		Roles       any               `json:"roles"`
		Scope       string            `json:"scope"`
		Scp         any               `json:"scp"`
		RealmAccess struct {
			Roles []string `json:"roles"`
		} `json:"realm_access"`
	}
	if err := idt.Claims(&claims); err != nil {
		return nil, fmt.Errorf("oidc: parse claims: %w", err)
	}
	var aud string
	switch t := claims.Aud.(type) {
	case string:
		aud = t
	case []any:
		if len(t) > 0 {
			if s, ok := t[0].(string); ok {
				aud = s
			}
		}
	case []string:
		if len(t) > 0 {
			aud = t[0]
		}
	}
	// Collect roles from multiple common locations
	roleSet := map[string]struct{}{}
	addRole := func(r string) {
		r = strings.TrimSpace(r)
		if r != "" {
			roleSet[r] = struct{}{}
		}
	}
	switch t := claims.Roles.(type) {
	case []any:
		for _, v := range t {
			if s, ok := v.(string); ok {
				addRole(s)
			}
		}
	case []string:
		for _, s := range t {
			addRole(s)
		}
	case string:
		// some IdPs encode single role as string
		addRole(t)
	}
	if len(claims.RealmAccess.Roles) > 0 {
		for _, r := range claims.RealmAccess.Roles {
			addRole(r)
		}
	}
	// scopes: "scope" (space-separated) or "scp" (array or string)
	var scopes []string
	if claims.Scope != "" {
		for _, s := range strings.Split(claims.Scope, " ") {
			s = strings.TrimSpace(s)
			if s != "" {
				scopes = append(scopes, s)
			}
		}
	}
	switch t := claims.Scp.(type) {
	case []any:
		for _, v := range t {
			if s, ok := v.(string); ok {
				scopes = append(scopes, strings.TrimSpace(s))
			}
		}
	case []string:
		for _, s := range t {
			scopes = append(scopes, strings.TrimSpace(s))
		}
	case string:
		for _, s := range strings.Split(t, " ") {
			s = strings.TrimSpace(s)
			if s != "" {
				scopes = append(scopes, s)
			}
		}
	}
	var roles []string
	for r := range roleSet {
		roles = append(roles, r)
	}

	return &Subject{
		Subject:   claims.Sub,
		Issuer:    claims.Iss,
		Audience:  aud,
		ExpiresAt: time.Unix(claims.Exp, 0).UTC(),
		Roles:     roles,
		Scopes:    scopes,
	}, nil
}

 // VerifierIface allows plugging a custom verifier (and simplifies testing).
type VerifierIface interface {
	Verify(ctx context.Context, rawToken string) (*Subject, error)
}

// Context helpers to access verified subject downstream (e.g., RBAC).
type contextKey string

const subjectContextKey contextKey = "oidcSubject"

func WithSubject(ctx context.Context, s *Subject) context.Context {
	if s == nil {
		return ctx
	}
	return context.WithValue(ctx, subjectContextKey, s)
}

func SubjectFromContext(ctx context.Context) (*Subject, bool) {
	v := ctx.Value(subjectContextKey)
	if v == nil {
		return nil, false
	}
	if s, ok := v.(*Subject); ok {
		return s, true
	}
	return nil, false
}

// Middleware enforces OIDC Bearer auth on incoming requests.
// It expects Authorization: Bearer <token>. On success it sets X-Admin-Subject header
// with the verified subject. On failure it returns 401.
func Middleware(v VerifierIface, exempt func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if exempt != nil && exempt(r) {
				next.ServeHTTP(w, r)
				return
			}
			authz := r.Header.Get("Authorization")
			if authz == "" || !strings.HasPrefix(authz, "Bearer ") {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			raw := strings.TrimSpace(strings.TrimPrefix(authz, "Bearer "))
			subj, err := v.Verify(r.Context(), raw)
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			// Propagate subject via header and context
			ctx := r.Context()
			if subj != nil {
				w.Header().Set("X-Admin-Subject", subj.Subject)
				ctx = WithSubject(ctx, subj)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}