package mtppart

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Server is a persistent partial-read session against the patched
// aft-mtp-cli's serve-parts command: requests go down stdin as
// "R <id> <offset> <size>", responses come back as "\x01OK <n>" plus n raw
// bytes. One MTP session serves every read, so per-request cost is the
// transfer itself (~ms) instead of the ~4 s session setup of one-shot
// invocations — the difference between video streaming working and not.
//
// The device stays claimed for the Server's whole lifetime; all other camera
// work (prefetch batches, gphoto2 thumbs) must be paused while one is open.
type Server struct {
	mu   sync.Mutex
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Reader
	errb cappedBuffer // stderr tail: names the failure when the process dies
}

// cappedBuffer keeps the first ~4KB of writes (enough for aft's error).
type cappedBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	if len(b.buf) < 4096 {
		n := min(len(p), 4096-len(b.buf))
		b.buf = append(b.buf, p[:n]...)
	}
	b.mu.Unlock()
	return len(p), nil
}

func (b *cappedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(string(b.buf))
}

// StartServer spawns the persistent session. Errors surface on the first
// ReadAt (the process reports claim conflicts on stdout, not at spawn).
func StartServer() (*Server, error) {
	bin := Bin()
	if bin == "" {
		return nil, fmt.Errorf("aft-mtp-cli-part not found")
	}
	c := exec.Command(bin, usbArgs()...)
	c.ExtraFiles = usbExtraFiles()
	in, err := c.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	s := &Server{cmd: c, in: in}
	c.Stderr = &s.errb
	if err := c.Start(); err != nil {
		return nil, err
	}
	if _, err := io.WriteString(in, "serve-parts\n"); err != nil {
		c.Process.Kill()
		return nil, err
	}
	s.out = bufio.NewReaderSize(out, 1<<20)
	return s, nil
}

// ReadAt fetches size bytes at offset from an MTP object. Short reads at the
// object's tail return the remaining bytes. Callers must validate content —
// the X-H2S stale-buffer bug applies to this path like every other.
func (s *Server) ReadAt(objectID string, offset, size int64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := fmt.Fprintf(s.in, "R %s %d %d\n", objectID, offset, size); err != nil {
		return nil, fmt.Errorf("serve-parts write: %w", err)
	}
	// Skip library chatter up to the \x01 response marker; keep it plus
	// the process's stderr tail for the error message if it died.
	chatter, err := s.out.ReadBytes(0x01)
	if err != nil {
		return nil, fmt.Errorf("serve-parts: %w; output: %.200s; stderr: %.300s",
			err, strings.TrimSpace(string(chatter)), s.errb.String())
	}
	line, err := s.out.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("serve-parts response: %w", err)
	}
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "OK ") {
		return nil, fmt.Errorf("serve-parts: %s", line)
	}
	n, err := strconv.Atoi(line[3:])
	if err != nil || n < 0 {
		return nil, fmt.Errorf("serve-parts: bad length %q", line)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(s.out, buf); err != nil {
		return nil, fmt.Errorf("serve-parts body: %w", err)
	}
	return buf, nil
}

// Close ends the session politely (Q, then SIGINT) so the MTP claim is
// released cleanly; a hard kill wedges the camera's session.
//
// Deliberately does NOT take s.mu: a ReadAt blocked on a wedged camera holds
// the mutex indefinitely, and Close is precisely how the janitor unwedges it
// — killing the process EOFs the blocked read, which then errors out and
// releases everything. Line-sized pipe writes are atomic (< PIPE_BUF), so
// the Q cannot interleave into a concurrent R request.
func (s *Server) Close() {
	fmt.Fprintln(s.in, "Q")
	s.in.Close()
	done := make(chan struct{})
	go func() { s.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		s.cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			s.cmd.Process.Kill()
			<-done
		}
	}
}
