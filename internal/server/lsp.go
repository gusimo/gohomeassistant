package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/coder/websocket"
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
	conn, err := websocket.Accept(w, r, nil)
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

	go func() { // gopls -> browser
		defer cancel()
		rd := bufio.NewReader(stdout)
		for {
			msg, err := readLSP(rd)
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
		if _, err := fmt.Fprintf(stdin, "Content-Length: %d\r\n\r\n%s", len(msg), msg); err != nil {
			return
		}
	}
}

func readLSP(rd *bufio.Reader) ([]byte, error) {
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
