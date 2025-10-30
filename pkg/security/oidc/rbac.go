package oidc

import (
	"net/http"
)

// Policy maps an HTTP request to a list of required roles/scopes.
// If it returns an empty slice or nil, no RBAC check is enforced for that request.
type Policy func(*http.Request) []string

// RBAC enforces role/scope checks using the provided policy.
// It expects that OIDC middleware has already attached a Subject to the context.
func RBAC(policy Policy) func(http.Handler) http.Handler {
	if policy == nil {
		policy = func(r *http.Request) []string { return nil }
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reqRoles := policy(r)
			if len(reqRoles) == 0 {
				next.ServeHTTP(w, r)
				return
			}
			subj, ok := SubjectFromContext(r.Context())
			if !ok || subj == nil {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			if !hasAny(subj, reqRoles) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// hasAny returns true if the subject has ANY of the required roles/scopes (exact match).
func hasAny(s *Subject, required []string) bool {
	if s == nil || len(required) == 0 {
		return true
	}
	// exact match in Roles or Scopes
	for _, req := range required {
		for _, r := range s.Roles {
			if r == req {
				return true
			}
		}
		for _, sc := range s.Scopes {
			if sc == req {
				return true
			}
		}
	}
	return false
}

// DefaultAdminPolicy defines minimal role requirements for Admin API endpoints:
// - GET /admin/health, GET /admin/version require role/scope: "admin.read"
// - POST /admin/gc/multipart requires role/scope: "admin.gc"
// Other GET /admin/* default to "admin.read". Non-admin routes have no RBAC requirement here.
func DefaultAdminPolicy() Policy {
	return func(r *http.Request) []string {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/admin/health":
			return []string{"admin.read"}
		case r.Method == http.MethodGet && r.URL.Path == "/admin/version":
			return []string{"admin.read"}
		case r.Method == http.MethodPost && r.URL.Path == "/admin/gc/multipart":
			return []string{"admin.gc"}
		default:
			// apply read requirement to other GET /admin endpoints as a conservative default
			if r.Method == http.MethodGet && len(r.URL.Path) >= 7 && r.URL.Path[:7] == "/admin/" {
				return []string{"admin.read"}
			}
			// otherwise no RBAC requirement (non-admin routes)
			return nil
		}
	}
}