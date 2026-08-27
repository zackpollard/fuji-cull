package cull

import (
	"testing"
	"time"
)

const mb = 1 << 20

// Photos batch up to the file-count bound: a session per handful of 10 MB
// files is what amortises the setup cost.
func TestChunkEndBatchesSmallFiles(t *testing.T) {
	sizes := make([]int64, 100)
	for i := range sizes {
		sizes[i] = 10 * mb
	}
	if got := chunkEnd(sizes, 0, 24, 512*mb); got != 24 {
		t.Errorf("chunkEnd = %d, want the full count of 24", got)
	}
}

// A big video gets a chunk of its own, so a timeout on it cannot fail the
// files it would otherwise have been batched with.
func TestChunkEndIsolatesALargeVideo(t *testing.T) {
	sizes := []int64{5 << 30, 10 * mb, 10 * mb}
	if got := chunkEnd(sizes, 0, 24, 512*mb); got != 1 {
		t.Errorf("chunkEnd = %d, want the 5 GB video alone in its chunk", got)
	}
	// and the photos after it still batch normally
	if got := chunkEnd(sizes, 1, 24, 512*mb); got != 3 {
		t.Errorf("chunkEnd after the video = %d, want both remaining photos", got)
	}
}

// The byte bound closes a chunk before the count bound when the files are big.
func TestChunkEndStopsOnBytes(t *testing.T) {
	sizes := make([]int64, 24)
	for i := range sizes {
		sizes[i] = 100 * mb
	}
	got := chunkEnd(sizes, 0, 24, 512*mb)
	if got < 1 || got > 6 {
		t.Errorf("chunkEnd = %d, want the byte bound to close it around 5", got)
	}
}

// Never empty, or the pull loop would spin without making progress.
func TestChunkEndNeverEmpty(t *testing.T) {
	if got := chunkEnd([]int64{9 << 30}, 0, 24, 512*mb); got != 1 {
		t.Errorf("chunkEnd = %d, want 1 for a file bigger than the bound", got)
	}
	if got := chunkEnd(nil, 0, 24, 512*mb); got != 0 {
		t.Errorf("chunkEnd on an empty list = %d, want 0", got)
	}
}

// The deadline has to follow the bytes: a chunk holding a 5 GB video needs
// materially longer than one holding a few photos, which is what budgeting by
// file count failed to give it.
func TestPullBudgetScalesWithBytes(t *testing.T) {
	small := pullBudget(50 * mb)
	big := pullBudget(5 << 30)
	if small < 2*time.Minute {
		t.Errorf("small chunk budget = %v, want at least the 2 minute floor", small)
	}
	if big <= small {
		t.Errorf("5 GB budget %v is not more than a 50 MB budget %v", big, small)
	}
	// A 5 GB video at a genuinely slow 10 MB/s takes ~8.5 minutes; the budget
	// must comfortably exceed that or it will keep timing out mid-transfer.
	if want := 9 * time.Minute; big < want {
		t.Errorf("5 GB budget = %v, want at least %v", big, want)
	}
}
