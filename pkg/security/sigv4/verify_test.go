package sigv4

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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

	if err := VerifyRequest(context.Background(), r, store); err == nil {
		t.Fatalf("expected VerifyRequest to fail with bad signature")
	}
}