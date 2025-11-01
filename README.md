
<img src="img/shardSeal_280x280.png" width="280">

# ShardSeal - Open S3-compatible, self-healing object store written in Go.
(Work in progress)

## Project Status & Goals

### Current State
This is an experimental project in early development, primarily designed for:
- Understanding distributed storage system internals
- Testing novel approaches to erasure coding and data placement algorithms
- Learning S3 protocol implementation details
- Experimenting with self-healing storage architectures

**This is NOT production-ready software.**

_______________

- Implemented
  - S3 basics: ListBuckets (/), CreateBucket (PUT /{bucket}), DeleteBucket (DELETE /{bucket})
  - Objects: Put (PUT /{bucket}/{key}), Get (GET), Head (HEAD), Delete (DELETE)
  - Range GET support (single range, requires seekable storage)
  - ListObjectsV2 (bucket object listing with prefix, pagination)
  - Multipart uploads (initiate/upload-part/complete/abort)
  - Config (YAML + env), structured logging, CI
  - Prometheus metrics (/metrics) and HTTP instrumentation
  - Tracing: OpenTelemetry scaffold (optional; OTLP gRPC/HTTP); spans include s3.error_code; optional s3.key_hash via config
  - Authentication: AWS Signature V4 (optional; header and presigned URL)
  - Local filesystem storage backend (dev/MVP), in-memory metadata store
  - Admin API (optional, separate port) with optional OIDC + RBAC: /admin/health, /admin/version; multipart GC endpoint (/admin/gc/multipart)
  - Unit tests for buckets/objects/multipart
  - **Production-ready fixes:** Streaming multipart completion, safe range handling, improved error logging
- Not yet implemented / in progress
  - Self-healing (erasure coding and background rewriter): verification-only scrubber implemented (no healing yet); sealed I/O and integrity verification are available behind feature flags.
  - Distributed metadata/placement

## Quick start
### Prerequisites
- Go 1.22+ installed

### Build and run
```bash
make build
# Run with sample config (will ensure ./data exists)
SHARDSEAL_CONFIG=configs/local.yaml make run
# Or
# go run ./cmd/shardseal
```

Default address: :8080 (override with env SHARDSEAL_ADDR). Data dirs: ./data (override with env SHARDSEAL_DATA_DIRS as comma-separated list).

### Using with curl (auth disabled by default; SigV4 optional)
Bucket naming: 3-63 chars; lowercase letters, digits, dots, hyphens; must start/end with letter or digit.

```bash
# List all buckets
curl -v http://localhost:8080/

# Create a bucket
curl -v -X PUT http://localhost:8080/my-bucket

# Put an object (from stdin)
printf 'Hello, ShardSeal!\n' | curl -v -X PUT http://localhost:8080/my-bucket/hello.txt --data-binary @-

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

## Docker (dev)
Two options are provided: local Docker build and docker-compose. The image exposes:
- 8080: S3 data-plane (configurable via SHARDSEAL_ADDR)
- 9090: Admin API (when adminAddress is configured)

Build and run (Dockerfile)
```bash
# Build the image locally
docker build -t shardseal:dev .

# Run with a mounted data directory and config
# Ensure your config mounts to /home/app/config/config.yaml or set SHARDSEAL_CONFIG accordingly.
docker run --rm -p 8080:8080 -p 9090:9090 \
  -v "$(pwd)/data:/home/app/data" \
  -v "$(pwd)/configs:/home/app/config:ro" \
  -e SHARDSEAL_CONFIG=/home/app/config/config.yaml \
  --name shardseal shardseal:dev
```

Compose (docker-compose.yml)
```bash
# Up/Down
docker compose up --build
docker compose down

# Override env from your shell or edit docker-compose.yml as needed.
# Data is mounted at ./data, config at ./configs (read-only) by default.
```

Notes:
- The container user is a non-root user (app). Data and config are mounted under /home/app.
- To enable Admin API, configure adminAddress in the config or set SHARDSEAL_ADMIN_ADDR (see [configs/local.yaml](configs/local.yaml:1) and [cmd.shardseal.main](cmd/shardseal/main.go:1)).
- Sealed mode can be enabled via:
  - YAML: sealed.enabled: true
  - Env: SHARDSEAL_SEALED_ENABLED=true
- Integrity scrubber (experimental verification-only) can be enabled via:
  - Env: SHARDSEAL_SCRUBBER_ENABLED=true
  - Optional overrides:
    - SHARDSEAL_SCRUBBER_INTERVAL=1h
    - SHARDSEAL_SCRUBBER_CONCURRENCY=2
    - SHARDSEAL_SCRUBBER_VERIFY_PAYLOAD=true  # overrides sealed.verifyOnRead inheritance
- Admin scrub endpoints (experimental, sealed integrity verification):
  - GET /admin/scrub/stats (RBAC: admin.read)
  - POST /admin/scrub/runonce (RBAC: admin.scrub)
  - The scrubber verifies sealed headers/footers and compares footer content-hash to the manifest. Payload re-hash verification is enabled when sealed.verifyOnRead is true (or forced via SHARDSEAL_SCRUBBER_VERIFY_PAYLOAD). Protect these with OIDC/RBAC as needed (see [security.oidc.rbac](pkg/security/oidc/rbac.go:1) and [cmd.shardseal.main](cmd/shardseal/main.go:1)).
- The provided [docker-compose.yml](docker-compose.yml:1) includes commented environment toggles for sealed mode, scrubber, tracing, admin OIDC, and GC; uncomment to enable as needed.

## Metrics
- Exposes Prometheus metrics at /metrics on the same HTTP server.
- Default counters and histograms include:
  - shardseal_http_requests_total{method,code}
  - shardseal_http_request_duration_seconds_bucket/sum/count{method,code}
  - shardseal_http_inflight_requests
  - shardseal_storage_bytes_total{op}
  - shardseal_storage_ops_total{op,result}
  - shardseal_storage_op_duration_seconds_bucket/sum/count{op}
  - shardseal_storage_sealed_ops_total{op,sealed,result,integrity_fail}
  - shardseal_storage_sealed_op_duration_seconds_bucket/sum/count{op,sealed,integrity_fail}
  - shardseal_storage_integrity_failures_total{op}
  - shardseal_scrubber_scanned_total
  - shardseal_scrubber_errors_total
  - shardseal_scrubber_last_run_timestamp_seconds
  - shardseal_scrubber_uptime_seconds
  - shardseal_repair_queue_depth
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
- Grafana dashboard (import JSON): configs/monitoring/grafana/shardseal_overview.json
- Includes sealed I/O metrics, scrubber metrics (scanned/errors/last_run/uptime), and repair metrics (queue_depth). The server polls scrubber stats and repair queue length every 10s and exports to the main registry.

Quick start:
```bash
# 1) Run shardseal (default :8080 exposes /metrics)
SHARDSEAL_CONFIG=configs/local.yaml make run

# 2) Start Prometheus (adjust path as needed)
prometheus --config.file=configs/monitoring/prometheus/prometheus.yml

# 3) Import Grafana dashboard JSON:
#    configs/monitoring/grafana/shardseal_overview.json
#    and set the Prometheus datasource accordingly.
```

Compose profile (optional monitoring stack):
```bash
# Bring up shardseal as usual (uses service 'shardseal')
docker compose up --build -d

# Bring up monitoring stack (Prometheus + Grafana) using the 'monitoring' profile
docker compose --profile monitoring up -d

# Access:
# - ShardSeal (S3 plane): http://localhost:8080
# - ShardSeal Admin (if enabled): http://localhost:9090
# - Prometheus: http://localhost:9091
# - Grafana: http://localhost:3000  (default admin/admin)
#   Add Prometheus data source at http://prometheus:9090 and import the dashboard:
#   configs/monitoring/grafana/shardseal_overview.json
```

Tracing and S3 error headers
- Server spans include: http.method, http.target, http.route, http.status_code, user_agent.original, net.peer.ip, http.server_duration_ms.
- S3 attributes (low cardinality): s3.op, s3.bucket_present, s3.admin, s3.error. New: s3.error_code on failures; optional s3.key_hash when enabled.
- Enable s3.key_hash via config (tracing.keyHashEnabled: true) or env (SHARDSEAL_TRACING_KEY_HASH=true). The key hash is sha256(key) truncated to 8 bytes (16 hex chars).
- Error responses include the header X-S3-Error-Code mirroring the S3 error code for observability. This header is only set on error responses.



Admin endpoints (optional; if admin server enabled). If OIDC is enabled, these endpoints require a valid Bearer token. RBAC defaults are enforced:
- admin.read for GET endpoints
- admin.gc for POST /admin/gc/multipart
- admin.scrub for POST /admin/scrub/runonce
- admin.repair.read for GET /admin/repair/stats
- admin.repair.enqueue for POST /admin/repair/enqueue
- admin.repair.control for POST /admin/repair/worker/pause and /admin/repair/worker/resume

- /admin/health: JSON status with ready/version/addresses
- /admin/version: JSON version info
- POST /admin/gc/multipart: run a single multipart GC pass (requires RBAC admin.gc; OIDC-protected if enabled)
- /admin/scrub/stats: get current scrubber stats (requires RBAC admin.read)
- POST /admin/scrub/runonce: trigger a single scrub pass (requires RBAC admin.scrub)
- /admin/repair/stats: current repair queue length (requires RBAC admin.repair.read)
- POST /admin/repair/enqueue: enqueue a repair item (requires RBAC admin.repair.enqueue). Body JSON accepts RepairItem fields {bucket, key, shardPath, reason, priority}; discovered timestamp is auto-populated when omitted. The queue is in-memory in this release.
- /admin/repair/worker/stats: repair worker status and counters (requires RBAC admin.repair.read)
- POST /admin/repair/worker/pause: pause the repair worker (requires RBAC admin.repair.control)
- POST /admin/repair/worker/resume: resume the repair worker (requires RBAC admin.repair.control)

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
#   serviceName: "shardseal"
#   keyHashEnabled: false      # emit s3.key_hash; or set SHARDSEAL_TRACING_KEY_HASH=true
#
# Sealed mode (experimental)
# sealed:
#   enabled: false
#   verifyOnRead: false
#
# Integrity Scrubber (experimental - verification only)
# Verifies sealed header/footer CRCs and compares footer content-hash with manifest.
# Payload re-hash verification follows sealed.verifyOnRead (enabled when true).
# scrubber:
#   enabled: false
#   interval: "1h"
#   concurrency: 1
```

Additional optional request size limits:
```yaml
# Request size limits (optional)
limits:
  singlePutMaxBytes: 5368709120    # 5 GiB cap for single PUT
  minMultipartPartSize: 5242880    # 5 MiB minimum for non-final multipart parts
```
<pre>
Environment overrides:
- SHARDSEAL_CONFIG                 // path to YAML config
- SHARDSEAL_ADDR                   // data-plane listen address (e.g., 0.0.0.0:8080)
- SHARDSEAL_ADMIN_ADDR             // admin-plane listen address (e.g., 0.0.0.0:9090) to enable admin endpoints
- SHARDSEAL_DATA_DIRS              // comma-separated data directories
- SHARDSEAL_AUTH_MODE              // "none" (default) or "sigv4"
- SHARDSEAL_ACCESS_KEYS            // comma-separated ACCESS_KEY:SECRET_KEY[:USER]
- SHARDSEAL_TRACING_ENABLED        // "true"/"false"
- SHARDSEAL_TRACING_ENDPOINT       // e.g., localhost:4317 (grpc) or localhost:4318 (http)
- SHARDSEAL_TRACING_PROTOCOL       // "grpc" or "http"
- SHARDSEAL_TRACING_SAMPLE         // 0.0 - 1.0
- SHARDSEAL_TRACING_SERVICE        // service.name override
- SHARDSEAL_TRACING_KEY_HASH       // "true"/"false"; when true, emit s3.key_hash (sha256 first 8 bytes hex of object key)
- SHARDSEAL_SEALED_ENABLED         // "true"/"false" to store objects using sealed format (experimental)
- SHARDSEAL_SEALED_VERIFY_ON_READ  // "true"/"false" to verify integrity on GET/HEAD
- SHARDSEAL_SCRUBBER_ENABLED       // "true"/"false" to enable background scrubber
- SHARDSEAL_SCRUBBER_INTERVAL      // e.g., "1h"
- SHARDSEAL_SCRUBBER_CONCURRENCY   // e.g., "2"
- SHARDSEAL_SCRUBBER_VERIFY_PAYLOAD // "true"/"false" to force payload re-hash verification (overrides sealed.verifyOnRead inheritance)
- SHARDSEAL_GC_ENABLED             // "true"/"false" to enable multipart GC
- SHARDSEAL_GC_INTERVAL            // e.g., "15m"
- SHARDSEAL_GC_OLDER_THAN          // e.g., "24h"
- SHARDSEAL_OIDC_ENABLED           // "true"/"false" to protect Admin API with OIDC
- SHARDSEAL_OIDC_ISSUER            // issuer URL for discovery (preferred)
- SHARDSEAL_OIDC_CLIENT_ID         // expected client_id (audience)
- SHARDSEAL_OIDC_AUDIENCE          // optional, overrides client_id
- SHARDSEAL_OIDC_JWKS_URL          // direct JWKS URL alternative to issuer
- SHARDSEAL_OIDC_ALLOW_UNAUTH_HEALTH   // "true"/"false" to allow unauthenticated /admin/health
- SHARDSEAL_OIDC_ALLOW_UNAUTH_VERSION  // "true"/"false" to allow unauthenticated /admin/version
- SHARDSEAL_LIMIT_SINGLE_PUT_MAX_BYTES     // e.g., 5368709120 (5 GiB)
- SHARDSEAL_LIMIT_MIN_MULTIPART_PART_SIZE  // e.g., 5242880 (5 MiB)
</pre>

## Sealed mode (experimental)

Summary
- When enabled, objects are stored as sealed shard files with a header | payload | footer encoding and a JSON manifest persisted alongside per-object metadata. The S3 API remains unchanged (ETag is still MD5 of the payload; SigV4 works the same).
- Range GETs are served by seeking past the header and reading a SectionReader over just the payload. See [storage.localfs](pkg/storage/localfs.go:1) and [storage.manifest](pkg/storage/manifest.go:1).

On-disk layout
- Object directory: ./data/objects/{bucket}/{key}/
- Data file: data.ss1
  - Header (little-endian): magic "ShardSealv1" | version:u16 | headerSize:u16 | payloadLen:u64 | headerCRC32C:u32
  - Footer: contentHash[32] (sha256 of payload) | footerCRC32C:u32
  - Format primitives implemented in [erasure.rs](pkg/erasure/rs.go:1) with unit tests in [erasure.rs_test](pkg/erasure/rs_test.go:1).
- Manifest: object.meta (JSON, v1)
  - Records bucket, key, size, ETag (MD5), lastModified, RS params, and a Shards[] slice with path, content hash algo/hex, payload length, header/footer CRCs.

Behavior
- GET/HEAD prefer sealed objects when a manifest exists; otherwise fall back to plain files (mixing sealed and plain is supported).
- Range GETs use io.SectionReader on the payload region (efficient partial reads).
- DELETE removes the sealed shard and the manifest; LIST derives keys from the parent dir of data.ss1 and reads metadata from the manifest. Implementation details in [storage.localfs](pkg/storage/localfs.go:1).

Integrity verification (optional)
- Set sealed.verifyOnRead: true to validate footer CRC and sha256(payload) against the manifest during GET/HEAD.
- Integrity failures are surfaced as 500 InternalError at the S3 layer and annotated in tracing. S3 mapping handled in [api.s3](pkg/api/s3/server.go:1).

Configuration
- YAML (see sample in configs/local.yaml):
  - sealed.enabled: false (default)
  - sealed.verifyOnRead: false (default)
- Environment:
  - SHARDSEAL_SEALED_ENABLED=true|false
  - SHARDSEAL_SEALED_VERIFY_ON_READ=true|false
- Sample config and env wiring in [cmd.shardseal.main](cmd/shardseal/main.go:1).

Observability
- Tracing: storage.sealed=true for sealed ops; storage.integrity_fail=true when verification fails.
- Prometheus (emitted by [obs.metrics.storage](pkg/obs/metrics/storage.go:1)):
  - shardseal_storage_bytes_total{op}
  - shardseal_storage_ops_total{op,result}
  - shardseal_storage_op_duration_seconds_bucket/sum/count{op}
  - shardseal_storage_sealed_ops_total{op,sealed,result,integrity_fail}
  - shardseal_storage_sealed_op_duration_seconds_bucket/sum/count{op,sealed,integrity_fail}
  - shardseal_storage_integrity_failures_total{op}

Migration and compatibility
- Enabling sealed mode affects only newly written objects. Existing plain files remain readable; GET/HEAD fall back to plain when no manifest is present.
- Disabling sealed mode does not delete existing sealed objects; they continue to be served via manifest. You can transition gradually and mix sealed/plain safely.
- ETag policy: MD5 of full object payload is preserved for S3 compatibility (even in sealed mode). For CompleteMultipartUpload, the ETag is MD5 of the final combined object (not AWS multipart-style ETag with a dash and part count). This may become configurable in a future release.

Scrubber behavior
- Performs sealed integrity verification: validates sealed headers/footers and footer content-hash against the manifest; optional payload re-hash when sealed.verifyOnRead is true.
- See the Admin endpoints section above for routes and RBAC.
## Authentication (optional SigV4)
- Disabled by default. Enable verification and provide credentials either via config or environment:
```bash
export SHARDSEAL_AUTH_MODE=sigv4
export SHARDSEAL_ACCESS_KEYS='AKIAEXAMPLE:secret:local'
# Run server after setting env
SHARDSEAL_CONFIG=configs/local.yaml make run
```
When enabled, the server requires valid AWS Signature V4 on S3 requests (both Authorization header and presigned URLs are supported). Health endpoints (/livez, /readyz, /metrics) remain unauthenticated.

## Notes & limitations (current MVP)
- Authentication: optional. AWS SigV4 supported (header and presigned; disabled by default via config/env).
- ETag is MD5 of full object for single-part PUTs; for multipart completes, ETag is also MD5 of the full final object (not AWS multipart-style ETag).
- Objects stored under ./data/objects/{bucket}/{key}
- Multipart temporary parts stored in separate staging bucket: .multipart/<bucket>/<object-key>/<uploadId>/part.N (excluded from user listings and bucket empty checks; cleaned up on complete/abort)
- Range requests require seekable storage (LocalFS supports this)
- Single PUT size cap: 5 GiB (configurable via limits.singlePutMaxBytes or env SHARDSEAL_LIMIT_SINGLE_PUT_MAX_BYTES). Larger uploads must use Multipart Upload (responds with S3 error code EntityTooLarge).
- Error detail: EntityTooLarge responses include MaxAllowedSize and a hint to use Multipart Upload.
- Multipart part size: 5 MiB minimum for all parts except the final part (configurable via limits.minMultipartPartSize or env SHARDSEAL_LIMIT_MIN_MULTIPART_PART_SIZE). Intended for S3 compatibility; very small multi-part aggregates used in tests may bypass this check.
- LocalFS writes are atomic via temp+rename on Put, reducing risk of partial files on error.

## Recent Improvements (2025-10-29)
- Implemented AWS SigV4 authentication verification (headers and presigned) with unit tests
- Exposed Prometheus metrics at /metrics and added HTTP instrumentation middleware
- Added liveness (/livez) and readiness (/readyz) endpoints; readiness gated after initialization
- Fixed critical memory issues: streaming multipart completion; safe handling for non-seekable Range GET
- Hid internal multipart files from listings and bucket-empty checks; normalized temp part layout

## Recent Improvements (2025-10-30)
- Tracing enrichment: error responses now set X-S3-Error-Code; tracing middleware records s3.error_code.
- Optional s3.key_hash attribute on spans (sha256(key) truncated to 8 bytes hex), configurable via tracing.keyHashEnabled or env SHARDSEAL_TRACING_KEY_HASH=true.
- README, sample config, and tests updated accordingly.

## Recent Improvements (2025-10-31)
- ShardSeal v1 sealed mode (experimental, feature-flagged):
  - LocalFS now writes sealed shard files (header | payload | footer) and persists a JSON manifest; Range GETs are served via a SectionReader. Delete/List are aware of sealed layout (see [storage.localfs](pkg/storage/localfs.go:1), [storage.manifest](pkg/storage/manifest.go:1)).
  - Optional verifyOnRead validates footer CRC and sha256(payload) on GET/HEAD; integrity failures are mapped to 500 InternalError at the S3 layer (see [api.s3](pkg/api/s3/server.go:1)).
  - Tests added for storage-level and S3-level sealed behavior including corruption detection (see [storage.localfs_sealed_test](pkg/storage/localfs_sealed_test.go:1), [api.s3.server_sealed_test](pkg/api/s3/server_sealed_test.go:1)).
  - Observability: tracing annotates storage.sealed and storage.integrity_fail; Prometheus sealed I/O metrics added (see [obs.metrics.storage](pkg/obs/metrics/storage.go:1)).

## Recent Improvements (2025-11-01)
- Admin repair control surface:
  - Queue endpoints: GET /admin/repair/stats, POST /admin/repair/enqueue
  - Worker endpoints: GET /admin/repair/worker/stats, POST /admin/repair/worker/pause, POST /admin/repair/worker/resume
  - RBAC roles: admin.repair.read, admin.repair.enqueue, admin.repair.control
- Observability:
  - shardseal_repair_queue_depth metric with periodic polling
  - Prometheus recording rules and alerts for repair queue depth
  - Grafana panels for repair queue depth (stat and timeseries)
- Documentation: Admin endpoints, RBAC roles, and monitoring sections updated to reflect current state.

## Roadmap (short)
1) ShardSeal v1 storage format + erasure coding
2) Background scrubber and self-healing
3) Admin API hardening (OIDC/RBAC), monitoring assets (dashboards/alerts)

## License
AGPL-3.0-or-later

## Contributing
Early-stage experimental project — contributions welcome, especially in areas of:
- Erasure coding implementations
- Distributed systems algorithms
- Storage integrity verification techniques
- Performance optimizations

Please keep code documented and tested. Note that the project structure and APIs may change significantly as the design evolves.
