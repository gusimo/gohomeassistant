package project

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Project struct {
	m       *Manager
	name    string
	dir     string
	log     *slog.Logger
	trigger chan struct{}
	cancel  context.CancelFunc
	sig     uint64 // last poll signature, only used by Manager.poll

	mu      sync.Mutex
	status  Status
	removed bool // set by the manager; no more status updates after that
}

// SetDebug switches debug mode and rebuilds: with debug on, the project is
// compiled without optimizations and runs under a headless Delve.
func (p *Project) SetDebug(on bool) {
	p.update(func(s *Status) { s.Debug = on })
	p.Trigger()
}

// Status is the observable state of a project.
type Status struct {
	Name       string    `json:"name"`
	Dir        string    `json:"dir"` // absolute path, for file:// URIs in the editor
	Building   bool      `json:"building"`
	Running    bool      `json:"running"`
	PID        int       `json:"pid,omitempty"`
	BuildError string    `json:"buildError,omitempty"` // of the last build, "" if it succeeded
	BuiltAt    time.Time `json:"builtAt,omitzero"`     // last successful build
	Restarts   int       `json:"restarts"`             // unexpected exits
	Debug      bool      `json:"debug"`                // debug mode requested
	DebugPort  int       `json:"debugPort,omitempty"`  // Delve's port while running under the debugger
	Removed    bool      `json:"removed,omitempty"`
}

func (p *Project) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

func (p *Project) update(fn func(*Status)) {
	p.mu.Lock()
	fn(&p.status)
	st, removed := p.status, p.removed
	p.mu.Unlock()
	if p.m.cfg.OnStatus != nil && !removed {
		p.m.cfg.OnStatus(st)
	}
}

func newProject(m *Manager, name string) *Project {
	return &Project{
		m:       m,
		name:    name,
		dir:     filepath.Join(m.cfg.Root, name),
		log:     m.cfg.Log.With("project", name),
		trigger: make(chan struct{}, 1),
		status:  Status{Name: name, Dir: filepath.Join(m.cfg.Root, name)},
	}
}

// Trigger schedules a rebuild.
func (p *Project) Trigger() {
	select {
	case p.trigger <- struct{}{}:
	default:
	}
}

func (p *Project) loop(ctx context.Context) {
	var proc *process
	defer func() {
		if proc != nil {
			proc.stop()
		}
	}()
	for {
		start := time.Now()
		debug := p.Status().Debug
		p.update(func(s *Status) { s.Building = true })
		bin, err := p.build(ctx, debug)
		p.update(func(s *Status) {
			s.Building = false
			if err != nil {
				s.BuildError = strings.TrimPrefix(err.Error(), "exit status 1\n")
			} else {
				s.BuildError = ""
				s.BuiltAt = time.Now()
			}
		})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			if proc != nil {
				p.log.Error("build failed, keeping previous version running", "err", err)
			} else {
				p.log.Error("build failed", "err", err)
			}
		} else if sum, err := fileHash(bin); err == nil && proc != nil && sum == proc.hash {
			// Identical binary (e.g. entities changed that this project
			// doesn't use): keep the running process and its state.
			os.Remove(bin)
			p.log.Info("build ok, binary unchanged: keeping the running process")
		} else {
			p.log.Info("build ok", "took", time.Since(start).Round(time.Millisecond))
			if proc != nil {
				proc.stop()
			}
			proc = p.supervise(bin, debug)
			proc.hash = sum
		}

		select {
		case <-ctx.Done():
			return
		case <-p.trigger:
		}
		// wait until changes have settled
		for settled := false; !settled; {
			select {
			case <-ctx.Done():
				return
			case <-p.trigger:
			case <-time.After(debounce):
				settled = true
			}
		}
	}
}

// build compiles the project to a fresh binary. A go.work file makes the
// local SDK module available without the project needing replace directives.
func (p *Project) build(ctx context.Context, debug bool) (string, error) {
	work, err := p.m.writeWork(p.name)
	if err != nil {
		return "", err
	}
	out := filepath.Dir(work)
	// New name per build: a running binary is never overwritten.
	bin := filepath.Join(out, fmt.Sprintf("app-%d", time.Now().UnixNano()))
	// An empty build ID makes the binary depend only on the compiled code,
	// so e.g. regenerated entities the project doesn't use don't restart it.
	// -buildvcs=false: git state (commits, dirty flag) must not change the
	// binary, or every commit would restart the project.
	args := []string{"build", "-buildvcs=false", "-ldflags=-buildid=", "-o", bin}
	if debug {
		args = append(args, "-gcflags=all=-N -l") // no optimizations: accurate stepping and variables
	}
	cmd := exec.CommandContext(ctx, "go", append(args, ".")...)
	cmd.Dir = p.dir
	cmd.Env = append(os.Environ(), "GOWORK="+work)
	var stderr bytes.Buffer
	cmd.Stdout = &stderr
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%w\n%s", err, strings.TrimSpace(stderr.String()))
	}
	return bin, nil
}

type process struct {
	hash     [sha256.Size]byte // of the binary
	stopCh   chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func (pr *process) stop() {
	pr.stopOnce.Do(func() { close(pr.stopCh) })
	<-pr.done
}

// supervise runs bin and restarts it with backoff when it exits on its own.
func (p *Project) supervise(bin string, debug bool) *process {
	pr := &process{stopCh: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(pr.done)
		defer os.Remove(bin)
		backoff := time.Second
		for {
			started := time.Now()
			cmd := exec.Command(bin)
			debugPort := 0
			if debug {
				var err error
				if cmd, debugPort, err = p.m.debugCommand(bin); err != nil {
					p.log.Error("cannot start debugger, running without", "err", err)
					cmd, debugPort = exec.Command(bin), 0
				}
			}
			cmd.Dir = p.dir
			cmd.Env = append(os.Environ(), "GHA_SOCKET="+p.m.cfg.Socket, "GHA_PROJECT="+p.name)
			// A leftover child holding stdout open must not block Wait.
			cmd.WaitDelay = 2 * time.Second
			lw := &lineWriter{log: p.log}
			cmd.Stdout, cmd.Stderr = lw, lw

			exited := make(chan error, 1)
			if err := cmd.Start(); err != nil {
				exited <- err
			} else {
				if debugPort != 0 {
					p.log.Info("started under debugger", "pid", cmd.Process.Pid, "port", debugPort)
				} else {
					p.log.Info("started", "pid", cmd.Process.Pid)
				}
				p.update(func(s *Status) { s.Running, s.PID, s.DebugPort = true, cmd.Process.Pid, debugPort })
				go func() { exited <- cmd.Wait() }()
			}

			select {
			case err := <-exited:
				lw.Flush()
				p.update(func(s *Status) { s.Running, s.PID, s.DebugPort = false, 0, 0; s.Restarts++ })
				p.log.Warn("exited", "err", err, "restart_in", backoff)
				if time.Since(started) > 10*time.Second {
					backoff = time.Second
				}
			case <-pr.stopCh:
				if debugPort != 0 && cmd.Process != nil {
					// Delve runs the program in its own process group; kill it
					// directly (it may be paused at a breakpoint anyway).
					killDescendants(cmd.Process.Pid)
				}
				signal(cmd, syscall.SIGTERM, debugPort != 0)
				select {
				case <-exited:
				case <-time.After(5 * time.Second):
					p.log.Warn("did not stop in time, killing")
					signal(cmd, syscall.SIGKILL, debugPort != 0)
					<-exited
				}
				lw.Flush()
				p.update(func(s *Status) { s.Running, s.PID, s.DebugPort = false, 0, 0 })
				p.log.Info("stopped")
				return
			}

			select {
			case <-time.After(backoff):
			case <-pr.stopCh:
				return
			}
			backoff = min(backoff*2, 30*time.Second)
		}
	}()
	return pr
}

// signal sends sig to the process, or to its whole process group (Delve and
// the program it runs).
func signal(cmd *exec.Cmd, sig syscall.Signal, group bool) {
	if cmd.Process == nil {
		return
	}
	if group {
		_ = syscall.Kill(-cmd.Process.Pid, sig)
		return
	}
	_ = cmd.Process.Signal(sig)
}

// killDescendants SIGKILLs all descendants of pid (Linux /proc; a no-op
// elsewhere, where debug mode isn't supported anyway).
func killDescendants(pid int) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	children := map[int][]int{}
	for _, e := range entries {
		child, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// "pid (comm) state ppid ...": comm may contain spaces and parens.
		fields := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+1:]))
		if len(fields) < 2 {
			continue
		}
		if ppid, err := strconv.Atoi(fields[1]); err == nil {
			children[ppid] = append(children[ppid], child)
		}
	}
	var kill func(int)
	kill = func(p int) {
		for _, c := range children[p] {
			kill(c)
			_ = syscall.Kill(c, syscall.SIGKILL)
		}
	}
	kill(pid)
}

// lineWriter logs process output line by line.
type lineWriter struct {
	log *slog.Logger
	mu  sync.Mutex
	buf []byte
}

func (w *lineWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, b...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.log.Info(string(w.buf[:i]), "source", "output")
		w.buf = w.buf[i+1:]
	}
	return len(b), nil
}

func (w *lineWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.log.Info(string(w.buf), "source", "output")
		w.buf = nil
	}
}

func fileHash(path string) ([sha256.Size]byte, error) {
	var sum [sha256.Size]byte
	f, err := os.Open(path)
	if err != nil {
		return sum, err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

// GoVersion returns the version of the installed Go toolchain, e.g. "1.24.3".
func GoVersion() (string, error) {
	out, err := exec.Command("go", "env", "GOVERSION").Output()
	if err != nil {
		return "", fmt.Errorf("go toolchain not found: %w", err)
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "go"), nil
}
