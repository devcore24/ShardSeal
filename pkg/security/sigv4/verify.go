package sigv4

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
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

// VerifyRequest validates AWS Signature V4 for both header-signed and presigned (query) requests.
// Supported:
// - Algorithm: AWS4-HMAC-SHA256
// - Header-signed: Authorization header with Credential, SignedHeaders, Signature; X-Amz-Date; x-amz-content-sha256
// - Presigned URLs: X-Amz-Algorithm, X-Amz-Credential, X-Amz-SignedHeaders, X-Amz-Signature, X-Amz-Date
// Payload hash:
// - If x-amz-content-sha256 is "UNSIGNED-PAYLOAD", uses that literal
// - Otherwise requires x-amz-content-sha256 header for requests with a body; for GET/HEAD it defaults to "UNSIGNED-PAYLOAD"
func VerifyRequest(ctx context.Context, r *http.Request, store CredentialsStore) error {
	// Check presigned first
	q := r.URL.Query()
	if algo := q.Get("X-Amz-Algorithm"); algo != "" {
		if algo != "AWS4-HMAC-SHA256" {
			return ErrAuthInvalid
		}

		cred := q.Get("X-Amz-Credential")
		ak, scopeDate, region, service, err := parseCredentialScope(cred)
		if err != nil {
			return ErrAuthInvalid
		}
		secret, _, ok := store.Lookup(ak)
		if !ok {
			return ErrAuthInvalid
		}
		amzDate := q.Get("X-Amz-Date")
		if amzDate == "" {
			return ErrAuthInvalid
		}

		signedHeadersCSV := q.Get("X-Amz-SignedHeaders")
		if signedHeadersCSV == "" {
			return ErrAuthInvalid
		}
		signedHeaders := strings.Split(strings.ToLower(signedHeadersCSV), ";")

		// Determine payload hash
		payloadHash := q.Get("X-Amz-Content-Sha256")
		if payloadHash == "" {
			// Default to UNSIGNED-PAYLOAD for typical presigned GET/HEAD
			payloadHash = "UNSIGNED-PAYLOAD"
		}

		canonicalReq, err := buildCanonicalRequest(r, signedHeaders, payloadHash, true /*presigned*/)
		if err != nil {
			return ErrAuthInvalid
		}
		crHash := sha256Hex([]byte(canonicalReq))
		stringToSign := buildStringToSign(amzDate, scopeDate, region, service, crHash)
		signingKey := deriveSigningKey(secret, scopeDate, region, service)
		expectedSig := hexLower(hmacSHA256Hex(signingKey, []byte(stringToSign)))
		got := q.Get("X-Amz-Signature")
		if got == "" {
			return ErrAuthInvalid
		}
		if strings.ToLower(got) != expectedSig {
			return ErrAuthInvalid
		}
		return nil
	}

	// Header-based
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ErrAuthMissing
	}
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		return ErrAuthInvalid
	}
	ak, scopeDate, region, service, signedHeaders, signature, err := parseAuthorizationHeader(auth)
	if err != nil {
		return ErrAuthInvalid
	}
	secret, _, ok := store.Lookup(ak)
	if !ok {
		return ErrAuthInvalid
	}
	amzDate := r.Header.Get("X-Amz-Date")
	if amzDate == "" {
		return ErrAuthInvalid
	}

	// Determine payload hash
	payloadHash, phErr := getPayloadHashFromHeaders(r)
	if phErr != nil {
		return ErrAuthInvalid
	}

	canonicalReq, err := buildCanonicalRequest(r, signedHeaders, payloadHash, false /*presigned*/)
	if err != nil {
		return ErrAuthInvalid
	}
	crHash := sha256Hex([]byte(canonicalReq))
	stringToSign := buildStringToSign(amzDate, scopeDate, region, service, crHash)
	signingKey := deriveSigningKey(secret, scopeDate, region, service)
	expectedSig := hexLower(hmacSHA256Hex(signingKey, []byte(stringToSign)))
	if strings.ToLower(signature) != expectedSig {
		return ErrAuthInvalid
	}
	return nil
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

// parseCredentialScope returns ak, date(YYYYMMDD), region, service from Credential scope.
func parseCredentialScope(cred string) (string, string, string, string, error) {
	if cred == "" {
		return "", "", "", "", ErrAuthInvalid
	}
	parts := strings.Split(cred, "/")
	if len(parts) < 5 {
		return "", "", "", "", ErrAuthInvalid
	}
	ak := strings.TrimSpace(parts[0])
	date := strings.TrimSpace(parts[1])
	region := strings.TrimSpace(parts[2])
	service := strings.TrimSpace(parts[3])
	if ak == "" || date == "" || region == "" || service == "" {
		return "", "", "", "", ErrAuthInvalid
	}
	return ak, date, region, service, nil
}

// parseAuthorizationHeader parses AWS4 auth header and returns components.
func parseAuthorizationHeader(auth string) (ak, date, region, service string, signedHeaders []string, signature string, err error) {
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 ") {
		return "", "", "", "", nil, "", ErrAuthInvalid
	}
	kv := strings.TrimPrefix(auth, "AWS4-HMAC-SHA256 ")
	// split by comma into key=value tokens
	toks := strings.Split(kv, ",")
	var credVal, shVal, sigVal string
	for _, t := range toks {
		t = strings.TrimSpace(t)
		if strings.HasPrefix(t, "Credential=") {
			credVal = strings.TrimPrefix(t, "Credential=")
		} else if strings.HasPrefix(t, "SignedHeaders=") {
			shVal = strings.TrimPrefix(t, "SignedHeaders=")
		} else if strings.HasPrefix(t, "Signature=") {
			sigVal = strings.TrimPrefix(t, "Signature=")
		}
	}
	if credVal == "" || shVal == "" || sigVal == "" {
		return "", "", "", "", nil, "", ErrAuthInvalid
	}
	ak, date, region, service, err = parseCredentialScope(credVal)
	if err != nil {
		return "", "", "", "", nil, "", ErrAuthInvalid
	}
	signedHeaders = strings.Split(strings.ToLower(strings.TrimSpace(shVal)), ";")
	signature = strings.TrimSpace(sigVal)
	return
}

// buildCanonicalRequest builds the SigV4 canonical request.
func buildCanonicalRequest(r *http.Request, signedHeaders []string, payloadHash string, presigned bool) (string, error) {
	// Method
	method := r.Method

	// Canonical URI: escaped path, ensure leading slash
	canonURI := r.URL.EscapedPath()
	if canonURI == "" || canonURI[0] != '/' {
		canonURI = "/" + canonURI
	}
	// Normalize: encode each segment (keep '/')
	canonURI = uriEncodePath(canonURI)

	// Canonical query string
	canonQuery := canonicalQueryString(r, presigned)

	// Canonical headers and signed headers
	canonHeaders, signed := canonicalHeadersAndList(r, signedHeaders)
	if len(signed) == 0 {
		return "", ErrAuthInvalid
	}
	signedCSV := strings.Join(signed, ";")

	var b bytes.Buffer
	b.WriteString(method)
	b.WriteByte('\n')
	b.WriteString(canonURI)
	b.WriteByte('\n')
	b.WriteString(canonQuery)
	b.WriteByte('\n')
	b.WriteString(canonHeaders)
	b.WriteByte('\n')
	b.WriteString(signedCSV)
	b.WriteByte('\n')
	b.WriteString(payloadHash)

	return b.String(), nil
}

func canonicalQueryString(r *http.Request, presigned bool) string {
	// Include all query params; for presigned exclude X-Amz-Signature
	values := r.URL.Query()
	keys := make([]string, 0, len(values))
	for k := range values {
		if presigned && strings.EqualFold(k, "X-Amz-Signature") {
			continue
		}
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return strings.ToLower(keys[i]) < strings.ToLower(keys[j]) })
	var parts []string
	for _, k := range keys {
		vs := values[k]
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k, false)+"="+uriEncode(v, false))
		}
	}
	return strings.Join(parts, "&")
}

func canonicalHeadersAndList(r *http.Request, signedHeaders []string) (string, []string) {
	// Ensure 'host' and 'x-amz-date' are included if present
	need := map[string]struct{}{}
	for _, h := range signedHeaders {
		need[strings.ToLower(h)] = struct{}{}
	}
	if _, ok := need["host"]; !ok {
		// If signedHeaders was provided without host, we cannot proceed
		// Some clients may always include host; enforce presence
	}
	// Build lowercased header map
	type kv struct{ k, v string }
	var hdrs []kv
	for name := range need {
		// special-case host from r.Host if not in Header map
		if name == "host" {
			host := r.Host
			if host == "" {
				// try header
				host = r.Header.Get("Host")
			}
			if host != "" {
				hdrs = append(hdrs, kv{k: "host", v: trimSpaces(host)})
			}
			continue
		}
		val := r.Header.Get(name)
		if val == "" {
			// Try canonical-cased retrieval to be safe
			val = r.Header.Get(headerCanonicalCase(name))
		}
		if val != "" {
			hdrs = append(hdrs, kv{k: strings.ToLower(name), v: trimSpaces(val)})
		}
	}
	// Sort by header name
	sort.Slice(hdrs, func(i, j int) bool { return hdrs[i].k < hdrs[j].k })
	var b bytes.Buffer
	actual := make([]string, 0, len(hdrs))
	for _, h := range hdrs {
		b.WriteString(h.k)
		b.WriteByte(':')
		b.WriteString(h.v)
		b.WriteByte('\n')
		actual = append(actual, h.k)
	}
	return b.String(), actual
}

func headerCanonicalCase(s string) string {
	// Minimal canonicalization for common headers
	switch strings.ToLower(s) {
	case "content-type":
		return "Content-Type"
	case "x-amz-content-sha256":
		return "X-Amz-Content-Sha256"
	case "x-amz-date":
		return "X-Amz-Date"
	case "host":
		return "Host"
	default:
		return s
	}
}

func uriEncode(s string, encodeSlash bool) string {
	var b bytes.Buffer
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') ||
			(c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else if c == '/' && !encodeSlash {
			b.WriteByte(c)
		} else {
			b.WriteString("%")
			b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

func uriEncodePath(path string) string {
	// Encode each segment but keep '/'
	if path == "" {
		return "/"
	}
	var b bytes.Buffer
	for i := 0; i < len(path); i++ {
		c := path[i]
		if c == '/' {
			b.WriteByte('/')
			continue
		}
		// Encode char
		if (c >= 'A' && c <= 'Z') ||
			(c >= 'a' && c <= 'z') ||
			(c >= '0' && c <= '9') ||
		c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteString("%")
			b.WriteString(strings.ToUpper(hex.EncodeToString([]byte{c})))
		}
	}
	return b.String()
}

func sha256Hex(bz []byte) string {
	h := sha256.Sum256(bz)
	return hex.EncodeToString(h[:])
}

func hmacSHA256Hex(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	sum := m.Sum(nil)
	dst := make([]byte, hex.EncodedLen(len(sum)))
	hex.Encode(dst, sum)
	return dst
}

func hexLower(b []byte) string { return strings.ToLower(string(b)) }

func deriveSigningKey(secret, date, region, service string) []byte {
	kDate := hmacSum([]byte("AWS4"+secret), []byte(date))
	kRegion := hmacSum(kDate, []byte(region))
	kService := hmacSum(kRegion, []byte(service))
	kSigning := hmacSum(kService, []byte("aws4_request"))
	return kSigning
}

func hmacSum(key, msg []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(msg)
	return m.Sum(nil)
}

func buildStringToSign(amzDate, date, region, service, canonicalRequestHash string) string {
	var b bytes.Buffer
	b.WriteString("AWS4-HMAC-SHA256\n")
	b.WriteString(amzDate)
	b.WriteByte('\n')
	b.WriteString(date)
	b.WriteByte('/')
	b.WriteString(region)
	b.WriteString("/" + service + "/aws4_request\n")
	b.WriteString(canonicalRequestHash)
	return b.String()
}

func trimSpaces(s string) string {
	// Collapse sequential spaces to single space and trim
	s = strings.TrimSpace(s)
	var b strings.Builder
	space := false
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if !space {
				b.WriteByte(' ')
				space = true
			}
		} else {
			space = false
			b.WriteRune(r)
		}
	}
	return b.String()
}

func getPayloadHashFromHeaders(r *http.Request) (string, error) {
	h := r.Header.Get("X-Amz-Content-Sha256")
	if h == "" {
		// For GET/HEAD, default to UNSIGNED-PAYLOAD
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			return "UNSIGNED-PAYLOAD", nil
		}
		// For other methods with potential body, require explicit header to avoid consuming body here
		cl := r.Header.Get("Content-Length")
		if cl == "" {
			// No body
			return sha256Hex(nil), nil
		}
		return "", ErrAuthInvalid
	}
	if strings.EqualFold(h, "UNSIGNED-PAYLOAD") {
		return "UNSIGNED-PAYLOAD", nil
	}
	// If header is provided as hex, do not recompute
	if len(h) == 64 {
		return strings.ToLower(h), nil
	}
	// As a fallback, compute if body is small and seekable (rare in header path). Avoid altering r.Body for handlers.
	if r.Body != nil {
		var buf bytes.Buffer
		tee := io.TeeReader(r.Body, &buf)
		sum := sha256HexFromReader(tee)
		// restore body
		r.Body = io.NopCloser(&buf)
		return sum, nil
	}
	return sha256Hex(nil), nil
}

func sha256HexFromReader(r io.Reader) string {
	h := sha256.New()
	_, _ = io.Copy(h, r)
	return hex.EncodeToString(h.Sum(nil))
}

// Optional: basic time check helper (not enforced yet)
func parseAmzDate(s string) (time.Time, error) {
	// Format: 20060102T150405Z
	return time.Parse("20060102T150405Z", s)
}