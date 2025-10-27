
<img src="img/s3free.png" width="260">

# S3free

## Open S3-compatible, self-healing object store written in Go. Work in progress, not production-ready yet:

- Implemented
  - S3 basics: ListBuckets (/), CreateBucket (PUT /{bucket}), DeleteBucket (DELETE /{bucket})
  - Objects: Put (PUT /{bucket}/{key}), Get (GET), Head (HEAD), Delete (DELETE)
  - Range GET support (single range)
  - Config (YAML + env), structured logging, CI
  - Local filesystem storage backend (dev/MVP), in-memory metadata store
  - Unit tests for buckets/objects
- Not yet implemented
  - ListObjectsV2 (bucket object listing)
  - Multipart uploads
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

### Using with curl (no auth yet)
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

# Delete object
curl -X DELETE http://localhost:8080/my-bucket/hello.txt

# Delete bucket (must be empty)
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
```
<pre>
Environment overrides:
- S3FREE_ADDR         // server listen address (e.g., 0.0.0.0:8080)
- S3FREE_DATA_DIRS    // comma-separated data directories
- S3FREE_CONFIG       // path to YAML config
</pre>

## Notes & limitations (current MVP)
- No authentication yet
- ETag is MD5 of full object for single-part PUTs
- Objects stored under ./data/objects/{bucket}/{key}
- No listing of objects in a bucket yet (ListObjectsV2 pending)

## Roadmap (short)
1) ListObjectsV2 (prefix, delimiter, pagination)
2) Multipart Upload (initiate/upload-part/complete/abort)
3) AWS SigV4 authentication
4) Prometheus metrics and basic traces
5) BeeXL v1 storage format + erasure coding scaffold

## License
Apache-2.0

## Contributing
Early-stage project — issues and PRs welcome. Please keep code documented and tested.
