package oidc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeVerifier struct {
	wantToken string
	subj      *Subject
	err       error
}

func (f fakeVerifier) Verify(ctx context.Context, raw string) (*Subject, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.wantToken != "" && raw != f.wantToken {
		return nil, errors.New("bad token")
	}
	if f.subj != nil {
		return f.subj, nil
	}
	return &Subject{Subject: "subj"}, nil
}

func TestMiddleware_Exempt_AllowsThroughWithoutAuth(t *testing.T) {
	// Exempt all requests
	exempt := func(r *http.Request) bool { return true }
	downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	h := Middleware(fakeVerifier{}, exempt)(downstream)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/health", nil)
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	if got := rr.Header().Get("X-Admin-Subject"); got != "" {
		t.Fatalf("expected no X-Admin-Subject header on exempt path, got %q", got)
	}
}

func TestMiddleware_MissingAuth_Returns401(t *testing.T) {
	h := Middleware(fakeVerifier{}, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/version", nil)
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rr.Code)
	}
}

func TestMiddleware_InvalidToken_Returns401(t *testing.T) {
	h := Middleware(fakeVerifier{err: errors.New("invalid")}, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/version", nil)
	req.Header.Set("Authorization", "Bearer badtoken")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rr.Code)
	}
}

func TestMiddleware_ValidToken_SetsSubjectAndProxies(t *testing.T) {
	fv := fakeVerifier{
		wantToken: "t123",
		subj:      &Subject{Subject: "alice"},
	}
	downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	h := Middleware(fv, nil)(downstream)
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/version", nil)
	req.Header.Set("Authorization", "Bearer t123")
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rr.Code)
	}
	if got := rr.Header().Get("X-Admin-Subject"); got != "alice" {
		t.Fatalf("X-Admin-Subject header = %q, want %q", got, "alice")
	}
	if rr.Body.String() != "ok" {
		t.Fatalf("downstream body = %q, want %q", rr.Body.String(), "ok")
	}
}