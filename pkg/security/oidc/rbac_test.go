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
// Additional tests for scrub and repair RBAC mappings and enforcement

func TestDefaultAdminPolicy_Mappings_ScrubAndRepair(t *testing.T) {
	p := DefaultAdminPolicy()

	// GET /admin/scrub/stats -> admin.read
	req := httptest.NewRequest(http.MethodGet, "/admin/scrub/stats", nil)
	if got := p(req); len(got) != 1 || got[0] != "admin.read" {
		t.Fatalf("policy(GET /admin/scrub/stats) = %v, want [admin.read]", got)
	}

	// POST /admin/scrub/runonce -> admin.scrub
	req = httptest.NewRequest(http.MethodPost, "/admin/scrub/runonce", nil)
	if got := p(req); len(got) != 1 || got[0] != "admin.scrub" {
		t.Fatalf("policy(POST /admin/scrub/runonce) = %v, want [admin.scrub]", got)
	}

	// GET /admin/repair/stats -> admin.repair.read
	req = httptest.NewRequest(http.MethodGet, "/admin/repair/stats", nil)
	if got := p(req); len(got) != 1 || got[0] != "admin.repair.read" {
		t.Fatalf("policy(GET /admin/repair/stats) = %v, want [admin.repair.read]", got)
	}

	// POST /admin/repair/enqueue -> admin.repair.enqueue
	req = httptest.NewRequest(http.MethodPost, "/admin/repair/enqueue", nil)
	if got := p(req); len(got) != 1 || got[0] != "admin.repair.enqueue" {
		t.Fatalf("policy(POST /admin/repair/enqueue) = %v, want [admin.repair.enqueue]", got)
	}
}

func TestRBAC_DefaultPolicy_RepairStats_PassesWithRole(t *testing.T) {
	h := RBAC(DefaultAdminPolicy())(okHandler())
	subj := &Subject{Subject: "alice", Roles: []string{"admin.repair.read"}}

	rr := httptest.NewRecorder()
	req := makeReq(http.MethodGet, "/admin/repair/stats", subj)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
}

func TestRBAC_DefaultPolicy_RepairEnqueue_PassesWithRole(t *testing.T) {
	h := RBAC(DefaultAdminPolicy())(okHandler())
	subj := &Subject{Subject: "bob", Roles: []string{"admin.repair.enqueue"}}

	rr := httptest.NewRecorder()
	req := makeReq(http.MethodPost, "/admin/repair/enqueue", subj)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
}

func TestRBAC_DefaultPolicy_RepairEnqueue_ForbiddenWithoutRole(t *testing.T) {
	h := RBAC(DefaultAdminPolicy())(okHandler())
	// Subject lacks required role
	subj := &Subject{Subject: "carol", Roles: []string{"admin.read"}}

	rr := httptest.NewRecorder()
	req := makeReq(http.MethodPost, "/admin/repair/enqueue", subj)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d", rr.Code)
	}
}