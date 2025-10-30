package oidc

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func makeReq(method, path string, subj *Subject) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	if subj != nil {
		req = req.WithContext(WithSubject(req.Context(), subj))
	}
	return req
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
}

func TestDefaultAdminPolicy_Mappings(t *testing.T) {
	p := DefaultAdminPolicy()

	// GET /admin/health -> admin.read
	req := httptest.NewRequest(http.MethodGet, "/admin/health", nil)
	if got := p(req); len(got) != 1 || got[0] != "admin.read" {
		t.Fatalf("policy(/admin/health) = %v, want [admin.read]", got)
	}

	// GET /admin/version -> admin.read
	req = httptest.NewRequest(http.MethodGet, "/admin/version", nil)
	if got := p(req); len(got) != 1 || got[0] != "admin.read" {
		t.Fatalf("policy(/admin/version) = %v, want [admin.read]", got)
	}

	// POST /admin/gc/multipart -> admin.gc
	req = httptest.NewRequest(http.MethodPost, "/admin/gc/multipart", nil)
	if got := p(req); len(got) != 1 || got[0] != "admin.gc" {
		t.Fatalf("policy(POST /admin/gc/multipart) = %v, want [admin.gc]", got)
	}

	// Other GET /admin/* -> admin.read
	req = httptest.NewRequest(http.MethodGet, "/admin/something", nil)
	if got := p(req); len(got) != 1 || got[0] != "admin.read" {
		t.Fatalf("policy(/admin/something) = %v, want [admin.read]", got)
	}

	// Non-admin path -> nil (no RBAC requirement)
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if got := p(req); got != nil {
		t.Fatalf("policy(/metrics) = %v, want nil", got)
	}
}

func TestRBAC_NoRequirement_PassesWithoutSubject(t *testing.T) {
	policy := func(r *http.Request) []string { return nil }
	h := RBAC(policy)(okHandler())

	rr := httptest.NewRecorder()
	req := makeReq(http.MethodGet, "/metrics", nil) // no subject
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
}

func TestRBAC_RequireRole_NoSubject_Forbidden(t *testing.T) {
	policy := func(r *http.Request) []string { return []string{"admin.read"} }
	h := RBAC(policy)(okHandler())

	rr := httptest.NewRecorder()
	req := makeReq(http.MethodGet, "/admin/health", nil) // no subject
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rr.Code)
	}
}

func TestRBAC_RequireRole_HasRole_Passes(t *testing.T) {
	policy := func(r *http.Request) []string { return []string{"admin.read"} }
	h := RBAC(policy)(okHandler())

	subj := &Subject{Subject: "alice", Roles: []string{"admin.read"}}
	rr := httptest.NewRecorder()
	req := makeReq(http.MethodGet, "/admin/health", subj)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
}

func TestRBAC_RequireRole_HasScope_Passes(t *testing.T) {
	policy := func(r *http.Request) []string { return []string{"admin.gc"} }
	h := RBAC(policy)(okHandler())

	subj := &Subject{Subject: "bob", Scopes: []string{"admin.gc"}}
	rr := httptest.NewRecorder()
	req := makeReq(http.MethodPost, "/admin/gc/multipart", subj)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
}