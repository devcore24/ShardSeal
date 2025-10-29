
<img src="img/s3free.png" width="260">

# S3free

## Open S3-compatible, self-healing object store written in Go. Work in progress, not production-ready yet:

- Implemented
  - S3 basics: ListBuckets (/), CreateBucket (PUT /{bucket}), DeleteBucket (DELETE /{bucket})
  - Objects: Put (PUT /{bucket}/{key}), Get (GET), Head (HEAD), Delete (DELETE)
  - Range GET support (single range, requires seekable storage)
  - ListObjectsV2 (bucket object listing with prefix, pagination)
  - Multipart uploads (initiate/upload-part/complete/abort)
  - Config (YAML + env), structured logging, CI
  - Local filesystem storage backend (dev/MVP), in-memory metadata store
  - Unit tests for buckets/objects/multipart
  - **Production-ready fixes:** Streaming multipart completion, safe range handling, improved error logging
- Not yet implemented
  - AWS Signature V4 authentication
  - Metrics/Tracing, BeeXL v1 self-healing format, erasure coding, background scrubber
  - Distributed metadata/placement

## Quick start
### Prerequisites
- Go 1.22+ installed

### Build and run
```bash
make build
# Run with sample config (will ensure ./data exists)
S3FREE_CONFIG=configs/local.yaml make run
# Or
# go run ./cmd/s3free
```

Default address: :8080 (override with env S3FREE_ADDR). Data dirs: ./data (override with env S3FREE_DATA_DIRS as comma-separated list).

### Using with curl (auth disabled by default; SigV4 optional)
Bucket naming: 3-63 chars; lowercase letters, digits, dots, hyphens; must start/end with letter or digit.

```bash
# List all buckets
curl -v http://localhost:8080/

# Create a bucket
curl -v -X PUT http://localhost:8080/my-bucket

# Put an object (from stdin)
printf 'Hello, s3free!\n' | curl -v -X PUT http://localhost:8080/my-bucket/hello.txt --data-binary @-

# Get an object
curl -v http://localhost:8080/my-bucket/hello.txt

# Range GET (first 10 bytes)
curl -v -H 'Range: bytes=0-9' http://localhost:8080/my-bucket/hello.txt

# Head object
curl -I http://localhost:8080/my-bucket/hello.txt

# List objects in bucket
curl -s "http://localhost:8080/my-bucket?list-type=2"

# List with prefix filter
curl -s "http://localhost:8080/my-bucket?list-type=2&prefix=folder/"

# Delete object
curl -X DELETE http://localhost:8080/my-bucket/hello.txt

# Delete bucket (must be empty - excludes internal .multipart files)
curl -X DELETE http://localhost:8080/my-bucket
```

## Testing
```bash
go test ./...
# Verbose tests for just the S3 API package
go test ./pkg/api/s3 -v
```

## Configuration
Example at configs/local.yaml:
```yaml
address: ":8080"
dataDirs:
  - "./data"

# Authentication (optional)
# authMode: "none"        # "none" or "sigv4"
# accessKeys:
#   - accessKey: "AKIAEXAMPLE"
#     secretKey: "secret"
#     user: "local"
```
<pre>
Environment overrides:
- S3FREE_ADDR         // server listen address (e.g., 0.0.0.0:8080)
- S3FREE_DATA_DIRS    // comma-separated data directories
- S3FREE_CONFIG       // path to YAML config
- S3FREE_AUTH_MODE    // "none" (default) or "sigv4"
- S3FREE_ACCESS_KEYS  // comma-separated ACCESS_KEY:SECRET_KEY[:USER]
</pre>

## Notes & limitations (current MVP)
- Authentication: optional. AWS SigV4 supported (header and presigned; disabled by default via config/env).
- ETag is MD5 of full object for single-part PUTs
- Objects stored under ./data/objects/{bucket}/{key}
- Multipart temporary files stored in .multipart/ subdirectory (excluded from listings and bucket empty checks)
- Range requests require seekable storage (LocalFS supports this)

## Recent Improvements (2025-10-27)
- Fixed critical memory exhaustion bugs in multipart completion and range GET fallback
- Improved error handling and logging for better debugging
- Added comprehensive interface documentation for concurrency safety
- Fixed bucket deletion to properly handle internal temporary files

## Roadmap (short)
1) AWS SigV4 authentication
2) Prometheus metrics and basic traces
3) BeeXL v1 storage format + erasure coding scaffold
4) Background scrubber and self-healing

## License
Apache-2.0

## Contributing
Early-stage project — issues and PRs welcome. Please keep code documented and tested.
