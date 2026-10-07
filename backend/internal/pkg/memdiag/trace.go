// Package memdiag provides opt-in, bounded diagnostics for large HTTP Responses requests.
// All memory values describe the whole process, including concurrent work.
package memdiag

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type contextKey struct{}

var selected atomic.Bool
var captures atomic.Int64
var requests atomic.Int64
var nextCapture atomic.Int64
var config = struct {
	enabled   bool
	threshold int64
	dir       string
}{os.Getenv("SUB2API_MEMDIAG") == "1", 16 << 20, os.Getenv("SUB2API_MEMDIAG_DIR")}

func init() {
	if n, err := strconv.ParseInt(os.Getenv("SUB2API_MEMDIAG_MIN_BYTES"), 10, 64); err == nil && n > 0 {
		config.threshold = n
	}
}
func Track() func() {
	if !config.enabled {
		return func() {}
	}
	requests.Add(1)
	return func() { requests.Add(-1) }
}

type Sample struct {
	Allocated uint64 `json:"process_total_alloc_bytes"`
	Heap      uint64 `json:"process_heap_bytes"`
	GC        uint64 `json:"process_gc_cycles"`
	RSS       uint64 `json:"process_rss_bytes"`
}

func snapshot() Sample {
	s := []metrics.Sample{{Name: "/gc/heap/allocs:bytes"}, {Name: "/memory/classes/heap/objects:bytes"}, {Name: "/gc/cycles/total:gc-cycles"}}
	metrics.Read(s)
	r := Sample{Allocated: s[0].Value.Uint64(), Heap: s[1].Value.Uint64(), GC: s[2].Value.Uint64()}
	if b, e := os.ReadFile("/proc/self/statm"); e == nil {
		f := strings.Fields(string(b))
		if len(f) > 1 {
			v, _ := strconv.ParseUint(f[1], 10, 64)
			r.RSS = v * uint64(os.Getpagesize())
		}
	}
	return r
}

type Trace struct {
	mu                sync.Mutex
	id                string
	start             time.Time
	last              Sample
	peakHeap, peakRSS uint64
	stages, profiles  int
	stop, done        chan struct{}
	once              sync.Once
}

func Begin(ctx context.Context, n int64) (context.Context, *Trace) {
	if !config.enabled || n < config.threshold || captures.Load() >= 3 || time.Now().UnixNano() < nextCapture.Load() || !selected.CompareAndSwap(false, true) {
		return ctx, nil
	}
	captures.Add(1)
	t := &Trace{id: fmt.Sprintf("%d-%d", time.Now().UnixNano(), captures.Load()), start: time.Now(), stop: make(chan struct{}), done: make(chan struct{})}
	t.last = snapshot()
	t.peakHeap = t.last.Heap
	t.peakRSS = t.last.RSS
	slog.Info("memdiag.begin", "trace_id", t.id, "body_length_hint", n, "process", t.last, "active_http_responses", requests.Load())
	t.profile("begin")
	go func() {
		defer close(t.done)
		tick := time.NewTicker(500 * time.Millisecond)
		defer tick.Stop()
		deadline := time.NewTimer(5 * time.Minute)
		defer deadline.Stop()
		for {
			select {
			case <-t.stop:
				return
			case <-deadline.C:
				return
			case <-tick.C:
				t.Stage("sample", -1)
			}
		}
	}()
	return context.WithValue(ctx, contextKey{}, t), t
}
func From(ctx context.Context) *Trace { t, _ := ctx.Value(contextKey{}).(*Trace); return t }
func (t *Trace) Stage(stage string, n int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stages >= 650 {
		return
	}
	t.stages++
	s := snapshot()
	if s.Heap > t.peakHeap {
		t.peakHeap = s.Heap
	}
	if s.RSS > t.peakRSS {
		t.peakRSS = s.RSS
	}
	slog.Info("memdiag.stage", "trace_id", t.id, "stage", stage, "elapsed_ms", time.Since(t.start).Milliseconds(), "body_bytes", n, "process", s, "process_alloc_delta_bytes", s.Allocated-t.last.Allocated, "active_http_responses", requests.Load(), "goroutines", runtime.NumGoroutine())
	t.last = s
	if s.Heap >= 128<<20 && t.profiles < 2 {
		t.profile("high_heap")
	}
}
func (t *Trace) Body(body []byte) {
	if t == nil {
		return
	}
	t.Stage("before_shape_scan", len(body))
	shape, err := Inspect(body)
	slog.Info("memdiag.shape", "trace_id", t.id, "shape", shape, "scan_ok", err == nil)
	t.Stage("after_shape_scan", len(body))
}
func (t *Trace) Flag(name string, value bool) {
	if t != nil {
		slog.Info("memdiag.flag", "trace_id", t.id, "name", name, "value", value)
	}
}
func (t *Trace) End() {
	if t == nil {
		return
	}
	t.once.Do(func() {
		close(t.stop)
		<-t.done
		t.Stage("handler_end", -1)
		t.mu.Lock()
		t.profile("end")
		slog.Info("memdiag.end", "trace_id", t.id, "peak_sampled_process_heap_bytes", t.peakHeap, "peak_sampled_process_rss_bytes", t.peakRSS, "elapsed_ms", time.Since(t.start).Milliseconds())
		t.mu.Unlock()
		nextCapture.Store(time.Now().Add(time.Minute).UnixNano())
		selected.Store(false)
	})
}

// Heap profiles contain sampled allocation stacks, not request payloads. No forced GC.
func (t *Trace) profile(label string) {
	if config.dir == "" || t.profiles >= 3 {
		return
	}
	t.profiles++
	if !filepath.IsAbs(config.dir) {
		return
	}
	if err := os.MkdirAll(config.dir, 0700); err != nil {
		slog.Warn("memdiag.profile_failed", "trace_id", t.id, "reason", "directory")
		return
	}
	path := filepath.Join(config.dir, t.id+"-"+label+".pprof")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		slog.Warn("memdiag.profile_failed", "trace_id", t.id, "reason", "create")
		return
	}
	err = pprof.WriteHeapProfile(&limitedWriter{f: f, left: 16 << 20})
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		_ = os.Remove(path)
		slog.Warn("memdiag.profile_failed", "trace_id", t.id, "reason", "write")
		return
	}
	slog.Info("memdiag.profile", "trace_id", t.id, "label", label, "file", path)
}

type limitedWriter struct {
	f    *os.File
	left int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if len(p) > w.left {
		return 0, fmt.Errorf("profile size limit")
	}
	n, err := w.f.Write(p)
	w.left -= n
	return n, err
}
func (t *Trace) ID() string {
	if t == nil {
		return ""
	}
	return t.id
}
