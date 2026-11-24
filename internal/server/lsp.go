package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/gusimo/gohomeassistant/internal/logbus"
)

// lsp connects the editor to a gopls process for one project. Each WebSocket
// text message is one JSON-RPC message; towards gopls they are framed with
// Content-Length headers (LSP base protocol).
func (s *Server) lsp(w http.ResponseWriter, r *http.Request) {
	if s.gopls == "" {
		http.Error(w, "gopls not installed", http.StatusNotImplemented)
		return
	}
	if _, ok := s.projectDir(w, r); !ok {
		return
	}
	dir, gowork, err := s.mgr.Workspace(r.PathValue("name"))
	if err != nil {
		httpError(w, err)
		return
	}
	// Under Ingress the browser's Origin is HA's host, not ours; only the
	// Supervisor can reach us there.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: s.SkipOriginCheck})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(16 << 20)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	cmd := exec.CommandContext(ctx, s.gopls, "serve")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK="+gowork)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	cmd.Stderr = &lineLogger{log: s.log.With("project", r.PathValue("name"), "component", "gopls")}
	if err := cmd.Start(); err != nil {
		s.log.Error("start gopls", "err", err)
		conn.Close(websocket.StatusInternalError, "cannot start gopls")
		return
	}
	defer func() {
		// Kill gopls first: Wait would otherwise block forever.
		cancel()
		stdin.Close()
		cmd.Wait()
	}()
	var stdinMu sync.Mutex
	write := func(msg []byte) error {
		stdinMu.Lock()
		defer stdinMu.Unlock()
		_, err := fmt.Fprintf(stdin, "Content-Length: %d\r\n\r\n%s", len(msg), msg)
		return err
	}
	go s.watchFiles(ctx, r.PathValue("name"), dir, write)

	go func() { // gopls -> browser
		defer cancel()
		rd := bufio.NewReader(stdout)
		for {
			msg, err := readFramed(rd)
			if err != nil {
				return
			}
			if err := conn.Write(ctx, websocket.MessageText, msg); err != nil {
				return
			}
		}
	}()

	for { // browser -> gopls
		_, msg, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if err := write(msg); err != nil {
			return
		}
	}
}

// watchFiles tells gopls about files that changed on disk. gopls relies on
// its client for that; without it, completion wouldn't know entities added
// to Home Assistant, or files changed by git or another editor, until the
// page is reloaded.
func (s *Server) watchFiles(ctx context.Context, project, dir string, write func([]byte) error) {
	events, unsubscribe := s.bus.Subscribe()
	defer unsubscribe()
	known := sourceFiles(dir)
	for {
		var ent, files bool
		note := func(ev logbus.Event) {
			ent = ent || ev.Type == "entities"
			files = files || (ev.Type == "files" && ev.Project == project)
		}
		select {
		case <-ctx.Done():
			return
		case ev := <-events:
			note(ev)
		}
		if !ent && !files {
			continue
		}
		for settle := time.After(200 * time.Millisecond); settle != nil; { // bursts
			select {
			case <-ctx.Done():
				return
			case ev := <-events:
				note(ev)
			case <-settle:
				settle = nil
			}
		}

		const created, changed, deleted = 1, 2, 3 // LSP FileChangeType
		type change struct {
			URI  string `json:"uri"`
			Type int    `json:"type"`
		}
		var changes []change
		if files {
			now := sourceFiles(dir)
			for path, sig := range now {
				if old, ok := known[path]; !ok {
					changes = append(changes, change{fileURI(path), created})
				} else if old != sig {
					changes = append(changes, change{fileURI(path), changed})
				}
			}
			for path := range known {
				if _, ok := now[path]; !ok {
					changes = append(changes, change{fileURI(path), deleted})
				}
			}
			known = now
		}
		if ent && s.EntDir != "" {
			changes = append(changes,
				change{fileURI(filepath.Join(s.EntDir, "ent.go")), changed},
				change{fileURI(filepath.Join(s.EntDir, "p", "p.go")), changed})
		}
		if len(changes) == 0 {
			continue
		}
		msg, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "method": "workspace/didChangeWatchedFiles", "params": map[string]any{"changes": changes},
		})
		if write(msg) != nil {
			return
		}
	}
}

func fileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}

// sourceFiles returns size and modification time of the files gopls reads.
func sourceFiles(dir string) map[string]string {
	out := map[string]string{}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && path != dir && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if base := d.Name(); !d.IsDir() && (strings.HasSuffix(base, ".go") || base == "go.mod" || base == "go.sum") {
			if fi, err := d.Info(); err == nil {
				out[path] = fmt.Sprintf("%d|%d", fi.Size(), fi.ModTime().UnixNano())
			}
		}
		return nil
	})
	return out
}

// readFramed reads one Content-Length framed message (LSP and DAP use the
// same base protocol).
func readFramed(rd *bufio.Reader) ([]byte, error) {
	hdr, err := textproto.NewReader(rd).ReadMIMEHeader()
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(hdr.Get("Content-Length"))
	if err != nil || n < 0 || n > 64<<20 {
		return nil, fmt.Errorf("bad Content-Length %q", hdr.Get("Content-Length"))
	}
	msg := make([]byte, n)
	_, err = io.ReadFull(rd, msg)
	return msg, err
}

// lineLogger logs a process' stderr at debug level, line by line.
type lineLogger struct {
	log interface{ Debug(string, ...any) }
	buf string
}

func (l *lineLogger) Write(b []byte) (int, error) {
	l.buf += string(b)
	for {
		i := strings.IndexByte(l.buf, '\n')
		if i < 0 {
			return len(b), nil
		}
		l.log.Debug(l.buf[:i])
		l.buf = l.buf[i+1:]
	}
}
