// Package mtppart drives the locally-patched aft-mtp-cli ("aft-mtp-cli-part",
// from ~/Source/aft-partial) that exposes MTP GetPartialObject. Partial reads
// unlock video poster frames (~8 MB of a multi-GB MOV) and camera streaming.
package mtppart

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Bin locates the patched binary; empty string when unavailable.
func Bin() string {
	if p := os.Getenv("FUJI_AFT_PART"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		home + "/.local/bin/aft-mtp-cli-part",
		home + "/Source/aft-partial/build/cli/aft-mtp-cli",
	} {
		if st, err := os.Stat(p); err == nil && st.Mode()&0o111 != 0 {
			return p
		}
	}
	if p, err := exec.LookPath("aft-mtp-cli-part"); err == nil {
		return p
	}
	return ""
}

// PartReq is one partial-object read within a batched invocation.
type PartReq struct {
	ObjectID string
	Offset   int64
	Size     int64
	Dest     string
}

// GetPart reads size bytes at offset from an MTP object into dest.
func GetPart(ctx context.Context, objectID string, offset, size int64, dest string) error {
	return GetParts(ctx, []PartReq{{ObjectID: objectID, Offset: offset, Size: size, Dest: dest}})
}

// GetParts runs a batch of partial reads in a single MTP session (one process
// invocation amortizes session setup — vital for header sweeps over many
// objects). Callers must validate the output bytes themselves: the X-H2S can
// answer partial reads with stale response buffers instead of file data.
func GetParts(ctx context.Context, reqs []PartReq) error {
	bin := Bin()
	if bin == "" {
		return fmt.Errorf("aft-mtp-cli-part not found")
	}
	var cmds strings.Builder
	for _, r := range reqs {
		fmt.Fprintf(&cmds, "get-part %s %d %d %q\n", r.ObjectID, r.Offset, r.Size, r.Dest)
	}
	var out bytes.Buffer
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}
		}
		c := exec.CommandContext(ctx, bin, "-b")
		c.Cancel = func() error { return c.Process.Signal(os.Interrupt) }
		c.WaitDelay = 3 * time.Second
		c.Stdin = strings.NewReader(cmds.String())
		out.Reset()
		c.Stdout = &out
		c.Stderr = &out
		err = c.Run()
		if err == nil || !strings.Contains(out.String(), "already used") {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("get-part: %w; output: %.200s", err, out.String())
	}
	if len(reqs) == 1 {
		if st, serr := os.Stat(reqs[0].Dest); serr != nil || st.Size() == 0 {
			return fmt.Errorf("get-part produced no data; output: %.200s", out.String())
		}
	}
	return nil
}
