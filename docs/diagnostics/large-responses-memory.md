# Temporary large HTTP Responses memory diagnostics

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
