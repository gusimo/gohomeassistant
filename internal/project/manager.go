// Package project discovers, builds and runs GoHomeAssistant projects.
//
// Every directory below the projects root that contains a go.mod is a
// project. On every change to a .go/go.mod/go.sum file the project is
// rebuilt; only if the build succeeds is the running process replaced.
// A failed build leaves the previous version running.
package project

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

type Config struct {
	Root     string   // projects directory
	BuildDir string   // build outputs, outside the projects (keeps git clean)
	Modules  []string // extra workspace modules (SDK, generated entities)
	Socket   string   // bridge socket, passed to projects as GHA_SOCKET
	Log      *slog.Logger
	// Poll, if > 0, additionally scans for changes at this interval. Needed
	// where file events don't arrive, e.g. Docker bind mounts on macOS/Windows.
	Poll time.Duration
	// OnStatus, if set, is called on every project status change.
	OnStatus func(Status)
	// OnFiles, if set, is called when files of a project change on disk.
	OnFiles func(project string)
}

type Manager struct {
	cfg       Config
	goVersion string

	mu       sync.Mutex
	projects map[string]*Project
	watcher  *fsnotify.Watcher
	wg       sync.WaitGroup
	ctx      context.Context // of Run, for projects started by Rescan
	dlv      string          // path of Delve, "" if not installed
}

func NewManager(cfg Config) (*Manager, error) {
	gv, err := GoVersion()
	if err != nil {
		return nil, err
	}
	for _, d := range []string{cfg.Root, cfg.BuildDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	// go.work paths must match the real paths the go tool sees.
	if cfg.Root, err = filepath.EvalSymlinks(cfg.Root); err != nil {
		return nil, err
	}
	mods := make([]string, len(cfg.Modules))
	for i, m := range cfg.Modules {
		if mods[i], err = filepath.EvalSymlinks(m); err != nil {
			return nil, err
		}
	}
	cfg.Modules = mods
	dlv, _ := exec.LookPath("dlv")
	return &Manager{cfg: cfg, goVersion: gv, projects: map[string]*Project{}, dlv: dlv}, nil
}

// Run watches the projects root until ctx is cancelled, then stops all
// projects.
func (m *Manager) Run(ctx context.Context) error {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	defer w.Close()
	m.mu.Lock()
	m.watcher = w
	m.ctx = ctx
	m.mu.Unlock()
	if err := w.Add(m.cfg.Root); err != nil {
		return err
	}
	m.rescan(ctx)

	var poll <-chan time.Time
	if m.cfg.Poll > 0 {
		t := time.NewTicker(m.cfg.Poll)
		defer t.Stop()
		poll = t.C
		m.cfg.Log.Info("polling for changes", "interval", m.cfg.Poll)
	}

	for {
		select {
		case <-ctx.Done():
			m.wg.Wait()
			return nil
		case <-poll:
			m.poll(ctx)
		case err := <-w.Errors:
			m.cfg.Log.Warn("watcher error", "err", err)
		case ev := <-w.Events:
			m.handleFSEvent(ctx, ev)
		}
	}
}

func (m *Manager) handleFSEvent(ctx context.Context, ev fsnotify.Event) {
	rel, err := filepath.Rel(m.cfg.Root, ev.Name)
	if err != nil || rel == "." {
		return
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if hidden(parts) {
		return
	}
	if ev.Has(fsnotify.Create) {
		if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
			m.watchTree(ev.Name)
		}
	}

	m.mu.Lock()
	p := m.projects[parts[0]]
	m.mu.Unlock()
	if p == nil || len(parts) == 1 || ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
		m.rescan(ctx) // project may have been added or removed
	}
	if p != nil && relevant(ev.Name) {
		p.Trigger()
	}
	if p != nil && len(parts) > 1 && m.cfg.OnFiles != nil {
		m.cfg.OnFiles(parts[0])
	}
}

// rescan starts new projects and stops removed ones.
func (m *Manager) rescan(ctx context.Context) {
	entries, err := os.ReadDir(m.cfg.Root)
	if err != nil {
		m.cfg.Log.Error("read projects dir", "err", err)
		return
	}
	found := map[string]bool{}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(m.cfg.Root, e.Name())
		m.watchTree(dir) // so a go.mod created later is noticed
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			found[e.Name()] = true
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for name, p := range m.projects {
		if !found[name] {
			m.cfg.Log.Info("project removed", "project", name)
			p.mu.Lock()
			p.removed = true
			p.mu.Unlock()
			p.cancel()
			delete(m.projects, name)
			if m.cfg.OnStatus != nil {
				m.cfg.OnStatus(Status{Name: name, Removed: true})
			}
		}
	}
	for name := range found {
		if _, ok := m.projects[name]; ok {
			continue
		}
		m.cfg.Log.Info("project found", "project", name)
		p := newProject(m, name)
		pctx, cancel := context.WithCancel(ctx)
		p.cancel = cancel
		m.projects[name] = p
		m.wg.Add(1)
		go func() { defer m.wg.Done(); p.loop(pctx) }()
	}
}

// Workspace returns the directory of a project and the go.work file that
// makes the SDK and generated entities available to it (for gopls).
func (m *Manager) Workspace(name string) (dir, gowork string, err error) {
	gowork, err = m.writeWork(name)
	return filepath.Join(m.cfg.Root, name), gowork, err
}

func (m *Manager) writeWork(name string) (string, error) {
	out := filepath.Join(m.cfg.BuildDir, name)
	if err := os.MkdirAll(out, 0o755); err != nil {
		return "", err
	}
	work := filepath.Join(out, "go.work")
	content := fmt.Sprintf("go %s\n\nuse (\n\t%s\n", m.goVersion, filepath.Join(m.cfg.Root, name))
	for _, mod := range m.cfg.Modules {
		content += "\t" + mod + "\n"
	}
	content += ")\n"
	_, err := writeIfChanged(work, []byte(content))
	return work, err
}

// writeIfChanged avoids touching go.work on every build: gopls watches it.
func writeIfChanged(path string, data []byte) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return false, nil
	}
	return true, os.WriteFile(path, data, 0o644)
}

// Root returns the (symlink-resolved) projects directory.
func (m *Manager) Root() string { return m.cfg.Root }

// Statuses returns the status of all projects, sorted by name.
func (m *Manager) Statuses() []Status {
	m.mu.Lock()
	out := make([]Status, 0, len(m.projects))
	for _, p := range m.projects {
		out = append(out, p.Status())
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Rescan picks up added or removed projects immediately.
func (m *Manager) Rescan() {
	m.mu.Lock()
	ctx := m.ctx
	m.mu.Unlock()
	if ctx != nil {
		m.rescan(ctx)
	}
}

// Trigger rebuilds one project. It returns false if the project is unknown.
func (m *Manager) Trigger(name string) bool {
	m.mu.Lock()
	p := m.projects[name]
	m.mu.Unlock()
	if p != nil {
		p.Trigger()
	}
	return p != nil
}

// SetDebug switches debug mode of a project.
func (m *Manager) SetDebug(name string, on bool) error {
	if on && m.dlv == "" {
		return errors.New("Delve (dlv) is not installed")
	}
	m.mu.Lock()
	p := m.projects[name]
	m.mu.Unlock()
	if p == nil {
		return fs.ErrNotExist
	}
	p.SetDebug(on)
	return nil
}

// debugCommand runs bin under a headless Delve that keeps the program
// running (--continue) and accepts debugger clients (DAP) at any time.
func (m *Manager) debugCommand(bin string) (*exec.Cmd, int, error) {
	port, err := freePort()
	if err != nil {
		return nil, 0, err
	}
	cmd := exec.Command(m.dlv, "exec", bin, "--headless", "--accept-multiclient", "--continue",
		"--api-version=2", fmt.Sprintf("--listen=127.0.0.1:%d", port))
	// Own process group, so stopping also reaches the debugged program.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, port, nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// TriggerAll rebuilds all projects, e.g. after the entities were regenerated.
func (m *Manager) TriggerAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.projects {
		p.Trigger()
	}
}

// poll rescans for added/removed projects and triggers a rebuild of every
// project whose source files changed since the last poll.
func (m *Manager) poll(ctx context.Context) {
	m.rescan(ctx)
	m.mu.Lock()
	projects := make([]*Project, 0, len(m.projects))
	for _, p := range m.projects {
		projects = append(projects, p)
	}
	m.mu.Unlock()
	for _, p := range projects {
		sig := signature(p.dir)
		if p.sig != 0 && sig != p.sig {
			p.Trigger()
			if m.cfg.OnFiles != nil {
				m.cfg.OnFiles(p.name)
			}
		}
		p.sig = sig
	}
}

// signature hashes path, size and mtime of all relevant files in dir.
func signature(dir string) uint64 {
	h := fnv.New64a()
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && path != dir && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if d.IsDir() || !relevant(path) {
			return nil
		}
		if fi, err := d.Info(); err == nil {
			fmt.Fprintf(h, "%s|%d|%d\n", path, fi.Size(), fi.ModTime().UnixNano())
		}
		return nil
	})
	return h.Sum64()
}

func (m *Manager) watchTree(dir string) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if path != dir && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if err := m.watcher.Add(path); err != nil {
			m.cfg.Log.Warn("watch", "path", path, "err", err)
		}
		return nil
	})
}

func hidden(parts []string) bool {
	for _, p := range parts {
		if strings.HasPrefix(p, ".") {
			return true
		}
	}
	return false
}

func relevant(path string) bool {
	base := filepath.Base(path)
	return strings.HasSuffix(base, ".go") || base == "go.mod" || base == "go.sum"
}

// debounce collects bursts of triggers (editors write several files).
const debounce = 300 * time.Millisecond
