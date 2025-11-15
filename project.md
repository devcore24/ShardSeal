# Engineering TODO (Updated)

Status: reflects recent changes (repair enqueue integration and S3-compatible multipart ETag).

- Completed
  - Storage enqueues repair items on sealed integrity failures during GET/HEAD.
  - Scrubber enqueues failing shards into a repair queue.
  - Admin wiring: when Admin API is enabled, a repair queue is created, metrics are exported, and a background repair worker (no-op) starts with admin controls (stats/pause/resume).
  - Multipart completion returns S3-compatible ETag (MD5 of part ETags with -N suffix).
  - Docs updated (README/project) to reflect repair pipeline and multipart ETag behavior.

- High Priority (Next)
  - Repair worker minimal rewrite for single-shard sealed objects (safety checks, idempotent rewrite, metrics); keep no-op as fallback when unsafe.
  - Make repair queue available even without Admin API (config toggle) so storage/scrubber enqueues are always accepted.
  - ListObjectsV2: implement delimiter/common prefixes for S3 parity.
  - SigV4 hardening: update package comment to match implementation, add clock-skew window and X-Amz-Expires validation; expand tests for canonicalization edge cases.
  - Storage/manifest durability: fsync manifest directory on atomic rename to harden against crashes.

- Short Term Enhancements
  - Observability: add S3 op-level metrics (get/put/head/delete/list/multipart) with low-cardinality labels.
  - Admin: add scrubber pause/resume endpoints; keep stats/runonce.
  - Sealed range tests: add range GET tests for sealed objects; verify SectionReader path.
  - Docs: add a short “Repair pipeline” section with example admin requests; note queue depth metric usage in Grafana.

- Medium Term (Core)
  - Real RS codec (klauspost/reedsolomon or similar) and wire encoding/decoding; multi-shard manifests; reconstruct on read.
  - Placement ring (vnodes) across multiple dataDirs/disks; prepare for multi-node.
  - Repair worker: full reconstruct + rewrite from surviving shards; backoff/retry; idempotency.

- Security
  - SigV4: broaden test coverage (headers/query normalization, signed headers superset checks, payload hash modes).
  - OIDC/RBAC: configurable claim-to-role mapping and policy table via config.

- Testing & Quality
  - Fuzz shard header/footer decoders and manifest parser.
  - Integration tests: multipart ETag compatibility, presigned URLs, copy-object.

- Developer Experience
  - `ssadm` CLI for admin endpoints (health, scrub stats/runonce, repair queue controls).

---

# ShardSeal — Open S3-compatible, self-healing object store (Go)

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
  - Integrity: bitrot detection, read-time detection/enqueue (no-op worker), object-level Merkle root
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
 - Healer & Scrubber: on-read detection/enqueue (current milestone); background scanners that detect bitrot, missing shards, and drift; prioritized repair queues.
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

## Storage Format: ShardSeal v1 (self-healing by design)
Goals
- Recoverability from partial state (missing metadata or data) using self-describing shards.
- Fast integrity checks and precise localization of corruption.
- Minimal small-file overhead while supporting multipart and large objects efficiently.

On-Disk Layout (per bucket/object/version)
- object directory: sharded by hash to avoid hot directories (e.g., /data/aa/bb/…)
- manifest file: object.meta (protobuf or JSON v1; protobuf preferred later)
- data shards: per-part, per-block shard files (or extented segments within larger files for compaction)

Per-Shard Header (fixed-size, at file start)
- Magic (ShardSealv1), format_version
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
- Single binary: `shardseal` with subcommands `server`, `disk`, `cluster`, `admin`, `bench`.
- Config file (YAML) + env overrides (address, dataDirs, authMode, accessKeys); hot-reload for select settings.
- Examples: local single-node with N disks (paths), distributed join via peer list.

## Development Plan & Roadmap
- Phase 0: Repo bootstrap
  - Modules, CI, lint/typecheck, unit test scaffold, basic docs, sample configs.
  - Choose local KV (Pebble) and checksum/hash libs (CRC32C, BLAKE3).
- Phase 1: Single-node MVP
  - Local disks, RS(k=6,m=3) default, chunking, headers/footers, per-object manifest.
  - S3: buckets, put/get/head/delete/list, multipart; SigV4; Prometheus metrics.
  - Read-time detection/enqueue; basic scrubber; config + CLI.
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
- cmd/shardseal: main entrypoint
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
- License: GNU Affero General Public License v3.0 or later (AGPL-3.0-or-later).
- Governance: maintainers + reviewers model; RFC process for format changes; versioned storage spec with migration tooling.

## Glossary
- RS(k,m): Reed–Solomon with k data and m parity shards per stripe.
- Stripe/Block: a group of k+m shards derived from a block of data; minimum k shards needed to reconstruct.
- Manifest: per-object metadata describing layout, placement, and integrity.
- Self-healing: automatic detection and repair of missing/corrupted shards during reads or background scans.

## Immediate Next Steps (MVP)
1) Scaffold repo, CI, linting; choose libs (Pebble, BLAKE3, RS codec).
2) Implement local single-node storage with ShardSeal v1 headers/footers and manifest.
3) S3 basic endpoints + multipart; SigV4; metrics.
4) Read-time repair; initial scrubber; docs and examples.

## Current Status & Progress (Updated 2025-11-01)

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
- Config extended with authMode and accessKeys; env overrides via SHARDSEAL_AUTH_MODE and SHARDSEAL_ACCESS_KEYS
- AWS SigV4 authentication implemented (header and presigned) with unit tests
- Prometheus metrics exposed at /metrics; HTTP request instrumentation added
- Readiness improvements: /readyz now reflects dependency readiness (config loaded, storage initialized, metrics registered)
- ShardSeal v1 scaffolding: erasure Params/Codec interfaces with NoopCodec; manifest model and in-memory ManifestStore
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
- Tracing enrichment (2025-10-30):
  - Added span attribute s3.error_code and optional s3.key_hash (config: tracing.keyHashEnabled or env SHARDSEAL_TRACING_KEY_HASH). Key hash uses sha256(key) truncated to 8 bytes (16 hex chars).
  - S3 error responses now include header X-S3-Error-Code; tracing middleware reads it to set s3.error_code.
  - Tests validate X-S3-Error-Code presence on errors and absence on success; full test suite passing.
  - README and sample config updated; main wiring passes config flag into tracing middleware.
- ShardSeal v1 scaffolding (2025-10-31):
  - Sealed shard primitives: header/footer encode/decode with CRC32C verification and sha256 content hashing helper in [erasure.rs](pkg/erasure/rs.go:1).
  - Manifest v1 extensions: ShardMeta and NewSingleShardManifest in [storage.manifest](pkg/storage/manifest.go:1) to describe sealed shards.
  - Unit tests for header/footer round-trip and tamper detection, and hashing helper in [erasure.rs_test](pkg/erasure/rs_test.go:1); test suite green.
  - Sealed I/O wired into LocalFS and S3 paths (feature-flagged via sealed.enabled; sealed.verifyOnRead optional). Writes produce data.ss1 plus manifest; reads prefer manifest; integrity failures mapped to 500 with X-S3-Error-Code; tracing annotates storage.sealed and storage.integrity_fail; sealed metrics emitted. See [storage.localfs](pkg/storage/localfs.go:1), [api.s3](pkg/api/s3/server.go:1), [obs.metrics.storage](pkg/obs/metrics/storage.go:1).
  - Admin scrubber endpoints wired with no-op scrubber and RBAC: GET /admin/scrub/stats, POST /admin/scrub/runonce (see [admin.scrubber](pkg/admin/scrubber.go:1), [security.oidc.rbac](pkg/security/oidc/rbac.go:1), [cmd.shardseal.main](cmd/shardseal/main.go:1)).
  
  Updates (2025-11-01)
  - Integrity scrubber (verification-only) implemented with configurable interval/concurrency and optional payload verification; metrics exposed: scanned_total, errors_total, last_run_timestamp_seconds, uptime_seconds (see [obs.metrics.scrubber](pkg/obs/metrics/scrubber.go:1)).
  - Admin repair control surface added:
    - Queue endpoints: GET /admin/repair/stats, POST /admin/repair/enqueue (see [admin.repair](pkg/admin/repair.go:1))
    - Worker controls: GET /admin/repair/worker/stats, POST /admin/repair/worker/pause, POST /admin/repair/worker/resume (see [admin.repair_worker](pkg/admin/repair_worker.go:1))
    - RBAC roles mapped in policy: admin.repair.read, admin.repair.enqueue, admin.repair.control (see [security.oidc.rbac](pkg/security/oidc/rbac.go:1))
  - Repair metrics added: shardseal_repair_queue_depth with periodic polling (see [obs.metrics.repair](pkg/obs/metrics/repair.go:1)); monitoring assets updated: [configs/monitoring/prometheus/rules.yml](configs/monitoring/prometheus/rules.yml:1), [configs/monitoring/grafana/shardseal_overview.json](configs/monitoring/grafana/shardseal_overview.json:1).

### Next Up 🚧
1) Self-healing (Phase A)
   - Wire repair producers on detection paths to enqueue items: scrubber verification and read-time integrity failures (see [repair.scrubber](pkg/repair/scrubber.go:1), [storage.localfs](pkg/storage/localfs.go:1)).
   - Implement repair worker execution path (no-op -> actual rewrite), ensure idempotency and add outcome/duration metrics.
   - Extend tests to cover enqueue paths and basic repair worker flows.
2) Sealed compaction and GC
   - Manifest reachability scans; sealed shard compaction workflows; admin/CLI triggers; metrics and tests.
3) Sealed robustness
   - Fuzzing and fault injection matrix (header/footer tamper, truncation, partial writes); long-run scrub correctness tests.
4) RS codec integration (Phase B)
   - Implement Reed–Solomon codec, read-time reconstruction (k-of-k+m), background rewriter to update manifests with generations.

## Development Guide

### Building & Running Tests

```bash
make build
go test ./...
```

### Running the Server

```bash
# Using sample config
SHARDSEAL_CONFIG=configs/local.yaml make run
# Or
go run ./cmd/shardseal
```

### Testing with curl (current features)

```bash
# List all buckets
curl -v http://localhost:8080/

# Create a bucket (3-63 chars, lowercase/digits/dot/hyphen)
curl -v -X PUT http://localhost:8080/my-bucket

# Put object from stdin
printf 'Hello, ShardSeal!\n' | curl -v -X PUT http://localhost:8080/my-bucket/hello.txt --data-binary @-

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
shardseal/
├── cmd/shardseal/       # Main entry point
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



Review and required fixes:

This section identifies key improvements and risks in the current codebase without conversational language.

### High-Level Architectural Observations

*   Strong structure: dependency injection via `New(store, objs)` and a clear `route` entry point that dispatches to specific handlers.
*   S3 logic aligns with REST conventions (path parsing, HTTP methods, query parameters) for ListBuckets, Put/Get/Head/Delete Object, and ListObjectsV2.
*   Readable Go code with clear naming and straightforward logic.

### Potential Issues and Risks

#### 1. Inefficient Range GET fallback (high memory risk)

In `handleObject` -> `http.MethodGet`:
```go
// Fallback: read all and slice (inefficient, test-only)
b, _ := io.ReadAll(rc)
// ...
_, _ = w.Write(b[start : end+1])
```
This fallback is marked as inefficient and is hazardous in practice: requesting a small byte range on a very large object forces reading the entire object into memory, which can exhaust memory and crash the process.

*   Recommendation: rely on the `io.ReadSeeker` path. Ensure the `objectStore` returns an `io.ReadCloser` that also implements `io.ReadSeeker` for efficient range handling. Remove the fallback and instead return `501 Not Implemented` when a non-seekable reader is encountered.

#### 2. Inefficient `CompleteMultipartUpload` (major scalability bottleneck)

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
This implementation reads all parts into a single memory buffer (`combinedData`) prior to writing the final object. Large uploads can lead to excessive memory usage.

*   Recommendation: implement streaming assembly. Accept an `io.Reader` in `objectStore.Put` and construct an `io.MultiReader` over per-part readers to stream data with minimal memory footprint.
    ```go
    // Conceptual streaming approach
    var partReaders []io.Reader
    var partClosers []io.ReadCloser
    for i := 1; i <= len(upload.Parts); i++ {
        partKey := /* derive part key */
        rc, _, _, _, err := s.objs.Get(ctx, bucket, partKey)
        // handle error
        partReaders = append(partReaders, rc)
        partClosers = append(partClosers, rc)
    }
    defer func() {
        for _, c := range partClosers { _ = c.Close() }
    }()
    combined := io.MultiReader(partReaders...)
    etag, _, err := s.objs.Put(ctx, bucket, key, combined)
    ```

#### 3. Error handling and ignored errors

Several sites ignore errors using `_ = ...`. While acceptable for best-effort cleanup, it can hide important failures in critical paths.

*   `_ = xml.NewEncoder(w).Encode(res)` in list operations: if the client disconnects, the encoder returns an error; log at debug level to aid diagnostics.
*   `_ = s.store.DeleteBucket(ctx, name)` in delete flows: if `s.objs.RemoveBucket` succeeds but `s.store.DeleteBucket` fails, the filesystem may retain an orphaned bucket. This is a consistency risk; operations need to be atomic or include compensating actions.

*   Recommendation: log ignored errors; design critical metadata flows to avoid partial-commit inconsistencies.

#### 4. Concurrency and race conditions

The current code does not use explicit locking, implying `metadata.Store` and `objectStore` must be safe for concurrent use.

*   Example: in bucket creation, a "check-then-act" sequence occurs:
    1.  `ok, _ := s.store.BucketExists(ctx, name)`
    2.  `if err := s.store.CreateBucket(ctx, name); err != nil { ... }`

    If concurrent requests pass the existence check, the create operation must be atomic. Document the concurrency guarantees in the interfaces and ensure implementations enforce them.

### Minor suggestions and improvements

*   Multipart part storage: storing under a visible path like `/.multipart/` may surface temporary objects in listings; prefer a separate hidden staging area or naming that avoids user-visible prefixes.
*   Range parsing: the `parseRange` implementation is solid for initial support; anticipate client edge cases (e.g., multiple ranges) as compatibility testing expands.
*   Constants: define shared constants for XML namespace, common S3 error codes (e.g., `"NoSuchBucket"`), and query parameters (e.g., `"list-type"`) to reduce typos.
    ```go
    const (
        s3Xmlns             = "http://s3.amazonaws.com/doc/2006-03-01/"
        errCodeNoSuchBucket = "NoSuchBucket"
    )
    ```
*   Continuation token in `ListObjectsV2`:
    ```go
    nextToken = objects[maxKeys-1].Key
    objects = objects[:maxKeys]
    ```
    Ensure the underlying list operation returns a stable, alphabetical order to keep pagination reliable.

### Summary

No issues requiring a complete rewrite were identified. Priority actions:

1.  Replace the `CompleteMultipartUpload` buffer-accumulation logic with a streaming implementation.
2.  Remove the Range GET fallback and require a seekable reader or return `501 Not Implemented`.
3.  Strengthen error handling for metadata consistency and log ignored errors appropriately.
4.  Document and enforce concurrency guarantees for "check-then-act" operations.
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
  - Existing: /metrics endpoint and basic HTTP metrics are provided via [obs.metrics.Metrics](pkg/obs/metrics/metrics.go) and wired in [main.main](cmd/shardseal/main.go)

- Exporters (system level)
  - node_exporter: host CPU, memory, IO, filesystem usage (bytes, inodes), context switches, saturation
  - smartctl/smartmon_exporter: disk SMART attributes (reallocated sectors, temperature, pending sectors)
  - blackbox_exporter: HTTP probes for /livez, /readyz, Admin API availability
  - Optional: process_exporter to monitor multiple daemons if running sidecars

- Application Metrics (additions to expose in-process)
  - Data plane
    - S3 request counters and latency histograms by method and status (already in place); consider route-level metrics carefully
    - Storage backend: bytes read/written, op latency and errors; read-path integrity failures
  - ShardSeal storage
    - Shards written/read/repaired; RS parameters distribution; header/footer/Merkle verification failure counts
    - Placement/ring balance score, pending migrations, move rates
  - Scrubber/repair
    - Queue depths, scan/repair rates, last run, last error (align with [repair.NoopScrubber](pkg/repair/scrubber.go))
  - Buckets/tenants
    - Low-cardinality aggregates: per-tenant/bucket totals only if bounded; prefer top-N via recording rules to avoid label explosion

- Health Endpoints and Readiness
  - /livez: lightweight liveness; returns OK quickly
  - /readyz: readiness gate reflecting config loaded, storage initialized, metrics registered (implemented in [main.main](cmd/shardseal/main.go))
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
  - Tracing (OpenTelemetry):
    - Status
      - Tracing is wired and optional; see [tracing.Init](pkg/obs/tracing/tracing.go) and middleware in [main.main](cmd/shardseal/main.go).
      - Enabled via config/env; exporter supports OTLP gRPC or HTTP.
      - HTTP server spans are created for data plane requests with common HTTP attributes.
    - Recommended defaults
      - Production: ParentBased(TraceIDRatioBased) sampling between 0.05–0.10 to balance cost vs. utility.
      - Staging: 0.10–0.25 for richer diagnostics during feature rollout.
      - Development: 1.0 (AlwaysSample) while actively debugging; disable when not needed.
      - Exporter: OTLP to a collector gateway (gRPC preferred). Use TLS unless running locally.
    - Suggested attributes and events
      - Data plane (HTTP middleware already records method, route, status, peer IP, UA, duration):
        - s3.bucket_present (bool) rather than bucket name to avoid high cardinality
        - s3.key_hash (short hash of key) to correlate without leaking key values
        - s3.op ("PutObject","GetObject","ListObjectsV2","CompleteMultipartUpload", etc.)
        - s3.range (if Range header present, e.g., "bytes=0-1023")
        - s3.error (bool) and s3.error_code (S3 error code) on failures
      - Storage operations (future spans; avoid high-cardinality values):
        - storage.op ("put","get","head","delete","list")
        - storage.bucket (optional; consider toggling on/off)
        - storage.bytes (int) and storage.result ("ok","error")
        - storage.seekable (bool) for GET
        - errors/warnings as span events (e.g., fs errors, partial retries)
      - Admin flows:
        - admin.action ("gc.multipart.run","gc.multipart.abort","health","version")
        - admin.result and counters (scanned/aborted/deleted for GC) as attributes
    - Propagation and context
      - Ensure trace context flows from data plane to background operations when initiated by requests (e.g., admin-triggered GC).
      - For periodic GC, start a new root span per cycle with clear attributes and sampling controls.
    - Cardinality guidance
      - Never record raw object keys or request IDs as high-cardinality labels/attributes.
      - Prefer booleans, enums, and hashed representations; use recording rules in metrics for top-N analyses.
    - Operations guidance
      - Start with 10% sampling; raise temporarily during incidents.
      - Tag environments (env=dev/staging/prod) and service.name to segment traces.
      - Correlate Admin API spans with data plane spans via consistent trace context.

- Metric Cardinality Guidelines
  - Avoid high-cardinality labels (object key, request ID, IP). Prefer bounded labels (method, code, tenant).
  - Use recording rules for top-N and long-window aggregations; keep raw series counts manageable.

- Rollout Plan in this Repository
  - Phase A: Admin API skeleton on separate port with OIDC auth and RBAC middleware; read-only endpoints (cluster/nodes status)
  - Phase B: Mutating admin operations with guardrails (cordon/drain, scrub start/stop, backups)
  - Phase C: Admin UI (SPA) + CLI; role-aware pages and commands
  - Phase D: Replication and backup end-to-end flows with audit trail
  - Phase E: Tracing, chaos tests for rebalance/drain; alerting rules and runbooks

## Release Notes — Experimental Sealed Mode and Integrity Scrubber (2025-10-31)

Scope
- Introduces ShardSeal v1 “sealed” object layout (header | payload | footer) with per-object JSON manifest.
- Adds optional integrity verification on read (verifyOnRead) and a background/admin-triggerable integrity scrubber (verification-only; no healing yet).
- Extends observability with sealed I/O metrics, integrity-failure counters, and scrubber metrics. Provides Prometheus rules and a Grafana overview dashboard.
- Maintains S3 compatibility (API surface unchanged; ETag = MD5 of full payload for both single PUT and multipart complete).

Highlights
- Storage
  - LocalFS sealed write path: reserves header, streams payload while hashing, writes footer, then back-fills header, fsync+rename, and persists manifest.
  - LocalFS sealed read path: prefers manifest; Range reads over payload via SectionReader; optional verifyOnRead re-hashes payload and checks footer.
  - List/Delete aware of sealed layout; plain and sealed objects can coexist.
- S3 API surface
  - GET/HEAD map integrity failures to InternalError (500) and set X-S3-Error-Code=InternalError for observability.
  - Range GET behaves consistently via ReadSeeker-backed reader.
  - Multipart Complete uses streaming of staged parts; ETag policy is MD5 of the final combined object.
- Integrity scrubber (verification-only)
  - Admin endpoints: GET /admin/scrub/stats, POST /admin/scrub/runonce (RBAC: admin.read/admin.scrub).
  - Walks manifests and verifies sealed shard header/footer CRCs and footer content hash; optional payload re-hash.
  - Background mode via config; also supports ad-hoc runs via Admin API.
- Observability
  - Sealed storage metrics: ops, latency, integrity failures with low-cardinality labels.
  - Scrubber metrics: shardseal_scrubber_scanned_total, errors_total, last_run_timestamp_seconds, uptime_seconds.
  - Repair metrics: shardseal_repair_queue_depth (queue depth gauge; low cardinality).
  - Example Prometheus rules and Grafana dashboard provided.

Configuration (YAML + env)
- Sealed mode (experimental)
  - YAML: 
    - sealed.enabled: false
    - sealed.verifyOnRead: false
  - Env:
    - SHARDSEAL_SEALED_ENABLED=true|false
    - SHARDSEAL_SEALED_VERIFY_ON_READ=true|false
- Integrity scrubber (experimental verification-only)
  - YAML:
    - scrubber.enabled: false
    - scrubber.interval: "1h"
    - scrubber.concurrency: 1
    - scrubber.verifyPayload: (optional bool) when omitted, inherits sealed.verifyOnRead
  - Env:
    - SHARDSEAL_SCRUBBER_ENABLED=true|false
    - SHARDSEAL_SCRUBBER_INTERVAL=1h
    - SHARDSEAL_SCRUBBER_CONCURRENCY=2
    - SHARDSEAL_SCRUBBER_VERIFY_PAYLOAD=true|false

Metrics (Prometheus)
- HTTP
  - shardseal_http_requests_total{method,code}
  - shardseal_http_request_duration_seconds_bucket/sum/count{method,code}
  - shardseal_http_inflight_requests
- Storage (generic + sealed)
  - shardseal_storage_bytes_total{op}
  - shardseal_storage_ops_total{op,result}
  - shardseal_storage_op_duration_seconds_bucket/sum/count{op}
  - shardseal_storage_sealed_ops_total{op,sealed,result,integrity_fail}
  - shardseal_storage_sealed_op_duration_seconds_bucket/sum/count{op,sealed,integrity_fail}
  - shardseal_storage_integrity_failures_total{op}
- Scrubber
  - shardseal_scrubber_scanned_total
  - shardseal_scrubber_errors_total
  - shardseal_scrubber_last_run_timestamp_seconds
  - shardseal_scrubber_uptime_seconds
- Repair
  - shardseal_repair_queue_depth

Monitoring assets
- Prometheus:
  - Config: configs/monitoring/prometheus/prometheus.yml
  - Rules: configs/monitoring/prometheus/rules.yml
- Grafana:
  - Dashboard: configs/monitoring/grafana/shardseal_overview.json (includes sealed I/O and scrubber panels)

Operational notes
- Upgrade/migration
  - Enabling sealed mode only affects new writes. Legacy plain files remain readable; mixed sealed/plain operation is supported.
  - Disabling sealed mode does not delete or hide sealed objects (they continue to be served via their manifests).
- ETag policy
  - ETag remains MD5 of full payload for single-part and multipart-completed objects (not AWS multipart-style ETag with part count). A future option may allow alternative policies.
- Performance guidance
  - verifyOnRead and scrubber.verifyPayload introduce CPU and I/O overhead due to hashing and footer validation. Enable selectively based on latency and assurance requirements.
  - Use Prometheus metrics to monitor integrity_failures_total and sealed ops integrity_fail ratios; adjust scrub cadence and verification strategy accordingly.

Known limitations (current)
- No self-healing yet (verification-only scrubber). Next milestone will add repair flows and rewrite support.
- Sealed format is v1 and subject to change under semver pre-release policy; manifests are JSON with minimal fields for M1.

Next steps
- Implement self-healing on read and via scrub/repair queues.
- Extend admin control plane for repair controls and progress reporting.
- Harden sealed format testing (fuzzing, fault injection, truncated-corruption matrix) and add compaction/GC hooks.

## Self-Healing (Design Draft) — RS Reconstruction, Read-Time Heal, Background Rewriter

Goals
- Detect and repair sealed shard corruption or loss automatically.
- Preserve S3 compatibility and latency SLOs by prioritizing on-demand (read-time) heals for hot paths and scheduling background rewriters for cold paths.
- Provide clear observability (metrics/tracing/logs) and safe, idempotent repair semantics.

Scope (phase A)
- M1 (single-shard per object, no parity) path will only detect and report; full RS requires k+m.
- Introduce RS codec integration points and repair queue scaffolding without changing S3 API contracts.
- Implement “read-time verification and repair stub” and “background scan and rewriter skeleton” wired behind config flags.

Architecture Components
- RepairQueue
  - In-memory/work-queue interface with pluggable persistence later.
  - Items: (bucket, key, shardPath, reason, discoveredAt, priority).
  - Producers: read-time detection, scrubber verification, admin enqueue.
  - Consumers: background rewriter workers (configurable concurrency).
- Healer (Read-Time)
  - Wraps storage GET path to detect integrity (already done) and enqueue repair when failures are transient or reconstructable.
  - For M1: if payload can be read but footer mismatch detected, mark object as “needs rewrite”; S3 returns 500 to caller (until RS available).
  - For RS k+m: attempt reconstruct(k of k+m), stream repaired data to client, enqueue rewrite.
- Rewriter (Background)
  - Worker loop that consumes RepairQueue items and executes repair plans:
    - M1: rewrite sealed shard by re-encoding from canonical payload (if available) or skip.
    - RS k+m: reconstruct missing/damaged shards from surviving shards using codec, write new sealed shard(s), update manifest generation.
  - Idempotent: safe to retry; track generation numbers in manifest to avoid stale rewrites.
- Codec Interface (RS)
  - [erasure.Codec](pkg/erasure/rs.go:1) exists; add concrete codec (e.g., Reed-Solomon via a new package).
  - Enhancements: streaming block encoder/decoder interfaces; block size, stripe size config.
- Manifest Extensions
  - Add generation/version per shard to support monotonic rewrite and race-safe updates.
  - Optionally record integrity status and last repair timestamp.
- Admin Control Plane (Phase B)
  - Endpoints to list/inspect queue, pause/resume workers, trigger targeted repair, and show progress.
  - RBAC roles: admin.repair.*.
- Observability
  - Metrics:
    - shardseal_repair_queue_depth
    - shardseal_repair_enqueued_total{reason}
    - shardseal_repair_completed_total{result}
    - shardseal_repair_duration_seconds_bucket/sum/count{mode, result}
    - shardseal_heal_on_read_total{result}
  - Tracing: attributes repair.reason, repair.result, storage.sealed=true during repair I/O operations.

Data Flow (RS k+m future)
1) Read-time detection:
   - Decode header/footer; verify footer hash matches computed; if mismatch => integrity_fail.
   - If RS enabled and enough good shards exist: reconstruct payload block(s) in memory; stream to client; enqueue repair(item: reason=read_heal).
2) Background scrubber:
   - Detect sealed shard with mismatch, enqueue repair(reason=scrub_fail).
3) Background rewriter:
   - Dequeue; for RS: choose target nodes; reconstruct missing/damaged shards; write sealed shard(s); update manifest shards with new generation and CRCs atomically.

RepairQueue Interface (Draft)
```go
// Simple interface to decouple producers/consumers (backed by channel or persistent queue later).
type RepairItem struct {
    Bucket     string
    Key        string
    ShardPath  string // relative path for sealed shard
    Reason     string // "read_heal" | "scrub_fail" | "admin"
    Priority   int
    Discovered time.Time
}
type RepairQueue interface {
    Enqueue(ctx context.Context, it RepairItem) error
    Dequeue(ctx context.Context) (RepairItem, error) // blocking
    Len() int
}
```

Config (Draft)
```yaml
# Healer
healer:
  enabled: true
  maxConcurrent: 8        # bound read-time concurrent heal attempts
  enqueueOnly: false      # when true, do not attempt read-time reconstruct, only enqueue

# Repair workers
repair:
  enabled: false
  concurrency: 2
  backoff: "5s"
  maxRetries: 5
```
Env overrides:
- SHARDSEAL_HEALER_ENABLED, SHARDSEAL_HEALER_MAX_CONCURRENT, SHARDSEAL_HEALER_ENQUEUE_ONLY
- SHARDSEAL_REPAIR_ENABLED, SHARDSEAL_REPAIR_CONCURRENCY, SHARDSEAL_REPAIR_BACKOFF, SHARDSEAL_REPAIR_MAX_RETRIES

Failure Handling
- Partial repair writes:
  - Use temp files + fsync + atomic rename for repaired shards (same as put).
  - Update manifest last — only after all shards safely written and verified.
- Conflicts/races:
  - Manifest generation increments per rewrite; consumers validate generation before apply.
  - If generation mismatch on commit: re-enqueue with backoff.
- Unavailable shards:
  - For RS: if insufficient good shards, defer and alert; for M1: report and skip.

Testing Strategy
- Unit
  - Queue semantics; repair planner from reasons; manifest generation rules; idempotency paths.
- Integration
  - Induce footer/hash mismatch, header CRC corruption, and shard delete scenarios; verify:
    - read-time enqueue or heal (RS later), background rewriter behavior, metrics emission.
- Chaos/Fault Injection
  - Random I/O errors during rewrite; manifest update races; ensure no data loss and no stuck states.
- Long-Run
  - Scrub + repair loop on synthetic dataset; track repair debt burn-down; alert thresholds validation.

Migration and Compatibility
- Off by default; feature flags gate healer/repair.
- Safe to roll out incrementally with verify-only scrubber active prior to enabling repair.
- No S3 surface changes; healing transparent to clients (except read-time 500s in M1 where repair is not possible).

Milestones
- A1: Queue + producers + consumer skeleton; metrics; admin stubs (list/len).
- A2: M1 rewriter no-op/warn; end-to-end metrics and idempotency validation.
- B1: RS codec integration (k+m) with block streaming; reconstruct path + rewriter.
- B2: Admin repair controls and progress endpoints; dashboards/alerts for repair.
- C1: Persistent queue (KV) and compaction hooks; multi-node placement considerations.

References
- Storage and sealed format: [storage.localfs](pkg/storage/localfs.go:1), [erasure.rs](pkg/erasure/rs.go:1), [storage.manifest](pkg/storage/manifest.go:1)
- Observability: [obs.metrics.storage](pkg/obs/metrics/storage.go:1), [obs.metrics.scrubber](pkg/obs/metrics/scrubber.go:1)
- Admin control: [admin.scrubber](pkg/admin/scrubber.go:1), [cmd.shardseal.main](cmd/shardseal/main.go:1)

## Dev Monitoring via Docker Compose

This repository includes an optional Prometheus + Grafana stack controlled via Docker Compose profiles. The compose file defines an explicit shared bridge network to ensure stable service discovery and avoid stale implicit network IDs.

- Network: [docker-compose.yml](docker-compose.yml:1) defines a bridge network "shardseal_net" and attaches services "shardseal", "prometheus", and "grafana" to it.
- Prometheus target: [configs/monitoring/prometheus/prometheus.yml](configs/monitoring/prometheus/prometheus.yml:1) scrapes "shardseal:8080" (service DNS on the Docker network), not "localhost".
- Access:
  - ShardSeal (S3 plane): http://localhost:8080
  - ShardSeal Admin (if enabled): http://localhost:9090
  - Prometheus: http://localhost:9091
  - Grafana: http://localhost:3000 (default admin/admin). Configure a data source pointing to http://prometheus:9090.

Start sequence:
```bash
# 1) Base service (builds the image and creates the network if needed)
docker compose up --build -d

# 2) Monitoring profile (Prometheus + Grafana)
docker compose --profile monitoring up -d
```

Troubleshooting: "failed to set up container networking: network ... not found"
This typically indicates stale compose state or a dangling user-defined network. Clean up and retry:
```bash
# Stop and remove services/anonymous resources from previous runs
docker compose down --remove-orphans

# Remove dangling user-defined networks that may reference old IDs
docker network prune -f

# (Optional) If Prometheus TSDB is not needed, also prune volumes
# docker volume prune -f

# Rebuild and start again
docker compose up --build -d
docker compose --profile monitoring up -d
```

Compose configuration notes:
- The compose stack mounts "./configs" into the container at "/config". Ensure your desired config exists at "./configs/local.yaml" and note that the recommended env for compose is:
  - SHARDSEAL_CONFIG=/config/local.yaml (see [docker-compose.yml](docker-compose.yml:1))
- The base image documentation uses "/home/app/config/config.yaml" for raw "docker run" examples; both paths are supported as long as SHARDSEAL_CONFIG points to the mounted file.

Related documentation:
- See also the README compose instructions and troubleshooting notes in [README.md](README.md:1).
