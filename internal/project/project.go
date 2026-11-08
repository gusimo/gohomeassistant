package project

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
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
}

func newProject(m *Manager, name string) *Project {
	return &Project{
		m:       m,
		name:    name,
		dir:     filepath.Join(m.cfg.Root, name),
		log:     m.cfg.Log.With("project", name),
		trigger: make(chan struct{}, 1),
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
		if bin, err := p.build(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			if proc != nil {
				p.log.Error("build failed, keeping previous version running", "err", err)
			} else {
				p.log.Error("build failed", "err", err)
			}
		} else {
			p.log.Info("build ok", "took", time.Since(start).Round(time.Millisecond))
			if proc != nil {
				proc.stop()
			}
			proc = p.supervise(bin)
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
func (p *Project) build(ctx context.Context) (string, error) {
	out := filepath.Join(p.m.cfg.BuildDir, p.name)
	if err := os.MkdirAll(out, 0o755); err != nil {
		return "", err
	}
	work := filepath.Join(out, "go.work")
	content := fmt.Sprintf("go %s\n\nuse (\n\t%s\n\t%s\n)\n", p.m.goVersion, p.dir, p.m.cfg.SDKDir)
	if err := os.WriteFile(work, []byte(content), 0o644); err != nil {
		return "", err
	}
	// New name per build: a running binary is never overwritten.
	bin := filepath.Join(out, fmt.Sprintf("app-%d", time.Now().UnixNano()))
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, ".")
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
	stopCh   chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func (pr *process) stop() {
	pr.stopOnce.Do(func() { close(pr.stopCh) })
	<-pr.done
}

// supervise runs bin and restarts it with backoff when it exits on its own.
func (p *Project) supervise(bin string) *process {
	pr := &process{stopCh: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(pr.done)
		defer os.Remove(bin)
		backoff := time.Second
		for {
			started := time.Now()
			cmd := exec.Command(bin)
			cmd.Dir = p.dir
			cmd.Env = append(os.Environ(), "GHA_SOCKET="+p.m.cfg.Socket, "GHA_PROJECT="+p.name)
			lw := &lineWriter{log: p.log}
			cmd.Stdout, cmd.Stderr = lw, lw

			exited := make(chan error, 1)
			if err := cmd.Start(); err != nil {
				exited <- err
			} else {
				p.log.Info("started", "pid", cmd.Process.Pid)
				go func() { exited <- cmd.Wait() }()
			}

			select {
			case err := <-exited:
				lw.Flush()
				p.log.Warn("exited", "err", err, "restart_in", backoff)
				if time.Since(started) > 10*time.Second {
					backoff = time.Second
				}
			case <-pr.stopCh:
				_ = cmd.Process.Signal(syscall.SIGTERM)
				select {
				case <-exited:
				case <-time.After(5 * time.Second):
					p.log.Warn("did not stop in time, killing")
					_ = cmd.Process.Kill()
					<-exited
				}
				lw.Flush()
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

func goVersion() (string, error) {
	out, err := exec.Command("go", "env", "GOVERSION").Output()
	if err != nil {
		return "", fmt.Errorf("go toolchain not found: %w", err)
	}
	return strings.TrimPrefix(strings.TrimSpace(string(out)), "go"), nil
}
