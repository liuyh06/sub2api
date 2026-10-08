# Deployment branch and bounded HTTP Responses memory diagnostics

This diagnostic branch includes the namespace allocation fix from PR #7910. It is not part of that PR. It observes natural traffic; it does not replay production requests, record request bodies or expose a pprof HTTP endpoint.

## Enable and bounds

Set these environment variables before starting the binary:

```ini
SUB2API_MEMDIAG=1
SUB2API_MEMDIAG_MIN_BYTES=16777216
SUB2API_MEMDIAG_DIR=/opt/sub2api/data/memdiag-20261008
```

The directory must be writable by the service user. Diagnostics are disabled by default. At most three requests per process lifetime are selected, one at a time, with a 60-second cooldown after completion. The sampling goroutine stops after five minutes; each trace emits at most 650 stage/sample records. Each trace writes at most three heap profiles, each capped at 16 MiB (144 MiB maximum per process lifetime). Counts reset on restart; existing profiles are retained. Files are mode 0600, directories created by the diagnostic code are 0700.

Only the authenticated HTTP Responses handler is selected. Native WebSocket ingress and other handlers are outside this capture scope. All participating HTTP Responses requests are counted, including small ones; the concurrency counter does not include other API routes or background tasks.

Selection before body reading uses Content-Length. Unknown-length or compressed small-on-wire requests can be selected after reading when their normalized body exceeds the threshold, so their read/decompression allocations will not have a pre-read baseline. Earlier middleware may have preread the body; the `body_preread` stage identifies that case. Rejected requests and partial reads can have an incomplete trace.

## Evidence collected

- `memdiag.selected` associates the existing request log identifiers with a new trace ID.
- `memdiag.shape`: raw byte totals for allowlisted top-level fields (unknown names aggregated as `other`); input item count and five largest item indexes/types/sizes; image item count; literal `data:image/` string count/size; encrypted-content and output-field sizes; direct input namespace count.
- Shape categories overlap (e.g. an image can be inside output and input). Counts include raw JSON syntax, not decoded binary bytes or token counts. Escaped data URI prefixes and remote-image contents are not decoded. Unknown item types are reported as `other`. On excessive nesting or malformed structure, `scan_ok=false` indicates partial results; this scanner is not a JSON validator.
- `memdiag.stage`: process-wide cumulative allocation bytes, heap object bytes, GC cycle count, Linux RSS, elapsed time and active observed HTTP Responses count. Stages include body reads, JSON normalization, compatibility transformations, namespace handling, request-map decode, body rebuild, upstream request construction and response headers. Every 500 ms a sample runs while the selected request is active (up to five minutes).
- `memdiag.flag`: whether tool-call namespaces are retained and whether namespace stripping changed the body length.
- `memdiag.end`: maximum sampled process heap/RSS, not an exact high-water mark.
- `memdiag.profile`: a baseline, the first observed heap >=128 MiB, and end heap profile (if the threshold was not crossed only baseline/end are written). Profiles contain sampled allocation stacks, not payload bytes. No forced GC is used. Heap profile accounting can lag GC, and the middle profile is not guaranteed to be the true peak.

The raw scanner performs one pass and does not decode content strings or copy the body. An 8 MiB escaped-text benchmark measured about 3.6 ms and 336 allocated bytes on the development machine; cloud CPU timings will differ. Profiling, logging and sampling still add overhead and appear in process-wide allocation totals. Use `before_shape_scan`/`after_shape_scan` to identify scanner overhead. No GC tuning or memory limit is changed.

## Interpretation

1. Use shape output to distinguish embedded images, encrypted replay, tool outputs and history growth. Correlate trace ID with request ID via `memdiag.selected`.
2. Compare adjacent transformation boundaries and process allocation deltas. These are not per-request allocations: concurrent traffic and background jobs contribute. The 500 ms sampler also creates intervening boundaries, so group records by stage/time rather than attributing every delta to the next named stage.
3. Retrieve the profiles and the exact deployed binary. Compare `alloc_space` to locate allocation-heavy stacks and `inuse_space` for retained heap. Do not equate allocation totals with RSS or infer a single-request multiplier from whole-process peaks.

```bash
go tool pprof -top -sample_index=alloc_space -base BEGIN.pprof ./sub2api-memdiag END.pprof
go tool pprof -top -sample_index=inuse_space ./sub2api-memdiag HIGH_OR_END.pprof
```

Reading Linux PSI/swap counters alongside traces can distinguish memory reclaim stalls from CPU work. Avoid a production load test on the 1.6 GiB server; wait for normal traffic.

## Deployment / rollback

Deployment uses a uniquely named backup of the existing executable and a dedicated systemd drop-in `90-memdiag.conf`. Verify SHA-256 on the staged executable before replacing it, restart, and verify `/health`, the embedded UI and reported build identity. Database migrations are unchanged from the prior deployed source commit.

To disable capture, remove only the diagnostic drop-in, run `systemctl daemon-reload`, then restart `sub2api`. To roll back the binary, restore the exact pre-diagnostic backup recorded in the deployment record before that restart. Preserve collected profiles for analysis. Do not delete unrelated drop-ins or change the production request size/swap settings.

## Verified deployment: 2026-10-08 01:46 Asia/Shanghai

- Host: `47.90.210.138`; service `sub2api.service`, port 8080.
- Build: `0.2.14-memdiag.1`, source commit `14afb7214` (diagnostic branch).
- Executable: `/opt/sub2api/sub2api`; SHA-256 `6fc7daf6966aa485327df52f11a14bb001efd84997f08aa4b7a2c80533ce2934` matched the local artifact and `/proc/16931/exe` immediately after deployment.
- Backup: `/opt/sub2api/sub2api.pre-memdiag-20261008-14afb7214`; SHA-256 `657c046f7f32405de21b1494b4cfd77849340616044a82ea963c14d3e163a6a3`.
- Drop-in: `/etc/systemd/system/sub2api.service.d/90-memdiag.conf`; the three environment values above verified in the running process.
- `/health` and `/` returned HTTP 200, service active, NRestarts=0. No diagnostic captures yet at verification. No synthetic large requests sent to the server.
- Full service and handler unit suites passed (185.703s / 39.431s); memdiag race test passed; enabled local HTTP handler smoke test wrote readable heap profiles. Frontend frozen dependency install and full build passed using pnpm 9.15.9. Root disk was not used for dependency/build caches.
- Local binary: `backend/bin/sub2api-memdiag` (158 MiB, ignored artifact). The local missing `go tool pprof` was built from the installed toolchain's own `src/cmd/pprof` sources and tested against a generated profile.

Exact rollback commands on this host (interrupts active requests on restart):

```sh
cp -p /opt/sub2api/sub2api.pre-memdiag-20261008-14afb7214 /opt/sub2api/sub2api.rollback-tmp
mv /opt/sub2api/sub2api.rollback-tmp /opt/sub2api/sub2api
rm /etc/systemd/system/sub2api.service.d/90-memdiag.conf
systemctl daemon-reload
systemctl restart sub2api
curl -fsS http://127.0.0.1:8080/health
```

## Ongoing deployment workflow

`codex/diagnose-large-request-memory` is the persistent integration/deployment branch for this fork. Keep upstream PR work on separate feature branches; do not add deployment diagnostics to those PRs.

For each rollout:

1. Start from a clean deployment branch and record the currently running binary hash and source/build identity.
2. Integrate only the reviewed feature commits. Use `git cherry-pick -x` when a PR branch is based on newer upstream history, so unrelated upstream changes do not enter production implicitly. Use a normal merge when its complete history is intentionally being deployed. Upstream upgrades are separate, explicitly scoped changes.
3. Keep namespace/bootstrap improvements and bounded diagnostics together. Run affected tests and lint on the combined branch; build the embedded UI with pnpm 9.15.9 and build the Go binary with source commit, version, and timestamp.
4. Preserve a uniquely named binary backup and the exact prior diagnostic drop-in. Stage the new binary, verify its hash, replace atomically, restart, and verify process executable hash, health, UI/assets, and build identity. A failed health check triggers rollback to the saved binary and drop-in.
5. Use a separate capture directory per deployment; retain previous captures. The existing per-process three-request budget, size threshold, cooldown, and privacy boundaries remain in effect. Observe natural traffic instead of sending large synthetic production requests.
6. Record deployment evidence and rollback instructions in this document and push the deployment branch to the fork. Updating a feature PR does not automatically deploy it.

The bootstrap rollout additionally records `before_bootstrap_normalize` and `after_bootstrap_normalize`. These values remain process-wide and may include concurrent work and diagnostic overhead. Compare them with heap profiles and request shape, not as isolated per-request RSS measurements.

## Verified bootstrap deployment: 2026-10-08 16:05 Asia/Shanghai

- Persistent deployment branch: `codex/diagnose-large-request-memory`, pushed to `origin` (the user's fork).
- Integrated bootstrap PR #7928 commit `3d7c4a253` as `56a6cb16e` with source attribution; no unrelated upstream updates or database/schema changes were included.
- Build: `0.2.14-memdiag.2`, source commit `0a23b085f0a54d70f699fe103f3bd20363b4e8e1`, built at `2026-10-08T08:01:39Z` with embedded frontend. Later documentation-only commits do not change this binary's source identity.
- Host/service: `47.90.210.138`, `sub2api.service`; verified PID `41873`, active/running, NRestarts=0.
- SHA-256 of local binary, installed binary, and `/proc/41873/exe`: `e3ed84f49520f2289e17830466d80e3c1bda15111c0992c27bab6e6833e7322d`.
- Prior binary backup: `/opt/sub2api/sub2api.pre-bootstrap-20261008-0a23b085f`, SHA-256 `6fc7daf6966aa485327df52f11a14bb001efd84997f08aa4b7a2c80533ce2934` (memdiag.1).
- Prior diagnostic configuration backup: `/opt/sub2api/memdiag-before-20261008-0a23b085f.conf`.
- New capture directory: `/opt/sub2api/data/memdiag-bootstrap-20261008-0a23b085f`; enabled flag and 16777216-byte threshold were verified in the running process. All previous capture files remain preserved.
- Health, embedded UI, and JS asset returned HTTP 200. The served `index-Cy7elDOu.js` hash matched the local asset: `1ad3f3eecf494a99f30a91512057a26d33b401c0885a2dc895024e6a5ef94b91`.
- Combined deployment-branch handler/memdiag unit tests passed (40.618s / 0.004s); complete backend lint v2.13.0 passed with 0 issues; pnpm 9.15.9 frozen dependency install and full frontend build passed (only bundle-size advisory warnings).
- Immediately after verification: no selected large requests or new profiles yet, and no panic/fatal markers from the new PID. MemorySwapCurrent=0 after restart is not proof of optimization benefit; compare equivalent natural traffic and process-wide profiles before drawing conclusions.
- Local artifacts and before/after evidence: `backend/bin/deploy-bootstrap-20261008/` (ignored).

Rollback to memdiag.1 on this host:

```sh
cp -p /opt/sub2api/sub2api.pre-bootstrap-20261008-0a23b085f /opt/sub2api/sub2api.rollback-bootstrap
mv /opt/sub2api/sub2api.rollback-bootstrap /opt/sub2api/sub2api
cp -p /opt/sub2api/memdiag-before-20261008-0a23b085f.conf /etc/systemd/system/sub2api.service.d/90-memdiag.conf
systemctl daemon-reload
systemctl restart sub2api
curl -fsS http://127.0.0.1:8080/health
sha256sum /opt/sub2api/sub2api
```
