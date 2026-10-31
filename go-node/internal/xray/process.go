// Package xray supervises the Xray-core child process and talks to its gRPC
// API. It replaces s6-overlay's xray service plus xray-process.service.ts.
package xray

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	stopTimeout   = 10 * time.Second // s6-svc -wd -T10000
	logRingLines  = 200
	logFileMaxLen = 10 << 20 // s6-log s10485760
)

// Status is s6-svstat -o up,pid.
type Status struct {
	Up  bool
	PID int
}

// Process runs one xray at a time. Like `s6-svc -o` in the TS image it never
// restarts xray on its own: a crash leaves it down until the next start.
type Process struct {
	bin     string
	logPath string
	log     *slog.Logger

	mu     sync.Mutex
	cmd    *exec.Cmd
	done   chan struct{}
	exit   error
	since  time.Time // last state change (start or exit), for StatusLine
	ring   []string
	logF   *os.File
	logLen int64
}

func NewProcess(bin, logPath string, log *slog.Logger) *Process {
	return &Process{bin: bin, logPath: logPath, log: log}
}

// Start launches xray with the given JSON config on stdin (nothing is
// written to disk, like the TS node's config-over-socket). A running xray is
// stopped first.
func (p *Process) Start(config []byte) error {
	if err := p.Stop(); err != nil {
		return err
	}
	cmd := exec.Command(p.bin, "run", "-config", "stdin:", "-format", "json")
	cmd.Stdin = bytes.NewReader(config)
	cmd.Env = os.Environ()
	cmd.SysProcAttr = sysProcAttr()
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start xray: %w", err)
	}

	p.mu.Lock()
	p.cmd, p.done, p.exit, p.since = cmd, make(chan struct{}), nil, time.Now()
	done := p.done
	p.mu.Unlock()

	go p.pump(out)
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.exit, p.since = err, time.Now()
		p.mu.Unlock()
		close(done)
		if err != nil {
			p.log.Warn("xray exited", "err", err)
		}
	}()
	return nil
}

// Stop sends SIGTERM and waits up to 10 s before SIGKILL.
func (p *Process) Stop() error {
	p.mu.Lock()
	cmd, done := p.cmd, p.done
	p.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	default:
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(stopTimeout):
		_ = cmd.Process.Kill()
		<-done
	}
	return nil
}

// Status reports whether the last started xray is still running.
func (p *Process) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd == nil || p.cmd.Process == nil {
		return Status{}
	}
	select {
	case <-p.done:
		return Status{}
	default:
		return Status{Up: true, PID: p.cmd.Process.Pid}
	}
}

// StatusLine has s6-svstat's format, which the TS node puts into its
// "process is not running anymore (s6: …)" error: "up (pid 42) 5 seconds"
// or "down (exitcode 2) 0 seconds, ready 0 seconds".
func (p *Process) StatusLine() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd == nil {
		return "down (exitcode 0) 0 seconds, ready 0 seconds"
	}
	secs := int(time.Since(p.since).Seconds())
	select {
	case <-p.done:
	default:
		return fmt.Sprintf("up (pid %d) %d seconds", p.cmd.Process.Pid, secs)
	}
	how := "exitcode 0"
	if ps := p.cmd.ProcessState; ps != nil {
		if code := ps.ExitCode(); code >= 0 {
			how = fmt.Sprintf("exitcode %d", code)
		} else if sig := signalName(ps); sig != "" {
			how = "signal " + sig
		}
	}
	return fmt.Sprintf("down (%s) %d seconds, ready %d seconds", how, secs, secs)
}

// Tail returns the last n xray log lines.
func (p *Process) Tail(n int) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n > len(p.ring) {
		n = len(p.ring)
	}
	return append([]string(nil), p.ring[len(p.ring)-n:]...)
}

func (p *Process) pump(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		p.mu.Lock()
		p.ring = append(p.ring, line)
		if len(p.ring) > logRingLines {
			p.ring = p.ring[len(p.ring)-logRingLines:]
		}
		p.writeLogFile(line)
		p.mu.Unlock()
	}
}

// writeLogFile keeps /var/log/xray/current like s6-log did (one rotation
// at 10 MiB), so `tail`-based tooling keeps working. Failures are ignored.
func (p *Process) writeLogFile(line string) {
	if p.logPath == "" {
		return
	}
	if p.logF == nil {
		if err := os.MkdirAll(filepath.Dir(p.logPath), 0o755); err != nil {
			p.logPath = ""
			return
		}
		f, err := os.OpenFile(p.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			p.logPath = ""
			return
		}
		st, _ := f.Stat()
		p.logF, p.logLen = f, st.Size()
	}
	if p.logLen > logFileMaxLen {
		_ = p.logF.Close()
		_ = os.Rename(p.logPath, p.logPath+".1")
		p.logF = nil
		p.writeLogFile(line)
		return
	}
	n, _ := p.logF.WriteString(line + "\n")
	p.logLen += int64(n)
}

var semverRe = regexp.MustCompile(`(\d+)(?:\.(\d+))?(?:\.(\d+))?`)

// Version runs `<bin> version` and returns the first line plus the
// semver.coerce() form (e.g. "Xray 26.10.21 (...)" → "26.10.21").
func Version(ctx context.Context, bin string) (raw, semver string, err error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "version").Output()
	if err != nil {
		return "", "", err
	}
	raw = strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if raw == "" {
		return "", "", errors.New("binary produced no version output")
	}
	m := semverRe.FindStringSubmatch(raw)
	if m == nil {
		return raw, "", nil
	}
	part := func(s string) string {
		n, _ := strconv.Atoi(s) // "" and leading zeros behave like semver.coerce
		return strconv.Itoa(n)
	}
	return raw, part(m[1]) + "." + part(m[2]) + "." + part(m[3]), nil
}
