// Package s3 implements S3 HTTP handlers and SigV4 authentication.
//
// This package will expose an http.Handler compatible router for S3 requests.
// Initially, handlers return NotImplemented stubs while we build out the
// storage, metadata, and auth layers. Keep handler signatures stable.
package s3
