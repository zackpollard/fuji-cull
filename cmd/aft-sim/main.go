// Command aft-sim emulates the patched aft-mtp-cli against a local media
// tree, so the whole engine (discovery, head sweep, streaming, import,
// breakers) can be exercised without a camera — on the desktop and inside
// the Android emulator alike. It speaks exactly the command surface the
// engine uses and reproduces the X-H2S behaviors observed in the field:
// per-invocation session setup cost, single-claim exclusivity, link-speed
// pacing, and the stale-buffer sickness (toggled at runtime via a file, the
// "power cycle" being its removal).
//
// Environment:
//
//	FUJI_FAKE_ROOT      media tree containing "SLOT 1/DCIM/NNN_FUJI/DSCF*"
//	FUJI_FAKE_SETUP_MS  session-setup latency per invocation (default 1200)
//	FUJI_FAKE_MBPS      transfer pacing in MB/s (default 30; 0 = unlimited)
//
// Sickness: write modes into <root>/.sick ("part", "bulk", "thumb",
// space-separated) and the matching transfers return stale-buffer garbage;
// delete the file to emulate the curing power cycle.
package main

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/zack/fuji-tools/internal/jpegmeta"
)

type sim struct {
	root   string
	cwd    string // camera-absolute, e.g. "/SLOT 1/DCIM"
	ids    map[string]string
	paths  map[string]string // id -> filesystem path
	rate   float64           // bytes per second; 0 = unlimited
	out    *bufio.Writer
	failed bool
}

func main() {
	root := os.Getenv("FUJI_FAKE_ROOT")
	if root == "" {
		fmt.Fprintln(os.Stderr, "aft-sim: FUJI_FAKE_ROOT not set")
		os.Exit(1)
	}

	// Single claim, like the real device: a second concurrent invocation
	// must fail with the exact phrase the retry logic greps for.
	lock, err := os.OpenFile(filepath.Join(root, ".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aft-sim:", err)
		os.Exit(1)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Fprintln(os.Stderr, "Device is already used by another process")
		os.Exit(1)
	}

	setup := 1200
	if v, err := strconv.Atoi(os.Getenv("FUJI_FAKE_SETUP_MS")); err == nil && v >= 0 {
		setup = v
	}
	time.Sleep(time.Duration(setup) * time.Millisecond)

	mbps := 30.0
	if v, err := strconv.ParseFloat(os.Getenv("FUJI_FAKE_MBPS"), 64); err == nil && v >= 0 {
		mbps = v
	}

	s := &sim{
		root:  root,
		cwd:   "/",
		ids:   map[string]string{},
		paths: map[string]string{},
		rate:  mbps * (1 << 20),
		out:   bufio.NewWriter(os.Stdout),
	}
	s.index()
	defer s.out.Flush()

	// args (-b, -v, --device-fd N) are accepted and ignored; commands come
	// from stdin in every mode the engine uses
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<20)
	for in.Scan() {
		if !s.dispatch(in.Text(), in) {
			break
		}
	}
	s.out.Flush()
	if s.failed {
		os.Exit(1)
	}
}

// index assigns stable object IDs in sorted walk order, from 1000.
func (s *sim) index() {
	var files []string
	filepath.WalkDir(s.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			return nil
		}
		files = append(files, p)
		return nil
	})
	sort.Strings(files)
	for i, p := range files {
		id := strconv.Itoa(1000 + i)
		s.ids[p] = id
		s.paths[id] = p
	}
}

func (s *sim) fsPath(cameraPath string) string {
	return filepath.Join(s.root, filepath.FromSlash(strings.TrimPrefix(cameraPath, "/")))
}

func (s *sim) resolve(arg string) string {
	if strings.HasPrefix(arg, "/") {
		return arg
	}
	return strings.TrimRight(s.cwd, "/") + "/" + arg
}

func (s *sim) sick(mode string) bool {
	raw, err := os.ReadFile(filepath.Join(s.root, ".sick"))
	return err == nil && strings.Contains(string(raw), mode)
}

// stale mimics the X-H2S replaying an unrelated response buffer: sized like
// a real answer but starting with none of the valid media magics.
func stale(n int64) []byte {
	b := make([]byte, n)
	copy(b, "STALEBUF")
	for i := 8; i < len(b); i++ {
		b[i] = byte(i * 37)
	}
	return b
}

func (s *sim) pace(n int64) {
	if s.rate > 0 {
		time.Sleep(time.Duration(float64(n) / s.rate * float64(time.Second)))
	}
}

func (s *sim) errf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", a...)
	s.failed = true
}

func (s *sim) dispatch(line string, in *bufio.Scanner) bool {
	args := tokenize(line)
	if len(args) == 0 {
		return true
	}
	switch args[0] {
	case "cd":
		if len(args) < 2 {
			s.errf("cd: missing path")
			return true
		}
		p := s.resolve(args[1])
		if st, err := os.Stat(s.fsPath(p)); err != nil || !st.IsDir() {
			s.errf("cd %s: no such directory", p)
			return true
		}
		s.cwd = p
	case "ls":
		entries, err := os.ReadDir(s.fsPath(s.cwd))
		if err != nil {
			s.errf("ls: %v", err)
			return true
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			id := s.ids[filepath.Join(s.fsPath(s.cwd), e.Name())]
			if id == "" {
				id = "0"
			}
			fmt.Fprintf(s.out, "%s\t%s\n", id, e.Name())
		}
	case "lsext":
		dir := s.cwd
		if len(args) >= 2 {
			dir = s.resolve(args[1])
		}
		entries, err := os.ReadDir(s.fsPath(dir))
		if err != nil {
			s.errf("lsext %s: %v", dir, err)
			return true
		}
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			full := filepath.Join(s.fsPath(dir), e.Name())
			// follow symlinks: corpora link real media, and the engine
			// validates pulls against this size
			info, err := os.Stat(full)
			if err != nil {
				continue
			}
			fmt.Fprintf(s.out, "%s 268435457 File %d %s %s\n",
				s.ids[full], info.Size(), info.ModTime().Format("2006-01-02 15:04:05"), e.Name())
		}
	case "lsprops":
		if len(args) < 2 {
			s.errf("lsprops: missing path")
			return true
		}
		dir := s.resolve(args[1])
		entries, err := os.ReadDir(s.fsPath(dir))
		if err != nil {
			s.errf("lsprops %s: %v", dir, err)
			return true
		}
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			full := filepath.Join(s.fsPath(dir), e.Name())
			info, err := os.Stat(full)
			if err != nil {
				continue
			}
			fmt.Fprintf(s.out, "%s %d %s\n", s.ids[full], info.Size(), e.Name())
		}
	case "get":
		if len(args) < 3 {
			s.errf("get: usage: get NAME DEST")
			return true
		}
		s.transfer(filepath.Join(s.fsPath(s.cwd), args[1]), args[2], "bulk")
	case "get-id":
		if len(args) < 3 {
			s.errf("get-id: usage: get-id ID DEST")
			return true
		}
		p, ok := s.paths[args[1]]
		if !ok {
			s.errf("get-id %s: no such object", args[1])
			return true
		}
		s.transfer(p, args[2], "bulk")
	case "get-thumb":
		if len(args) < 3 {
			s.errf("get-thumb: usage: get-thumb NAME DEST")
			return true
		}
		src := filepath.Join(s.fsPath(s.cwd), args[1])
		if s.sick("thumb") {
			os.WriteFile(args[2], stale(6*1024), 0o644)
			s.pace(6 * 1024)
			return true
		}
		head := make([]byte, 256<<10)
		f, err := os.Open(src)
		if err != nil {
			s.errf("get-thumb %s: %v", args[1], err)
			return true
		}
		n, _ := io.ReadFull(f, head)
		f.Close()
		th := jpegmeta.Thumbnail(head[:n])
		if th == nil {
			s.errf("get-thumb %s: no embedded thumbnail", args[1])
			return true
		}
		os.WriteFile(args[2], th, 0o644)
		s.pace(int64(len(th)))
	case "get-part":
		if len(args) < 5 {
			s.errf("get-part: usage: get-part ID OFFSET SIZE DEST")
			return true
		}
		off, _ := strconv.ParseInt(args[2], 10, 64)
		size, _ := strconv.ParseInt(args[3], 10, 64)
		data, err := s.readPart(args[1], off, size)
		if err != nil {
			s.errf("get-part %s: %v", args[1], err)
			return true
		}
		os.WriteFile(args[4], data, 0o644)
		s.pace(int64(len(data)))
	case "serve-parts":
		s.serveParts(in)
		return false
	case "device-info":
		fmt.Fprintln(s.out, "Manufacturer: FUJIFILM")
		fmt.Fprintln(s.out, "Model: X-H2S (aft-sim)")
	case "quit", "q":
		return false
	default:
		s.errf("unknown command %q", args[0])
	}
	return true
}

// transfer copies a full object, paced, honoring bulk sickness. Writes are
// incremental like real MTP pulls, so incremental promotion sees growth.
func (s *sim) transfer(src, dest, sickMode string) {
	info, err := os.Stat(src)
	if err != nil {
		s.errf("get %s: %v", src, err)
		return
	}
	if s.sick(sickMode) {
		os.WriteFile(dest, stale(info.Size()), 0o644)
		s.pace(info.Size())
		return
	}
	in, err := os.Open(src)
	if err != nil {
		s.errf("get %s: %v", src, err)
		return
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		s.errf("get %s: %v", src, err)
		return
	}
	defer out.Close()
	buf := make([]byte, 1<<20)
	for {
		n, rerr := in.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
			s.pace(int64(n))
		}
		if rerr != nil {
			return
		}
	}
}

func (s *sim) readPart(id string, off, size int64) ([]byte, error) {
	p, ok := s.paths[id]
	if !ok {
		return nil, fmt.Errorf("no such object")
	}
	if s.sick("part") {
		return stale(size), nil
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, size)
	n, err := f.ReadAt(buf, off)
	if err != nil && err != io.EOF {
		return nil, err
	}
	return buf[:n], nil
}

// serveParts speaks the persistent-session protocol: "R id off size" per
// request, responses framed as \x01OK <n>\n plus n raw bytes; "Q" quits.
// Reuses the caller's scanner — a fresh one would silently eat any request
// lines it had already buffered.
func (s *sim) serveParts(in *bufio.Scanner) {
	for in.Scan() {
		f := strings.Fields(in.Text())
		if len(f) == 0 {
			continue
		}
		if f[0] == "Q" {
			return
		}
		if f[0] != "R" || len(f) < 4 {
			continue
		}
		off, _ := strconv.ParseInt(f[2], 10, 64)
		size, _ := strconv.ParseInt(f[3], 10, 64)
		data, err := s.readPart(f[1], off, size)
		if err != nil {
			fmt.Fprintf(s.out, "\x01ERR %v\n", err)
			s.out.Flush()
			continue
		}
		fmt.Fprintf(s.out, "\x01OK %d\n", len(data))
		s.out.Write(data)
		s.out.Flush()
		s.pace(int64(len(data)))
	}
}

// tokenize splits a command line honoring Go-%q-style quoted arguments.
func tokenize(line string) []string {
	var args []string
	i := 0
	for i < len(line) {
		for i < len(line) && line[i] == ' ' {
			i++
		}
		if i >= len(line) {
			break
		}
		if line[i] == '"' {
			j := i + 1
			for j < len(line) {
				if line[j] == '\\' {
					j += 2
					continue
				}
				if line[j] == '"' {
					break
				}
				j++
			}
			if j >= len(line) {
				args = append(args, line[i+1:])
				break
			}
			if tok, err := strconv.Unquote(line[i : j+1]); err == nil {
				args = append(args, tok)
			} else {
				args = append(args, line[i+1:j])
			}
			i = j + 1
		} else {
			j := i
			for j < len(line) && line[j] != ' ' {
				j++
			}
			args = append(args, line[i:j])
			i = j
		}
	}
	return args
}
