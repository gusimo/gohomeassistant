// Package server serves the web UI and its API.
//
// All UI and API URLs are relative, so the UI works behind Home Assistant
// Ingress, which serves it under a changing path prefix.
package server

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gusimo/gohomeassistant/internal/auth"
	"github.com/gusimo/gohomeassistant/internal/ha"
	"github.com/gusimo/gohomeassistant/internal/logbus"
	"github.com/gusimo/gohomeassistant/internal/project"
)

// dist/app is written by the frontend build (web/, `npm run build`).
//
//go:embed all:dist
var dist embed.FS

const maxFileSize = 1 << 20

var projectName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

type Server struct {
	mgr       *project.Manager
	bus       *logbus.Bus
	log       *slog.Logger
	goVersion string
	gopls     string // path, "" if not installed

	// SkipOriginCheck disables the WebSocket origin check (HA Ingress).
	SkipOriginCheck bool
	// Auth protects all routes; nil means no authentication (tests).
	Auth *auth.Auth
	// HA and Refs power the entity browser (optional).
	HA   *ha.Client
	Refs func() map[string]string
}

func New(mgr *project.Manager, bus *logbus.Bus, goVersion string, log *slog.Logger) *Server {
	gopls, err := exec.LookPath("gopls")
	if err != nil {
		log.Warn("gopls not found: no code completion in the editor")
	}
	return &Server{mgr: mgr, bus: bus, goVersion: goVersion, log: log, gopls: gopls}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	if s.Auth == nil {
		s.routes(mux)
		return mux
	}
	s.Auth.Register(mux)
	s.routes(mux)
	return s.Auth.Wrap(mux)
}

func (s *Server) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/projects", s.listProjects)
	mux.HandleFunc("POST /api/projects", s.createProject)
	mux.HandleFunc("DELETE /api/projects/{name}", s.deleteProject)
	mux.HandleFunc("POST /api/projects/{name}/build", s.build)
	mux.HandleFunc("GET /api/projects/{name}/files", s.listFiles)
	mux.HandleFunc("GET /api/projects/{name}/files/{path...}", s.readFile)
	mux.HandleFunc("PUT /api/projects/{name}/files/{path...}", s.writeFile)
	mux.HandleFunc("DELETE /api/projects/{name}/files/{path...}", s.deleteFile)
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("GET /api/projects/{name}/lsp", s.lsp)
	mux.HandleFunc("GET /api/entities", s.entities)
	mux.HandleFunc("POST /api/projects/{name}/debug", s.setDebug)
	mux.HandleFunc("GET /api/projects/{name}/dap", s.dap)

	if app, err := fs.Sub(dist, "dist/app"); err == nil {
		if _, err := fs.Stat(app, "index.html"); err == nil {
			mux.Handle("GET /", http.FileServerFS(app))
			return
		}
	}
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "UI not built: run `npm run build` in web/", http.StatusNotFound)
	})
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.mgr.Statuses())
}

const mainTemplate = `package main

import (
	"log"

	ha "github.com/gusimo/gohomeassistant/sdk"
)

func main() {
	ha.MustConnect()
	log.Println("started")

	// Typed entities: import "github.com/gusimo/gohomeassistant/ent", then e.g.
	//
	//	ent.BinarySensor.FrontDoor.OnChange(func(ch ha.StateChange) {
	//		if ent.BinarySensor.FrontDoor.IsOn() {
	//			ent.Light.Hall.TurnOn(ent.LightTurnOn{BrightnessPct: 80})
	//		}
	//	})

	ha.Default().Run()
}
`

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !projectName.MatchString(req.Name) {
		http.Error(w, "name must match "+projectName.String(), http.StatusBadRequest)
		return
	}
	dir := filepath.Join(s.mgr.Root(), req.Name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		if errors.Is(err, fs.ErrExist) {
			http.Error(w, "project exists", http.StatusConflict)
			return
		}
		httpError(w, err)
		return
	}
	mod := fmt.Sprintf("module %s\n\ngo %s\n", req.Name, s.goVersion)
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(mainTemplate), 0o644); err != nil {
		httpError(w, err)
		return
	}
	// go.mod last: it is what makes the directory a project.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o644); err != nil {
		httpError(w, err)
		return
	}
	s.mgr.Rescan()
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.projectDir(w, r)
	if !ok {
		return
	}
	if err := os.RemoveAll(dir); err != nil {
		httpError(w, err)
		return
	}
	s.mgr.Rescan()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) build(w http.ResponseWriter, r *http.Request) {
	if !s.mgr.Trigger(r.PathValue("name")) {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

type fileInfo struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.projectDir(w, r)
	if !ok {
		return
	}
	files := []fileInfo{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".") && p != dir {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		files = append(files, fileInfo{Path: filepath.ToSlash(rel), Size: info.Size()})
		return nil
	})
	if err != nil {
		httpError(w, err)
		return
	}
	writeJSON(w, files)
}

func (s *Server) readFile(w http.ResponseWriter, r *http.Request) {
	p, ok := s.filePath(w, r)
	if !ok {
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		httpError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write(b)
}

func (s *Server) writeFile(w http.ResponseWriter, r *http.Request) {
	p, ok := s.filePath(w, r)
	if !ok {
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxFileSize))
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		httpError(w, err)
		return
	}
	// Atomic replace: a build running right now never sees half a file.
	tmp := filepath.Join(filepath.Dir(p), ".gha-tmp-"+filepath.Base(p))
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		httpError(w, err)
		return
	}
	if err := os.Rename(tmp, p); err != nil {
		httpError(w, err)
		return
	}
	s.mgr.Trigger(r.PathValue("name")) // don't wait for the file watcher
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request) {
	p, ok := s.filePath(w, r)
	if !ok {
		return
	}
	if filepath.Base(p) == "go.mod" && filepath.Dir(p) == filepath.Join(s.mgr.Root(), r.PathValue("name")) {
		http.Error(w, "delete the project instead of its go.mod", http.StatusBadRequest)
		return
	}
	if err := os.Remove(p); err != nil {
		httpError(w, err)
		return
	}
	s.mgr.Trigger(r.PathValue("name"))
	w.WriteHeader(http.StatusNoContent)
}

// events streams logs and status changes as server-sent events. It starts
// with the log history and the current status of every project.
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // proxies, e.g. HA Ingress

	ch, unsubscribe := s.bus.Subscribe() // before reading history: nothing is lost
	defer unsubscribe()
	send := func(ev logbus.Event) error {
		b, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "data: %s\n\n", b)
		return err
	}
	lastSeq := int64(0)
	for _, e := range s.bus.History() {
		send(logbus.Event{Type: "log", Log: &e})
		lastSeq = e.Seq
	}
	for _, st := range s.mgr.Statuses() {
		send(logbus.Event{Type: "status", Status: st})
	}
	flusher.Flush()

	keepalive := time.NewTicker(25 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
		case ev := <-ch:
			if ev.Log != nil && ev.Log.Seq <= lastSeq {
				continue // already sent as history
			}
			if send(ev) != nil {
				return
			}
		}
		flusher.Flush()
	}
}

func (s *Server) projectDir(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	dir := filepath.Join(s.mgr.Root(), name)
	if !projectName.MatchString(name) {
		http.Error(w, "invalid project name", http.StatusBadRequest)
		return "", false
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		http.NotFound(w, r)
		return "", false
	}
	return dir, true
}

// filePath resolves a file path inside a project. It rejects anything that
// could leave the project or touch hidden files (.git, ...).
func (s *Server) filePath(w http.ResponseWriter, r *http.Request) (string, bool) {
	dir, ok := s.projectDir(w, r)
	if !ok {
		return "", false
	}
	rel := r.PathValue("path")
	clean := path.Clean("/" + rel)[1:]
	if clean == "" || clean != rel {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return "", false
	}
	for _, part := range strings.Split(clean, "/") {
		if strings.HasPrefix(part, ".") {
			http.Error(w, "hidden files are not accessible", http.StatusBadRequest)
			return "", false
		}
	}
	return filepath.Join(dir, filepath.FromSlash(clean)), true
}

// entities returns all current states, each with the Go expression of the
// generated entity (e.g. "ent.Light.Kitchen").
func (s *Server) entities(w http.ResponseWriter, r *http.Request) {
	if s.HA == nil {
		http.Error(w, "not available", http.StatusNotImplemented)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	raw, err := s.HA.Call(ctx, map[string]any{"type": "get_states"})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	var res struct {
		Result []map[string]any `json:"result"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		httpError(w, err)
		return
	}
	var refs map[string]string
	if s.Refs != nil {
		refs = s.Refs()
	}
	for _, st := range res.Result {
		id, _ := st["entity_id"].(string)
		st["ref"] = refs[id]
		delete(st, "context")
	}
	sort.Slice(res.Result, func(i, j int) bool {
		a, _ := res.Result[i]["entity_id"].(string)
		b, _ := res.Result[j]["entity_id"].(string)
		return a < b
	})
	writeJSON(w, res.Result)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		http.Error(w, "not found", http.StatusNotFound)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
