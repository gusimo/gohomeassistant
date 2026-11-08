package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

func (s *Server) setDebug(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.mgr.SetDebug(r.PathValue("name"), req.Enabled); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusNotImplemented)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// dap connects the UI's debugger to the project's headless Delve (Debug
// Adapter Protocol). Like lsp: one message per WebSocket frame, Content-Length
// framing towards Delve.
func (s *Server) dap(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	port := 0
	for _, st := range s.mgr.Statuses() {
		if st.Name == name {
			port = st.DebugPort
		}
	}
	if port == 0 {
		http.Error(w, "project is not running under the debugger", http.StatusConflict)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// The UI connects as soon as the status reports the port, which can be
	// a few milliseconds before Delve listens.
	var tcp net.Conn
	var err error
	for deadline := time.Now().Add(5 * time.Second); ; {
		var d net.Dialer
		if tcp, err = d.DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port)); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		http.Error(w, "debugger not reachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer tcp.Close()
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: s.SkipOriginCheck})
	if err != nil {
		return
	}
	defer conn.CloseNow()
	conn.SetReadLimit(16 << 20)

	go func() { // Delve -> browser
		defer cancel()
		rd := bufio.NewReader(tcp)
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
	for { // browser -> Delve
		_, msg, err := conn.Read(ctx)
		if err != nil {
			return
		}
		if _, err := fmt.Fprintf(tcp, "Content-Length: %d\r\n\r\n%s", len(msg), msg); err != nil {
			return
		}
	}
}
