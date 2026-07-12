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
	mu  sync.Mutex
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
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
	c.Stderr = nil
	if err := c.Start(); err != nil {
		return nil, err
	}
	if _, err := io.WriteString(in, "serve-parts\n"); err != nil {
		c.Process.Kill()
		return nil, err
	}
	return &Server{cmd: c, in: in, out: bufio.NewReaderSize(out, 1<<20)}, nil
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
	// Skip library chatter up to the \x01 response marker; keep it for the
	// error message if the process died (e.g. "device is already used").
	chatter, err := s.out.ReadBytes(0x01)
	if err != nil {
		return nil, fmt.Errorf("serve-parts: %w; output: %.200s", err, strings.TrimSpace(string(chatter)))
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
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
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
