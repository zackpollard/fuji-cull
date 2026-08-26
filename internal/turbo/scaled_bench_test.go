package turbo

import (
	"os"
	"path/filepath"
	"testing"
)

// sample is a real camera frame from the local buffer; benchmarks only, so
// skipping when the buffer is empty is correct rather than a gap.
func sample(t testing.TB) []byte {
	m, _ := filepath.Glob(os.ExpandEnv("$HOME/.cache/fuji-cull/default/*.jpg"))
	for _, p := range m {
		if fi, err := os.Stat(p); err == nil && fi.Size() > 8<<20 {
			b, err := os.ReadFile(p)
			if err == nil {
				return b
			}
		}
	}
	t.Skip("no cached full-size jpeg")
	return nil
}

func BenchmarkFull(b *testing.B) {
	d := sample(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Decode(d)
	}
}
func Benchmark4K(b *testing.B) {
	d := sample(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		DecodeScaled(d, 3840, 2160)
	}
}
func Benchmark2K(b *testing.B) {
	d := sample(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		DecodeScaled(d, 1920, 1080)
	}
}
