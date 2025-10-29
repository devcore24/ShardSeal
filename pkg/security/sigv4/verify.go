package sigv4

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// Package sigv4 provides scaffolding for AWS Signature V4 authentication.
// NOTE: This is an initial scaffold. It currently validates the presence of
// credentials and known access keys but DOES NOT compute/verify HMAC signatures yet.
// Do NOT use in production. A full canonical request + signing implementation will follow.

// Errors returned by the verifier.
var (
	ErrAuthMissing     = errors.New("sigv4: missing authorization")
	ErrAuthInvalid     = errors.New("sigv4: invalid authorization")
	ErrNotImplemented  = errors.New("sigv4: signature verification not implemented yet")
)

// CredentialsStore provides a way to look up a secret key by access key.
type CredentialsStore interface {
	Lookup(accessKey string) (secret string, user string, ok bool)
}

// AccessKey represents a static access/secret key pair and optional user label.
// This mirrors (but intentionally does not import) config.StaticAccessKey to avoid cycles.
type AccessKey struct {
	AccessKey string
	SecretKey string
	User      string
}

// StaticCredentialsStore is an in-memory implementation of CredentialsStore.
type StaticCredentialsStore struct {
	creds map[string]struct {
		secret string
		user   string
	}
}

// NewStaticStore builds a StaticCredentialsStore from a slice of AccessKey.
func NewStaticStore(keys []AccessKey) *StaticCredentialsStore {
	m := make(map[string]struct {
		secret string
		user   string
	})
	for _, k := range keys {
		ak := strings.TrimSpace(k.AccessKey)
		sk := strings.TrimSpace(k.SecretKey)
		if ak == "" || sk == "" {
			continue
		}
		m[ak] = struct {
			secret string
			user   string
		}{secret: sk, user: strings.TrimSpace(k.User)}
	}
	return &StaticCredentialsStore{creds: m}
}

// Lookup implements CredentialsStore.
func (s *StaticCredentialsStore) Lookup(accessKey string) (string, string, bool) {
	if s == nil || s.creds == nil {
		return "", "", false
	}
	v, ok := s.creds[accessKey]
	if !ok {
		return "", "", false
	}
	return v.secret, v.user, true
}

// VerifyRequest performs basic SigV4 request validation.
// CURRENT BEHAVIOR:
// - Ensures an Authorization header (or query-based auth) is present
// - Extracts the access key and checks that it's known
// - Returns ErrNotImplemented (signature computation not yet wired)
// INTENDED FUTURE BEHAVIOR:
// - Parse canonical request, derive signing key (date/region/service), HMAC signature verification
// - Support both header and querystring (presigned URL) authentication
func VerifyRequest(ctx context.Context, r *http.Request, store CredentialsStore) error {
	// Check query-string authentication first (presigned URLs)
	q := r.URL.Query()
	if algo := q.Get("X-Amz-Algorithm"); algo != "" {
		// Expect AWS4-HMAC-SHA256
		if algo != "AWS4-HMAC-SHA256" {
			return ErrAuthInvalid
		}
		cred := q.Get("X-Amz-Credential")
		ak, err := extractAccessKeyFromCredential(cred)
		if err != nil {
			return ErrAuthInvalid
		}
		if _, _, ok := store.Lookup(ak); !ok {
			return ErrAuthInvalid
		}
		// TODO: Perform full canonical request and signature verification
		return ErrNotImplemented
	}

	// Header-based authentication
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ErrAuthMissing
	}
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		return ErrAuthInvalid
	}
	// Extract Credential=... from the auth header
	ak, err := extractAccessKeyFromAuthorization(auth)
	if err != nil {
		return ErrAuthInvalid
	}
	if _, _, ok := store.Lookup(ak); !ok {
		return ErrAuthInvalid
	}
	// TODO: Compute canonical request, derive signing key, verify signature
	return ErrNotImplemented
}

// Middleware returns an HTTP middleware that enforces SigV4 verification
// except for requests where exempt(r) == true.
// If verification fails, it responds with 403 Forbidden and a minimal error body.
// NOTE: Error body is plain text for now; the S3 XML error envelope can be added later
// without importing the s3 package (to avoid cycles) by duplicating a minimal encoder here.
func Middleware(store CredentialsStore, exempt func(*http.Request) bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if exempt != nil && exempt(r) {
				next.ServeHTTP(w, r)
				return
			}
			if err := VerifyRequest(r.Context(), r, store); err != nil {
				// Map errors to HTTP status (403 for auth failures)
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte("AccessDenied: " + err.Error()))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Helper: extract access key from Authorization header's Credential= component.
// Authorization: AWS4-HMAC-SHA256 Credential=<AKID>/<Date>/<Region>/s3/aws4_request, SignedHeaders=..., Signature=...
func extractAccessKeyFromAuthorization(auth string) (string, error) {
	// Split by spaces to get the kv pairs
	// Example pieces: ["AWS4-HMAC-SHA256", "Credential=AKID/20250101/us-east-1/s3/aws4_request,", "SignedHeaders=host;x-amz-date", "Signature=..."]
	parts := strings.Split(auth, " ")
	if len(parts) < 2 {
		return "", ErrAuthInvalid
	}
	kv := parts[1:]
	// Join back in case of commas then split by comma
	joined := strings.Join(kv, " ")
	fields := strings.Split(joined, ",")
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if strings.HasPrefix(f, "Credential=") {
			cred := strings.TrimPrefix(f, "Credential=")
			return extractAccessKeyFromCredential(cred)
		}
	}
	return "", ErrAuthInvalid
}

// Helper: Credential format: <AKID>/<Date>/<Region>/<Service>/aws4_request
func extractAccessKeyFromCredential(cred string) (string, error) {
	if cred == "" {
		return "", ErrAuthInvalid
	}
	slash := strings.Split(cred, "/")
	if len(slash) < 5 {
		return "", ErrAuthInvalid
	}
	ak := strings.TrimSpace(slash[0])
	if ak == "" {
		return "", ErrAuthInvalid
	}
	return ak, nil
}