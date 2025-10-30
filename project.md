# s3free — Open S3-compatible, self-healing object store (Go)

A cross‑platform, open-source object/file storage system offering S3 compatibility with built‑in self-healing, strong data integrity, and corruption recovery—without enterprise gates. Written in Go.

## Vision & Motivation
- Deliver an easy-to-run, production-grade, S3-compatible object store with first-class self-healing and integrity verification built into the storage format.
- Prioritize correctness, simplicity, and operability; scale from a single node with multiple disks to a distributed cluster.
- Take inspiration from proven designs (e.g., MinIO, SeaweedFS) while focusing on transparent, community-first features that are commonly gated in enterprise editions elsewhere.

## Core Tenets
- Integrity by design: layered checksums (chunk/header/footer), Merkle trees, and end-to-end verification.
- Self-healing by default: detect and repair on reads and via background scrubbing.
- Predictable durability: erasure coding with clear failure-domain placement and repair semantics.
- S3-first UX: pragmatic API coverage and compatibility, including multipart, presigned URLs, and bucket policies.
- Operational clarity: great metrics, logs, tracing, clear failure modes, and simple config.

## Target Use Cases
- On-prem or edge S3 storage with commodity disks.
- Backup/archival with strong integrity guarantees and bitrot protection.
- Analytics/data lake staging where self-healing reduces operational toil.
- Developer-friendly single-binary for local testing that upgrades to clustered deployments.

## Non-Goals (initially)
- Full POSIX/NFS semantics.
- Strong multi-region transactions (we’ll support async geo-replication first).
- Object transforms/pipelines (focus on storage core; events can integrate later).

## Feature Set
- MVP
  - S3 API: CreateBucket, PutObject (incl. multipart), GetObject (range), Head, Delete, List V2
  - Authentication: AWS Signature V4; optional static users/keys
  - Storage: local disks/paths, erasure-coded stripes, per-chunk checksums
  - Integrity: bitrot detection, read-time repair, object-level Merkle root
  - Metadata: strongly consistent (single-node: local), version IDs (UUIDv7), ETag compatibility
  - Observability: Prometheus metrics, structured logs, health endpoints
- P1
  - Distributed clustering: embedded Raft for metadata (no external deps), consistent hashing/placement
  - Background scrubbing & automatic rewrite of bad/missing shards
  - Server-side encryption (SSE-S3) with envelope keys; SSE-C
  - Bucket policies/ACLs, lifecycle (expire/transition stubs)
  - Object versioning, soft delete, undelete
- P2
  - KMS plugin interface (Vault, cloud KMS)
  - Cross-cluster replication (asynchronous), selective bucket replication
  - WORM/Object Lock (governance/compliance), legal hold
  - Tiering (remote S3, filesystem, glacier-like backends), compression
- P3
  - Events/notifications (AMQP/Kafka/Webhooks), audit logs
  - Deduplication (content-addressable option), server-side copy optimization
  - Advanced placement policies (rack/zone), CRUSH-like hints

## Architecture Overview
- API Gateway (S3 layer): HTTP server, SigV4 auth, request routing, multipart orchestration.
- Metadata Service: authoritative object metadata, bucket configs, multipart state; Raft for consensus in clusters; pluggable local KV (Pebble/Badger) for single-node.
- Object Store (Chunk/Shard Layer): writes data in erasure-coded stripes across disks/nodes; verifies checksums and seals shards with headers/footers.
- Erasure Engine: Reed–Solomon (RS k+m), vectorized parity; configurable stripe size and block size.
- Healer & Scrubber: on-read repair; background scanners that detect bitrot, missing shards, and drift; prioritized repair queues.
- Placement: consistent hashing ring with virtual nodes; awareness of failure domains (disk/node/rack/zone) when available.
- Security: envelope encryption, per-object data keys, KMS plugins, SSE-C support.
- Observability: Prometheus, OpenTelemetry traces, debug profiles.

## Consistency & Durability Model
- Write Path (simplified):
  1) Client initiates PUT; data is chunked into fixed-size blocks; RS(k,m) shards produced per block.
  2) Shards written to placement targets; per-shard headers/footers include checksums and identity.
  3) Metadata transaction prepared (object manifest with shard map); on clusters, Raft commits before client ACK.
  4) ACK after quorum of shard writes and metadata commit. Quorum defaults: >=k shards per block and Raft majority.
- Read Path:
  - Locate object manifest; fetch any k good shards per block; verify shard checksums and object Merkle root.
  - If corruption/missing detected, reconstruct and optionally rewrite repaired shards (self-healing on read).
- Failure Model:
  - Tolerate up to m shard losses per stripe without data loss.
  - Background scrubbing constantly looks for latent corruption and heals proactively.

## Storage Format: FreeXL v1 (self-healing by design)
Goals
- Recoverability from partial state (missing metadata or data) using self-describing shards.
- Fast integrity checks and precise localization of corruption.
- Minimal small-file overhead while supporting multipart and large objects efficiently.

On-Disk Layout (per bucket/object/version)
- object directory: sharded by hash to avoid hot directories (e.g., /data/aa/bb/…)
- manifest file: object.meta (protobuf or JSON v1; protobuf preferred later)
- data shards: per-part, per-block shard files (or extented segments within larger files for compaction)

Per-Shard Header (fixed-size, at file start)
- Magic (FreeXLv1), format_version
- object_id, bucket_id, version_id, part_id
- stripe_id, block_index, shard_index, rs_k, rs_m
- content_hash (BLAKE3 of plaintext block)
- checksum (CRC32C) over header+payload; header includes payload length
- enc_info (algo, key_id, nonce) if encrypted

Per-Shard Footer (fixed-size, at file end)
- Merkle_leaf (sha256/blake3) and footer checksum (CRC32C)
- Sequence/gen counter to deduce “latest” during metadata-loss recovery

Object Manifest (object.meta)
- bucket, key, version_id, size, content_type, etag, last_modified, user_metadata
- parts: [index, size, etag, block_count]
- rs_params: k, m, block_size, stripe_size
- placement: list of shard -> node_id/disk_id, generation
- encryption: key_id, algorithm (if SSE-S3/SSE-C)
- integrity: object_merkle_root, hash_algo

Recovery Flows
- Missing manifest: scan shard headers for object_id/version_id; pick highest generation; reconstruct manifest.
- Missing shards: use RS to rebuild from surviving shards; verify via header/footer and object Merkle root; rewrite with a new generation.
- Corrupted shards: detect via CRC32C mismatch or Merkle mismatch; invalidate and repair.

Compaction & GC
- Periodically coalesce small shard files; rewrite with new headers/footers and generations.
- GC aborted multipart states and unreferenced shards by manifest reachability scans.

## Cluster Membership & Placement
- Discovery: static peers or join via seed node.
- Metadata: embedded Raft group (3–5 nodes recommended) for buckets/manifests/multipart state.
- Placement: consistent hashing ring (vnodes) per disk; ensure stripes span distinct failure domains.
- Rebalancing: smooth migrations when nodes/disks added/removed; throttle copy; maintain durability before removal.

## Security
- Auth: AWS SigV4; optional OIDC for users/groups mapping to S3 credentials.
- Encryption: SSE-S3 (AES-GCM envelope; per-object data key wrapped by KEK); SSE-C (client-provided key, never persisted).
- KMS: plugin interface for Vault/cloud KMS; key rotation policy; rewrap workflows.
- Auditing: structured access logs; tamper-evident hashing chain (later phase).

## Observability & Ops
- Metrics: request latency, error rates, healing queue depth, RS rebuild rates, bitrot events, disk health, Raft state.
- Tracing: per-request spans (S3 -> metadata -> storage -> RS -> network).
- Health: /readyz, /livez; disk/node health checks; degraded-mode flags.
- Admin APIs/CLI: disk add/remove, rebalance progress, scrub status, repair controls, snapshot/backup of metadata.

## S3 Compatibility Scope
- Buckets: create/delete, list, policies (subset initially), tags.
- Objects: single & multipart upload, range reads, copy, presigned URLs.
- Versioning: optional; delete markers.
- ETags: MD5 for single-part; multipart ETag compatibility (documented behavior when encryption/transform is used).
- Error codes and semantics aligned with S3 where practical; documented deviations.

## CLI & Configuration (high level)
- Single binary: `s3free` with subcommands `server`, `disk`, `cluster`, `admin`, `bench`.
- Config file (YAML) + env overrides (address, dataDirs, authMode, accessKeys); hot-reload for select settings.
- Examples: local single-node with N disks (paths), distributed join via peer list.

## Development Plan & Roadmap
- Phase 0: Repo bootstrap
  - Modules, CI, lint/typecheck, unit test scaffold, basic docs, sample configs.
  - Choose local KV (Pebble) and checksum/hash libs (CRC32C, BLAKE3).
- Phase 1: Single-node MVP
  - Local disks, RS(k=6,m=3) default, chunking, headers/footers, per-object manifest.
  - S3: buckets, put/get/head/delete/list, multipart; SigV4; Prometheus metrics.
  - Read-time repair; basic scrubber; config + CLI.
- Phase 2: Distributed metadata + placement
  - Raft cluster; consistent hashing; replication of manifests; node/disk discovery.
  - Quorum writes/reads; background healing across nodes; rebalance; failure-domain awareness.
  - SSE-S3; bucket policies (subset); versioning.
- Phase 3: Operational hardening
  - Chaos testing, fuzzing, scrub scheduling, repair prioritization; compaction/GC.
  - Admin APIs; audit logging; OIDC integration; lifecycle basics.
- Phase 4: Data mobility & compliance
  - Cross-cluster replication; SSE-C; KMS plugins; Object Lock; tiering/compression.

## Testing Strategy
- Unit tests for S3 handlers, RS math, checksum paths, manifest logic.
- Property-based tests for encode/decode, shard corruption, partial writes, recovery.
- Fuzzing of manifest parser and shard headers.
- Integration tests: S3 compatibility (awscli/minio client), multipart, presigned URLs.
- Chaos: fault injection (I/O errors, random corruption), node loss, delayed disks; verify durability/integrity.
- Load: concurrency/latency SLOs, repair under load, disk thrash behavior, cold vs warm cache.

## Performance Goals (initial)
- Line-rate throughput on 10GbE for large sequential PUT/GET with k=6,m=3 on modern CPUs.
- Latency p99: single-digit milliseconds for small GETs on warm cache; bounded degradation during repairs.
- Efficient small-object handling via coalescing/packing and minimal metadata I/O.

## Risks & Mitigations
- Write amplification from headers/footers and compaction: mitigate with aligned block sizes and batching.
- Raft hotspots: separate metadata from data path; tune snapshotting; shard metadata if needed.
- Operational complexity: sane defaults, auto-tuning, rich diagnostics, and clear docs.
- Compatibility edge cases (multipart ETag, range on encrypted objects): document and test early.

## Repository Layout (proposal)
- cmd/s3free: main entrypoint
- pkg/api/s3: S3 HTTP handlers, SigV4
- pkg/metadata: Raft, manifests, bucket state, multipart
- pkg/storage: disk I/O, shard files, scrubber, compaction
- pkg/erasure: RS codec and SIMD optimizations
- pkg/placement: ring, failure domains, rebalancer
- pkg/security: SSE/SSE-C, envelope keys, KMS plugins
- pkg/repair: healing queues, background workers
- pkg/obs: metrics, tracing, logging
- internal/testutil: integration harness, fault injection
- docs/: specs, ops guides, API notes

## Licensing & Governance
- License: Apache-2.0 (permissive, community-friendly).
- Governance: maintainers + reviewers model; RFC process for format changes; versioned storage spec with migration tooling.

## Glossary
- RS(k,m): Reed–Solomon with k data and m parity shards per stripe.
- Stripe/Block: a group of k+m shards derived from a block of data; minimum k shards needed to reconstruct.
- Manifest: per-object metadata describing layout, placement, and integrity.
- Self-healing: automatic detection and repair of missing/corrupted shards during reads or background scans.

## Immediate Next Steps (MVP)
1) Scaffold repo, CI, linting; choose libs (Pebble, BLAKE3, RS codec).
2) Implement local single-node storage with FreeXL v1 headers/footers and manifest.
3) S3 basic endpoints + multipart; SigV4; metrics.
4) Read-time repair; initial scrubber; docs and examples.

## Current Status & Progress (Updated 2025-10-30)

### Completed ✅
- Project scaffold: modules, Makefile, CI
- Config loader (YAML + env overrides) and data-dir creation
- Structured logging with slog
- S3 basics implemented and tested:
  - CreateBucket, DeleteBucket, ListBuckets
  - PutObject, GetObject (with Range), HeadObject, DeleteObject
  - ListObjectsV2 (prefix, max-keys, continuation-token)
  - Multipart Upload (initiate/upload-part/complete/abort)
- In-memory metadata store (buckets + multipart uploads)
- Local filesystem ObjectStore (dev/MVP)
- Unit tests for buckets/objects/multipart; added tests for LocalFS multipart visibility and emptiness
- Internal multipart files are hidden from listings and empty checks; normalized part layout to ".multipart/<key>/<uploadId>/part.N"
- Config extended with authMode and accessKeys; env overrides via S3FREE_AUTH_MODE and S3FREE_ACCESS_KEYS
- AWS SigV4 authentication implemented (header and presigned) with unit tests
- Prometheus metrics exposed at /metrics; HTTP request instrumentation added
- Readiness improvements: /readyz now reflects dependency readiness (config loaded, storage initialized, metrics registered)
- FreeXL v1 scaffolding: erasure Params/Codec interfaces with NoopCodec; manifest model and in-memory ManifestStore
- Background scrubber scaffold: Scrubber interface and NoopScrubber with start/stop/runOnce and counters; unit tests added
- OpenTelemetry tracing scaffold (optional; OTLP gRPC/HTTP) and HTTP tracing middleware wired
- Storage I/O metrics for LocalFS (bytes, ops, latency) exposed via Prometheus
- Admin API skeleton on separate port with read-only endpoints: /admin/health and /admin/version
- Multipart temporary parts moved to internal staging bucket ".multipart" (outside user buckets)
- Monitoring assets: Prometheus scrape config and alert rules; Grafana overview dashboard JSON
- **Critical fixes applied (2025-10-27):**
  - Fixed CompleteMultipartUpload to use streaming (prevents memory exhaustion)
  - Fixed Range GET fallback (now returns 501 instead of loading entire file)
  - Improved error handling and logging for metadata consistency
  - Added constants for S3 error codes and query parameters
  - Documented concurrency safety requirements for Store and ObjectStore interfaces
  - Fixed bucket deletion to exclude .multipart temporary files from empty check
- Admin API hardening: OIDC authentication with optional exemptions (health/version) and RBAC default policy (admin.read, admin.gc) wired
- Multipart lifecycle hardening: on-demand GC endpoint and background GC with thresholds; guard bucket delete during active uploads
- Admin package: factored multipart GC into pkg/admin with reusable RunMultipartGC and NewMultipartGCHandler
- Tests added: OIDC middleware and RBAC unit tests; admin GC unit tests; go build/test passing

### Next Up 🚧
1) FreeXL v1 implementation: encoding path (headers/footers), checksum verification, manifest writer/reader
2) Tracing guidance: sampling defaults and span attributes for storage operations
3) Monitoring documentation and additional dashboards; production Alertmanager integration examples
4) Admin UI/CLI planning: role-aware pages/commands and secure transport; basic read-only views
5) Distributed metadata/placement: design notes for embedded Raft and consistent hashing ring

## Development Guide

### Building & Running Tests

```bash
make build
go test ./...
```

### Running the Server

```bash
# Using sample config
S3FREE_CONFIG=configs/local.yaml make run
# Or
go run ./cmd/s3free
```

### Testing with curl (current features)

```bash
# List all buckets
curl -v http://localhost:8080/

# Create a bucket (3-63 chars, lowercase/digits/dot/hyphen)
curl -v -X PUT http://localhost:8080/my-bucket

# Put object from stdin
printf 'Hello, s3free!\n' | curl -v -X PUT http://localhost:8080/my-bucket/hello.txt --data-binary @-

# Get object
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

### Project Structure

```
s3free/
├── cmd/s3free/           # Main entry point
├── pkg/
│   ├── api/s3/         # S3 HTTP handlers, routing
│   ├── metadata/       # Bucket/object metadata store
│   ├── storage/        # Object storage layer (LocalFS, erasure coding)
│   ├── erasure/        # Reed-Solomon erasure coding
│   ├── config/         # Configuration management
│   ├── obs/            # Observability (metrics, tracing)
│   ├── placement/      # Consistent hashing, placement
│   ├── repair/         # Self-healing, scrubbing
│   └── security/       # Authentication, encryption
├── internal/testutil/  # Test utilities
├── configs/            # Sample configurations
└── data/               # Default data directory (local dev)
```



Review, and needed fixes:
This review will focus on a few key areas that could be improved or might pose problems as the project grows. I won't focus on minor style nits, as the code is already very readable.

### High-Level Architectural Observations

*   **Excellent Structure:** The dependency injection (`New(store, objs)`) is a great pattern. The main `route` function is a clean entry point that correctly dispatches requests to more specific handlers.
*   **Clear S3 Logic:** The implementation of core S3 operations (ListBuckets, Put/Get/Head/Delete Object, ListObjectsV2) correctly follows the S3 REST API conventions (path parsing, HTTP methods, query parameters).
*   **Good Readability:** The code is idiomatic Go. Function and variable names are clear, and the logic is straightforward.

### Potential Issues and "Huge Mistakes" to Avoid

Here are some areas that stand out as potential problems, ranging from correctness bugs to significant performance issues.

#### 1. Inefficient Range GET Fallback (Potential for High Memory Usage)

In `handleObject` -> `http.MethodGet`:
```go
// Fallback: read all and slice (inefficient, test-only)
b, _ := io.ReadAll(rc)
// ...
_, _ = w.Write(b[start : end+1])
```
You've correctly identified this as inefficient, but it's worth highlighting how dangerous this can be. If a user requests a small byte range from a multi-gigabyte (or terabyte) object, your server will **read the entire object into memory** before sending a small slice to the client. This will exhaust server memory and crash the application.

*   **Recommendation:** The `io.ReadSeeker` check is the correct primary path. However, the `objectStore` interface should guarantee that the returned `io.ReadCloser` also implements `io.ReadSeeker` if range requests are to be supported efficiently. The fallback should probably be removed or return a `501 Not Implemented` error to prevent a catastrophic memory allocation.

#### 2. Inefficient `CompleteMultipartUpload` (Major Scalability Bottleneck)

In `handleCompleteMultipartUpload`:
```go
// Combine parts into final object (simplified: concatenate)
// In production, this would stream parts in order
var combinedData []byte
for i := 1; i <= len(upload.Parts); i++ {
    // ...
    rc, _, _, _, err := s.objs.Get(ctx, bucket, partKey)
    // ...
    data, _ := io.ReadAll(rc)
    rc.Close()
    combinedData = append(combinedData, data...)
}
// Write the final object
etag, _, err := s.objs.Put(ctx, bucket, key, strings.NewReader(string(combinedData)))
```
This is the most critical issue in the file. Much like the Range GET fallback, this implementation reads **all parts of a multipart upload into a single memory buffer** (`combinedData`) before writing the final object. A user uploading a 100GB file would require your server to have over 100GB of RAM just to complete the upload.

*   **Recommendation:** This process must be implemented as a streaming operation. The `objectStore.Put` method should ideally accept an `io.Reader`. You can then create an `io.MultiReader` that wraps the readers for each part in sequence. This way, you read from each part file and write to the final object file in chunks, with minimal memory overhead.

    ```go
    // Conceptual Example
    var partReaders []io.Reader
    var partClosers []io.ReadCloser // To close them later
    for i := 1; i <= len(upload.Parts); i++ {
        partKey := // ...
        rc, _, _, _, err := s.objs.Get(ctx, bucket, partKey)
        // handle error
        partReaders = append(partReaders, rc)
        partClosers = append(partClosers, rc)
    }
    defer func() {
        for _, closer := range partClosers {
            closer.Close()
        }
    }()

    combinedReader := io.MultiReader(partReaders...)
    etag, _, err := s.objs.Put(ctx, bucket, key, combinedReader)
    ```

#### 3. Error Handling and Ignored Errors

There are numerous places where errors are ignored with `_ = ...`. While sometimes acceptable for closing operations in `defer`, it can hide bugs in critical paths.

*   `_ = xml.NewEncoder(w).Encode(res)` in `handleListBuckets` and elsewhere. If the client closes the connection midway, this will return an error. While you can't do much about it, logging it can be useful for debugging.
*   `_ = s.store.DeleteBucket(ctx, name)` in `handleDeleteBucket`. What if `s.objs.RemoveBucket` succeeds but `s.store.DeleteBucket` fails? You now have an orphaned bucket on the filesystem that doesn't exist in your metadata. This is a consistency issue. The operation should be atomic or have a rollback mechanism.

*   **Recommendation:** At a minimum, log these ignored errors. For critical metadata operations, consider how to handle partial failures to prevent state inconsistency.

#### 4. Concurrency and Race Conditions (Implicit Issue)

Your code doesn't have any explicit locking, which implies that the `metadata.Store` and `objectStore` implementations must be safe for concurrent use. This is a perfectly valid design choice, but it's a critical one.

*   For example, in `handleCreateBucket`, you have a "check-then-act" sequence:
    1.  `ok, _ := s.store.BucketExists(ctx, name)`
    2.  `if err := s.store.CreateBucket(ctx, name); err != nil { ... }`

    If two requests to create the same bucket arrive at nearly the same time, it's possible for both to pass the `BucketExists` check before either has a chance to call `CreateBucket`. The `CreateBucket` implementation itself must be atomic to handle this. Make sure this is documented in your interfaces.

### Minor Suggestions and Code Improvements

*   **Multipart Part Storage:** Storing parts under a visible path like `/.multipart/` within the same bucket could be problematic. Clients performing a `ListObjectsV2` might see these temporary files if the prefix matches. A common pattern is to use a separate, hidden "staging" area or to use object keys that are suffixed with upload IDs and part numbers in a way that is unlikely to be listed by users.
*   **Range Parsing Robustness:** Your `parseRange` function is quite good. However, S3 clients can be quirky. It's solid for a starting point, but be prepared to encounter more edge cases as you test with different S3 clients (e.g., multiple ranges, which you are correctly not supporting yet).
*   **Magic Numbers/Strings:** Use constants for common strings like XML namespaces, error codes (`"NoSuchBucket"`), and query parameters (`"list-type"`). This prevents typos and makes the code easier to maintain.
    ```go
    const (
        s3Xmlns         = "http://s3.amazonaws.com/doc/2006-03-01/"
        errCodeNoSuchBucket = "NoSuchBucket"
    )
    ```
*   **Continuation Token Logic in `ListObjectsV2`:**
    ```go
    nextToken = objects[maxKeys-1].Key
    objects = objects[:maxKeys]
    ```
    This is a common and correct pattern. Just ensure that the `objectStore.List` implementation guarantees a stable, alphabetical sort order for keys. If the order is not guaranteed, pagination will be unreliable.

### Summary

You do not have any "huge mistakes" that require a complete rewrite. The foundation is solid. The primary actions you should take are:

1.  **Fix the `CompleteMultipartUpload` memory usage immediately.** This is a critical scalability bug.
2.  **Fix the Range GET fallback memory usage.** This is also a critical stability bug.
3.  **Review error handling for metadata consistency.** Decide on a strategy for handling partial failures (e.g., in `handleDeleteBucket`).
4.  **Ensure your backend implementations are concurrent-safe**, especially for "check-then-act" operations.

This is a fantastic start to a complex project. Keep up the great work
## Future Work: Admin Control Plane, UI, and Monitoring

This section captures planned control-plane capabilities (admin API/UI/CLI) and a practical monitoring strategy for production operations.

- Admin Control Plane and UI
  - Separate Admin API (control plane) from S3 data plane to avoid privilege bleed and to keep admin flows isolated.
  - Expose a secure Admin UI (SPA) and CLI that talk only to the Admin API; operators never use the S3 endpoints for admin tasks.
  - Core endpoints (initial set):
    - Cluster nodes: add/join, cordon/drain, decommission, list/describe
    - Placement/ring: view, rebalance (throttled), progress/status
    - Scrubber/repair: status, start/stop/pause, queue depths, last error (wire to [repair.Scrubber](pkg/repair/scrubber.go))
    - Backups: list/create metadata snapshot, restore (with guardrails)
    - Tenants/buckets: create/delete, quotas/policies (scoped RBAC)
    - Configuration: get/set (transactional with validation; idempotent)
  - Control-plane state (future): embedded Raft metadata store to be the single source of truth for cluster membership, placement configs, and admin operations. Admin API mutates state via the leader; workers (rebalance, scrub, replicate) consume work queues derived from Raft state.

- Authentication and Authorization (Admin API)
  - AuthN: OIDC/OAuth2 (e.g., Keycloak, Dex, Auth0, Okta, Zitadel) using go-oidc + oauth2 for Go.
  - AuthZ: RBAC/ABAC with Casbin or policy-as-code with OPA (Open Policy Agent).
  - Suggested roles:
    - cluster-admin: all admin ops; can approve destructive/irreversible actions
    - storage-admin: node/ring/scrub operations; no auth configuration access
    - tenant-admin: bucket/policy operations scoped to a tenant
    - read-only: dashboards/metrics/cluster info only
    - auditor: read-only plus audit log access
  - Transport:
    - TLS for Admin API/UI; mTLS for node-to-node control-plane communications and joining workflows
    - Separate admin listen address/port from S3 data plane

- Cluster Operations (examples)
  - Node add/join: new node authenticates via mTLS/OIDC; Admin API validates and places node into warm-up; rebalance gradually with safety checks
  - Cordon/drain: prevent new placements and migrate shards away before maintenance
  - Decommission: only when drained and redundancy satisfied
  - Rebalance: ring-aware, throttled, failure-domain aware; visible progress
  - Replication (future): asynchronous, bucket-level policies to secondary cluster or remote storage

- Backups and Restore
  - Metadata: periodic Raft snapshots with versioning and tested restore path; encryption and immutability recommended
  - Data: replication or snapshot-and-replay approach with post-restore scrub
  - Guardrails: require cluster-admin approval; record audit events

- Admin UI/CLI
  - UI: SPA with OIDC login; role-aware; features include nodes health, ring balance, scrub status, backup lifecycle, tenant policies
  - CLI: same Admin API; supports GitOps workflows and automation; dry-run/diff support

### Monitoring and Health

- Recommended Stack
  - Prometheus (scrape /metrics), Alertmanager (alert routing), Grafana (dashboards)
  - Existing: /metrics endpoint and basic HTTP metrics are provided via [obs.metrics.Metrics](pkg/obs/metrics/metrics.go) and wired in [main.main](cmd/s3free/main.go)

- Exporters (system level)
  - node_exporter: host CPU, memory, IO, filesystem usage (bytes, inodes), context switches, saturation
  - smartctl/smartmon_exporter: disk SMART attributes (reallocated sectors, temperature, pending sectors)
  - blackbox_exporter: HTTP probes for /livez, /readyz, Admin API availability
  - Optional: process_exporter to monitor multiple daemons if running sidecars

- Application Metrics (additions to expose in-process)
  - Data plane
    - S3 request counters and latency histograms by method and status (already in place); consider route-level metrics carefully
    - Storage backend: bytes read/written, op latency and errors; read-path integrity failures
  - FreeXL storage
    - Shards written/read/repaired; RS parameters distribution; header/footer/Merkle verification failure counts
    - Placement/ring balance score, pending migrations, move rates
  - Scrubber/repair
    - Queue depths, scan/repair rates, last run, last error (align with [repair.NoopScrubber](pkg/repair/scrubber.go))
  - Buckets/tenants
    - Low-cardinality aggregates: per-tenant/bucket totals only if bounded; prefer top-N via recording rules to avoid label explosion

- Health Endpoints and Readiness
  - /livez: lightweight liveness; returns OK quickly
  - /readyz: readiness gate reflecting config loaded, storage initialized, metrics registered (implemented in [main.main](cmd/s3free/main.go))
  - Admin API health: separate probe (e.g., /admin/health) with internal dependency checks (Raft leader, queues)

- Dashboards (Grafana) and Alerts (Alertmanager)
  - Dashboards
    - Data plane: throughput, error rate, p50/p95/p99 latencies per operation
    - Storage systems: disk capacity and IO latencies; SMART status and temperature charts
    - Placement/ring: balance/bucket migration views; progress bars for rebalances
    - Scrubber and repair: debt and rate charts; time since last success; error trend
  - Alerts (examples)
    - SLO: sustained 5xx error rate; GET/PUT p99 latency breaches
    - Capacity: disk usage (bytes/inodes) over thresholds; insufficient free stripes before maintenance
    - Integrity/health: SMART critical; repeated integrity failures; scrubber persistent errors
    - Availability: node down; blackbox probe failures for S3 and Admin endpoints

- Logging and Tracing
  - Logs: centralize with Loki or ELK; include request IDs and subject identities for admin actions
  - Tracing: add OpenTelemetry later; correlate Admin API actions with background operations (rebalance/scrub/replicate)

- Metric Cardinality Guidelines
  - Avoid high-cardinality labels (object key, request ID, IP). Prefer bounded labels (method, code, tenant).
  - Use recording rules for top-N and long-window aggregations; keep raw series counts manageable.

- Rollout Plan in this Repository
  - Phase A: Admin API skeleton on separate port with OIDC auth and RBAC middleware; read-only endpoints (cluster/nodes status)
  - Phase B: Mutating admin operations with guardrails (cordon/drain, scrub start/stop, backups)
  - Phase C: Admin UI (SPA) + CLI; role-aware pages and commands
  - Phase D: Replication and backup end-to-end flows with audit trail
  - Phase E: Tracing, chaos tests for rebalance/drain; alerting rules and runbooks
