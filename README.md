
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
  - Prometheus metrics (/metrics) and HTTP instrumentation
  - Tracing: OpenTelemetry scaffold (optional; OTLP gRPC/HTTP)
  - Authentication: AWS Signature V4 (optional; header and presigned URL)
  - Local filesystem storage backend (dev/MVP), in-memory metadata store
  - Admin API skeleton (optional, separate port): /admin/health, /admin/version
  - Unit tests for buckets/objects/multipart
  - **Production-ready fixes:** Streaming multipart completion, safe range handling, improved error logging
- Not yet implemented
  - FreeXL v1 self-healing format, erasure coding, background scrubber
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

## Metrics
- Exposes Prometheus metrics at /metrics on the same HTTP server.
- Default counters and histograms:
  - s3free_http_requests_total{method,code}
  - s3free_http_request_duration_seconds_bucket/sum/count{method,code}
  - s3free_http_inflight_requests
- Example:
```bash
curl -s http://localhost:8080/metrics | head -n 20
```

## Health endpoints
- /livez: liveness probe (always OK when process is running)
- /readyz: readiness probe gated on initialization completion
- /metrics: Prometheus metrics endpoint

## Monitoring (Prometheus + Grafana)
- Prometheus sample config: configs/monitoring/prometheus/prometheus.yml
- Example alert rules: configs/monitoring/prometheus/rules.yml
- Grafana dashboard (import JSON): configs/monitoring/grafana/s3free_overview.json

Quick start:
```bash
# 1) Run s3free (default :8080 exposes /metrics)
S3FREE_CONFIG=configs/local.yaml make run

# 2) Start Prometheus (adjust path as needed)
prometheus --config.file=configs/monitoring/prometheus/prometheus.yml

# 3) Import Grafana dashboard JSON:
#    configs/monitoring/grafana/s3free_overview.json
#    and set the Prometheus datasource accordingly.
```

Admin endpoints (optional; if admin server enabled). If OIDC is enabled, these endpoints require a valid Bearer token. RBAC defaults are enforced: admin.read for GET endpoints; admin.gc for POST /admin/gc/multipart.
- /admin/health: JSON status with ready/version/addresses
- /admin/version: JSON version info

## Configuration
Example at configs/local.yaml:
```yaml
address: ":8080"
# Optional admin/control plane on a separate port (read-only endpoints)
# adminAddress: ":9090"

dataDirs:
  - "./data"

# Authentication (optional)
# authMode: "none"        # "none" or "sigv4"
# accessKeys:
#   - accessKey: "AKIAEXAMPLE"
#     secretKey: "secret"
#     user: "local"

# Tracing (optional - OpenTelemetry OTLP)
# tracing:
#   enabled: false
#   endpoint: "localhost:4317"  # grpc default; or "localhost:4318" for http
#   protocol: "grpc"            # "grpc" or "http"
#   sampleRatio: 0.0            # 0.0-1.0
#   serviceName: "s3free"
```
<pre>
Environment overrides:
- S3FREE_CONFIG                 // path to YAML config
- S3FREE_ADDR                   // data-plane listen address (e.g., 0.0.0.0:8080)
- S3FREE_ADMIN_ADDR             // admin-plane listen address (e.g., 0.0.0.0:9090) to enable admin endpoints
- S3FREE_DATA_DIRS              // comma-separated data directories
- S3FREE_AUTH_MODE              // "none" (default) or "sigv4"
- S3FREE_ACCESS_KEYS            // comma-separated ACCESS_KEY:SECRET_KEY[:USER]
- S3FREE_TRACING_ENABLED        // "true"/"false"
- S3FREE_TRACING_ENDPOINT       // e.g., localhost:4317 (grpc) or localhost:4318 (http)
- S3FREE_TRACING_PROTOCOL       // "grpc" or "http"
- S3FREE_TRACING_SAMPLE         // 0.0 - 1.0
- S3FREE_TRACING_SERVICE        // service.name override
- S3FREE_GC_ENABLED             // "true"/"false" to enable multipart GC
- S3FREE_GC_INTERVAL            // e.g., "15m"
- S3FREE_GC_OLDER_THAN          // e.g., "24h"
- S3FREE_OIDC_ENABLED           // "true"/"false" to protect Admin API with OIDC
- S3FREE_OIDC_ISSUER            // issuer URL for discovery (preferred)
- S3FREE_OIDC_CLIENT_ID         // expected client_id (audience)
- S3FREE_OIDC_AUDIENCE          // optional, overrides client_id
- S3FREE_OIDC_JWKS_URL          // direct JWKS URL alternative to issuer
- S3FREE_OIDC_ALLOW_UNAUTH_HEALTH   // "true"/"false" to allow unauthenticated /admin/health
- S3FREE_OIDC_ALLOW_UNAUTH_VERSION  // "true"/"false" to allow unauthenticated /admin/version
</pre>

## Authentication (optional SigV4)
- Disabled by default. Enable verification and provide credentials either via config or environment:
```bash
export S3FREE_AUTH_MODE=sigv4
export S3FREE_ACCESS_KEYS='AKIAEXAMPLE:secret:local'
# Run server after setting env
S3FREE_CONFIG=configs/local.yaml make run
```
When enabled, the server requires valid AWS Signature V4 on S3 requests (both Authorization header and presigned URLs are supported). Health endpoints (/livez, /readyz, /metrics) remain unauthenticated.

## Notes & limitations (current MVP)
- Authentication: optional. AWS SigV4 supported (header and presigned; disabled by default via config/env).
- ETag is MD5 of full object for single-part PUTs
- Objects stored under ./data/objects/{bucket}/{key}
- Multipart temporary parts stored in separate staging bucket: .multipart/<bucket>/<object-key>/<uploadId>/part.N (excluded from user listings and bucket empty checks; cleaned up on complete/abort)
- Range requests require seekable storage (LocalFS supports this)

## Recent Improvements (2025-10-29)
- Implemented AWS SigV4 authentication verification (headers and presigned) with unit tests
- Exposed Prometheus metrics at /metrics and added HTTP instrumentation middleware
- Added liveness (/livez) and readiness (/readyz) endpoints; readiness gated after initialization
- Fixed critical memory issues: streaming multipart completion; safe handling for non-seekable Range GET
- Hid internal multipart files from listings and bucket-empty checks; normalized temp part layout

## Roadmap (short)
1) FreeXL v1 storage format + erasure coding
2) Background scrubber and self-healing
3) Admin API hardening (OIDC/RBAC), monitoring assets (dashboards/alerts)

## License
Apache-2.0

## Contributing
Early-stage project — issues and PRs welcome. Please keep code documented and tested.
