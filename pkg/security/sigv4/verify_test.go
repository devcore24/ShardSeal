package sigv4

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func withFixedNow(t *testing.T, ts time.Time) func() {
	orig := nowFunc
	nowFunc = func() time.Time { return ts }
	return func() { nowFunc = orig }
}

func TestVerifyRequest_Header_Succeeds(t *testing.T) {
	// Static creds
	ak := "AKIDEXAMPLE"
	secret := "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	store := NewStaticStore([]AccessKey{{AccessKey: ak, SecretKey: secret, User: "u"}})

	// Fixed date/scope
	date := "20250101"
	amzDate := "20250101T120000Z"
	region := "us-east-1"
	service := "s3"

	// Build a GET request (unsigned payload)
	r := httptest.NewRequest(http.MethodGet, "http://example.com/bucket/object.txt", nil)
	// Signed headers
	signedHeaders := []string{"host", "x-amz-date"}
	r.Header.Set("X-Amz-Date", amzDate)

	// Compute canonical request and signature (must match VerifyRequest logic)
	payloadHash := "UNSIGNED-PAYLOAD"
	canonReq, err := buildCanonicalRequest(r, signedHeaders, payloadHash, false /*presigned*/)
	if err != nil {
		t.Fatalf("canonical request: %v", err)
	}
	crHash := sha256Hex([]byte(canonReq))
	stringToSign := buildStringToSign(amzDate, date, region, service, crHash)
	signingKey := deriveSigningKey(secret, date, region, service)
	signature := strings.ToLower(string(hmacSHA256Hex(signingKey, []byte(stringToSign))))

	auth := fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s/%s/%s/aws4_request, SignedHeaders=%s, Signature=%s",
		ak, date, region, service, strings.Join(signedHeaders, ";"), signature,
	)
	r.Header.Set("Authorization", auth)

	cleanup := withFixedNow(t, time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC))
	defer cleanup()

	if err := VerifyRequest(context.Background(), r, store); err != nil {
		t.Fatalf("VerifyRequest header path failed: %v", err)
	}
}

func TestVerifyRequest_Presigned_Succeeds(t *testing.T) {
	ak := "AKIDEXAMPLE"
	secret := "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	store := NewStaticStore([]AccessKey{{AccessKey: ak, SecretKey: secret}})

	date := "20250101"
	amzDate := "20250101T120000Z"
	region := "us-east-1"
	service := "s3"

	// Base request with host header present
	r := httptest.NewRequest(http.MethodGet, "http://example.com/bucket/data.bin", nil)

	// Presigned parameters (excluding Signature which we compute)
	q := r.URL.Query()
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", fmt.Sprintf("%s/%s/%s/%s/aws4_request", ak, date, region, service))
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-SignedHeaders", "host")
	q.Set("X-Amz-Expires", "600")
	// We are not adding X-Amz-Content-Sha256; VerifyRequest defaults to UNSIGNED-PAYLOAD for presigned
	r.URL.RawQuery = q.Encode()

	// Build canonical request in presigned mode with payload hash "UNSIGNED-PAYLOAD"
	payloadHash := "UNSIGNED-PAYLOAD"
	signedHeaders := []string{"host"}
	canonReq, err := buildCanonicalRequest(r, signedHeaders, payloadHash, true /*presigned*/)
	if err != nil {
		t.Fatalf("canonical request: %v", err)
	}
	crHash := sha256Hex([]byte(canonReq))
	stringToSign := buildStringToSign(amzDate, date, region, service, crHash)
	signingKey := deriveSigningKey(secret, date, region, service)
	signature := strings.ToLower(string(hmacSHA256Hex(signingKey, []byte(stringToSign))))

	// Attach signature
	q = r.URL.Query()
	q.Set("X-Amz-Signature", signature)
	r.URL.RawQuery = q.Encode()

	cleanup := withFixedNow(t, time.Date(2025, 1, 1, 12, 5, 0, 0, time.UTC))
	defer cleanup()

	if err := VerifyRequest(context.Background(), r, store); err != nil {
		t.Fatalf("VerifyRequest presigned path failed: %v", err)
	}
}

func TestVerifyRequest_BadSignature_Fails(t *testing.T) {
	ak := "AKIDEXAMPLE"
	secret := "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	store := NewStaticStore([]AccessKey{{AccessKey: ak, SecretKey: secret}})

	date := "20250101"
	amzDate := "20250101T120000Z"
	region := "us-east-1"
	service := "s3"

	r := httptest.NewRequest(http.MethodGet, "http://example.com/bucket/obj", nil)
	signedHeaders := []string{"host", "x-amz-date"}
	r.Header.Set("X-Amz-Date", amzDate)

	// Compute a valid signature first
	payloadHash := "UNSIGNED-PAYLOAD"
	canonReq, err := buildCanonicalRequest(r, signedHeaders, payloadHash, false)
	if err != nil {
		t.Fatalf("canonical request: %v", err)
	}
	crHash := sha256Hex([]byte(canonReq))
	stringToSign := buildStringToSign(amzDate, date, region, service, crHash)
	signingKey := deriveSigningKey(secret, date, region, service)
	validSig := strings.ToLower(string(hmacSHA256Hex(signingKey, []byte(stringToSign))))

	// Corrupt the signature
	badSig := validSig
	if len(badSig) > 2 {
		badSig = "00" + badSig[2:]
	}

	auth := fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s/%s/%s/aws4_request, SignedHeaders=%s, Signature=%s",
		ak, date, region, service, strings.Join(signedHeaders, ";"), badSig,
	)
	r.Header.Set("Authorization", auth)

	cleanup := withFixedNow(t, time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC))
	defer cleanup()

	if err := VerifyRequest(context.Background(), r, store); err == nil {
		t.Fatalf("expected VerifyRequest to fail with bad signature")
	}
}

func TestCanonicalQueryStringSorting(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://example.com/test?b=2&a=3&a=1&space=a%20b", nil)
	got := canonicalQueryString(r, false)
	want := "a=1&a=3&b=2&space=a%20b"
	if got != want {
		t.Fatalf("canonicalQueryString mismatch: got %q want %q", got, want)
	}
}

func TestCanonicalHeadersTrimWhitespace(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://example.com/test", nil)
	r.Host = "Example.COM"
	r.Header.Set("Content-Type", "text/plain  ; charset=utf-8")
	r.Header.Set("X-Amz-Date", "20250101T000000Z")
	headers, list := canonicalHeadersAndList(r, []string{"host", "content-type", "x-amz-date"})
	if !strings.Contains(headers, "content-type:text/plain ; charset=utf-8\n") {
		t.Fatalf("expected trimmed content-type header, got %q", headers)
	}
	if len(list) != 3 || list[0] != "content-type" || list[1] != "host" || list[2] != "x-amz-date" {
		t.Fatalf("unexpected signed header order: %v", list)
	}
}

func TestVerifyRequest_HeaderExpired(t *testing.T) {
	ak := "AKIDEXAMPLE"
	secret := "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	store := NewStaticStore([]AccessKey{{AccessKey: ak, SecretKey: secret}})

	date := "20250101"
	amzDate := "20250101T120000Z"
	region := "us-east-1"
	service := "s3"

	r := httptest.NewRequest(http.MethodGet, "http://example.com/bucket/object.txt", nil)
	signedHeaders := []string{"host", "x-amz-date"}
	r.Header.Set("X-Amz-Date", amzDate)

	payloadHash := "UNSIGNED-PAYLOAD"
	canonReq, err := buildCanonicalRequest(r, signedHeaders, payloadHash, false)
	if err != nil {
		t.Fatalf("canonical request: %v", err)
	}
	crHash := sha256Hex([]byte(canonReq))
	stringToSign := buildStringToSign(amzDate, date, region, service, crHash)
	signingKey := deriveSigningKey(secret, date, region, service)
	signature := strings.ToLower(string(hmacSHA256Hex(signingKey, []byte(stringToSign))))
	auth := fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s/%s/%s/aws4_request, SignedHeaders=%s, Signature=%s",
		ak, date, region, service, strings.Join(signedHeaders, ";"), signature,
	)
	r.Header.Set("Authorization", auth)

	cleanup := withFixedNow(t, time.Date(2025, 1, 1, 12, 30, 0, 0, time.UTC))
	defer cleanup()

	if err := VerifyRequest(context.Background(), r, store); !errors.Is(err, ErrRequestExpired) {
		t.Fatalf("expected ErrRequestExpired, got %v", err)
	}
}

func TestVerifyRequest_PresignedExpired(t *testing.T) {
	ak := "AKIDEXAMPLE"
	secret := "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	store := NewStaticStore([]AccessKey{{AccessKey: ak, SecretKey: secret}})

	date := "20250101"
	amzDate := "20250101T120000Z"
	region := "us-east-1"
	service := "s3"

	r := httptest.NewRequest(http.MethodGet, "http://example.com/bucket/data.bin", nil)
	q := r.URL.Query()
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", fmt.Sprintf("%s/%s/%s/%s/aws4_request", ak, date, region, service))
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-SignedHeaders", "host")
	q.Set("X-Amz-Expires", "60")
	r.URL.RawQuery = q.Encode()

	payloadHash := "UNSIGNED-PAYLOAD"
	canonReq, err := buildCanonicalRequest(r, []string{"host"}, payloadHash, true)
	if err != nil {
		t.Fatalf("canonical request: %v", err)
	}
	crHash := sha256Hex([]byte(canonReq))
	stringToSign := buildStringToSign(amzDate, date, region, service, crHash)
	signingKey := deriveSigningKey(secret, date, region, service)
	signature := strings.ToLower(string(hmacSHA256Hex(signingKey, []byte(stringToSign))))
	q = r.URL.Query()
	q.Set("X-Amz-Signature", signature)
	r.URL.RawQuery = q.Encode()

	cleanup := withFixedNow(t, time.Date(2025, 1, 1, 12, 17, 0, 0, time.UTC))
	defer cleanup()

	if err := VerifyRequest(context.Background(), r, store); !errors.Is(err, ErrRequestExpired) {
		t.Fatalf("expected ErrRequestExpired, got %v", err)
	}
}
