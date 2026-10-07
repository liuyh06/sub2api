package memdiag

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
)

func TestInspect(t *testing.T) {
	b := []byte(`{"input":[{"type":"function_call","namespace":"secret-name","arguments":"SECRET"},{"type":"message","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]},{"type":"function_call_output","output":"secret-output"}],"tools":[{"type":"namespace","name":"SECRET"}],"instructions":"SECRET"}`)
	s, err := Inspect(b)
	if err != nil {
		t.Fatal(err)
	}
	if s.Items != 3 || s.ImageItems != 1 || s.DataImages != 1 || s.Namespaces != 1 || s.OutputBytes != 15 {
		t.Fatalf("bad stats: %+v", s)
	}
	out, _ := json.Marshal(s)
	if strings.Contains(string(out), "SECRET") || strings.Contains(string(out), "secret-") || strings.Contains(string(out), "AAAA") {
		t.Fatal("content leaked")
	}
	if s.Top["input"] == 0 || len(s.Largest) != 3 {
		t.Fatal(s)
	}
}
func TestInspectMalformedBounded(t *testing.T) {
	for _, s := range []string{`{"input":[`, `{"x":"unterminated`, strings.Repeat("[", 130) + strings.Repeat("]", 130)} {
		if _, err := Inspect([]byte(s)); err == nil {
			t.Fatal("expected error")
		}
	}
}
func BenchmarkInspectLargeBody(b *testing.B) {
	body := []byte(`{"input":[{"type":"message","content":"` + strings.Repeat(`text\n`, (8<<20)/6) + `"}]}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := Inspect(body)
		if err != nil {
			b.Fatal(err)
		}
		runtime.KeepAlive(out)
	}
}
func TestTraceLifecycle(t *testing.T) {
	old := config
	config.enabled = true
	config.threshold = 1
	config.dir = t.TempDir()
	defer func() { config = old; captures.Store(0); nextCapture.Store(0) }()
	ctx, tr := Begin(t.Context(), 10)
	if tr == nil || From(ctx) != tr {
		t.Fatal("missing trace")
	}
	tr.Stage("test", 10)
	tr.End()
	tr.End()
	if selected.Load() {
		t.Fatal("capture slot leaked")
	}
}
