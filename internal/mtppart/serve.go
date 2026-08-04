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
	// poisoned marks a session whose stream framing can no longer be trusted.
	// Reusing it would return another object's bytes as if they were the
	// requested ones, with no error to notice — see ReadAt.
	poisoned bool
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
// ReadAt reads a byte range of an object.
//
// The wire protocol carries no correlation between a request and its response:
// a reply is just a marker, a length and that many bytes. So a read that is
// abandoned or fails part-way leaves unread payload in the pipe, and the NEXT
// read would scan that binary data for the marker byte and hand back a slice
// of some other object as if it were the one asked for — silently, with no
// error anywhere. That is the worst failure this component can produce, so any
// error poisons the session: it must be closed and reopened, never reused.
func (s *Server) ReadAt(objectID string, offset, size int64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned {
		return nil, fmt.Errorf("serve-parts session is poisoned by an earlier failed read; reopen it")
	}
	if _, err := fmt.Fprintf(s.in, "R %s %d %d\n", objectID, offset, size); err != nil {
		s.poisoned = true
		return nil, fmt.Errorf("serve-parts write: %w", err)
	}
	return s.readFramed()
}

// readFramed reads one '\x01OK <n>\n' + n bytes reply. Caller holds s.mu.
func (s *Server) readFramed() ([]byte, error) {
	// Skip library chatter up to the \x01 response marker; keep it plus
	// the process's stderr tail for the error message if it died.
	chatter, err := s.out.ReadBytes(0x01)
	if err != nil {
		s.poisoned = true
		return nil, fmt.Errorf("serve-parts: %w; output: %.200s; stderr: %.300s",
			err, strings.TrimSpace(string(chatter)), s.errb.String())
	}
	line, err := s.out.ReadString('\n')
	if err != nil {
		s.poisoned = true
		return nil, fmt.Errorf("serve-parts response: %w", err)
	}
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "OK ") {
		// An ERR reply is framed and leaves nothing unread, so the session
		// stays usable; anything else means we have lost the framing.
		if !strings.HasPrefix(line, "ERR ") {
			s.poisoned = true
		}
		return nil, fmt.Errorf("serve-parts: %s", line)
	}
	n, err := strconv.Atoi(line[3:])
	if err != nil || n < 0 {
		s.poisoned = true
		return nil, fmt.Errorf("serve-parts: bad length %q", line)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(s.out, buf); err != nil {
		s.poisoned = true // payload half-consumed: framing is gone
		return nil, fmt.Errorf("serve-parts body: %w", err)
	}
	return buf, nil
}

// NameOf asks the camera what a handle is actually called.
//
// This is the authoritative identity check, and the reason it lives on the
// session: the same question through a fresh aft invocation costs ~4 s of
// setup, which is far too slow to ask per file. Here it is one round trip on a
// pipe that is already open.
//
// It matters because a camera rebinds handles as files come and go. A rebound
// handle returns a different photo under the name the caller asked for — and
// if the two files happen to share a byte count, nothing else in the pipeline
// can tell.
func (s *Server) NameOf(objectID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned {
		return "", fmt.Errorf("serve-parts session is poisoned by an earlier failed read; reopen it")
	}
	if _, err := fmt.Fprintf(s.in, "N %s\n", objectID); err != nil {
		s.poisoned = true
		return "", fmt.Errorf("serve-parts write: %w", err)
	}
	body, err := s.readFramed()
	if err != nil {
		return "", err
	}
	return string(body), nil
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
