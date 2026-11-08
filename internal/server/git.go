package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gusimo/gohomeassistant/internal/auth"
)

// Git integration: every project directory is its own repository. We use
// the git CLI; all paths are validated like the file API and passed after
// "--", revisions must look like hashes.

var revPattern = regexp.MustCompile(`^(HEAD|[0-9a-f]{4,40})$`)

type gitFile struct {
	Path   string `json:"path"`
	Status string `json:"status"` // modified, added, deleted, renamed, untracked
}

type gitCommit struct {
	Hash    string `json:"hash"`
	Short   string `json:"short"`
	Author  string `json:"author"`
	Date    string `json:"date"`
	Message string `json:"message"`
}

func (s *Server) git(ctx context.Context, dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// safe.directory: projects may be owned by another user (bind mounts).
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "safe.directory=*", "-c", "core.quotepath=off"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(stderr.String()+" "+err.Error()))
	}
	return out, nil
}

func isRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git", "HEAD"))
	return err == nil
}

// gitInit creates a repository with an initial commit of all files.
func (s *Server) gitInit(ctx context.Context, dir string, user auth.User) error {
	if _, err := s.git(ctx, dir, "init", "-b", "main"); err != nil {
		return err
	}
	if _, err := s.git(ctx, dir, "add", "-A"); err != nil {
		return err
	}
	_, err := s.commit(ctx, dir, user, "Create project", nil)
	return err
}

func (s *Server) commit(ctx context.Context, dir string, user auth.User, msg string, paths []string) ([]byte, error) {
	name := user.Name
	if name == "" {
		name = "GoHomeAssistant"
	}
	email := os.Getenv("GHA_GIT_EMAIL")
	if email == "" {
		email = "gohomeassistant@localhost"
	}
	args := []string{"-c", "user.name=" + name, "-c", "user.email=" + email, "commit", "--no-verify", "-m", msg}
	if len(paths) > 0 {
		args = append(append(args, "--"), paths...)
	}
	return s.git(ctx, dir, args...)
}

func (s *Server) gitStatus(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.projectDir(w, r)
	if !ok {
		return
	}
	if !isRepo(dir) {
		writeJSON(w, map[string]any{"repo": false})
		return
	}
	out, err := s.git(r.Context(), dir, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		httpError(w, err)
		return
	}
	files := []gitFile{}
	entries := strings.Split(string(out), "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		x, y, path := e[0], e[1], e[3:]
		st := "modified"
		switch {
		case x == '?':
			st = "untracked"
		case x == 'R' || y == 'R':
			st = "renamed"
			i++ // the old path follows
		case x == 'A':
			st = "added"
		case x == 'D' || y == 'D':
			st = "deleted"
		}
		files = append(files, gitFile{Path: path, Status: st})
	}
	branch, _ := s.git(r.Context(), dir, "symbolic-ref", "--short", "HEAD")
	head, _ := s.git(r.Context(), dir, "rev-parse", "--short", "HEAD")
	writeJSON(w, map[string]any{
		"repo":   true,
		"branch": strings.TrimSpace(string(branch)),
		"head":   strings.TrimSpace(string(head)),
		"files":  files,
	})
}

func (s *Server) gitInitHandler(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.projectDir(w, r)
	if !ok {
		return
	}
	if isRepo(dir) {
		http.Error(w, "already a git repository", http.StatusConflict)
		return
	}
	user, _ := auth.UserFrom(r.Context())
	if err := s.gitInit(r.Context(), dir, user); err != nil {
		httpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) gitCommit(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.repoDir(w, r)
	if !ok {
		return
	}
	var req struct {
		Message string   `json:"message"`
		Paths   []string `json:"paths"` // empty = all changes
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Message) == "" {
		http.Error(w, "commit message required", http.StatusBadRequest)
		return
	}
	for _, p := range req.Paths {
		if !validRelPath(p) {
			http.Error(w, "invalid path "+p, http.StatusBadRequest)
			return
		}
	}
	add := append([]string{"add", "-A", "--"}, req.Paths...)
	if _, err := s.git(r.Context(), dir, add...); err != nil {
		httpError(w, err)
		return
	}
	user, _ := auth.UserFrom(r.Context())
	if _, err := s.commit(r.Context(), dir, user, req.Message, req.Paths); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest) // e.g. nothing to commit
		return
	}
	s.bus.PublishFiles(r.PathValue("name")) // status changed
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) gitLog(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.repoDir(w, r)
	if !ok {
		return
	}
	out, err := s.git(r.Context(), dir, "log", "-n", "200", "--format=%H%x1f%h%x1f%an%x1f%aI%x1f%s%x1e")
	commits := []gitCommit{}
	if err != nil { // no commits yet
		writeJSON(w, commits)
		return
	}
	for _, rec := range strings.Split(string(out), "\x1e") {
		f := strings.Split(strings.TrimSpace(rec), "\x1f")
		if len(f) == 5 {
			commits = append(commits, gitCommit{Hash: f[0], Short: f[1], Author: f[2], Date: f[3], Message: f[4]})
		}
	}
	writeJSON(w, commits)
}

// gitCommitFiles lists the files a commit changed.
func (s *Server) gitCommitFiles(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.repoDir(w, r)
	if !ok {
		return
	}
	rev := r.PathValue("rev")
	if !revPattern.MatchString(rev) {
		http.Error(w, "invalid revision", http.StatusBadRequest)
		return
	}
	out, err := s.git(r.Context(), dir, "show", "--format=", "--name-status", "-z", "--root", rev)
	if err != nil {
		httpError(w, err)
		return
	}
	files := []gitFile{}
	f := strings.Split(string(out), "\x00")
	for i := 0; i+1 < len(f); i += 2 {
		if f[i] == "" {
			break
		}
		st := map[byte]string{'A': "added", 'D': "deleted", 'M': "modified", 'R': "renamed"}[f[i][0]]
		if f[i][0] == 'R' && i+2 < len(f) {
			i++ // old path, then new path
		}
		files = append(files, gitFile{Path: f[i+1], Status: st})
	}
	writeJSON(w, files)
}

// gitShow returns a file's content at a revision ("" if it didn't exist).
// rev may be "HEAD", a hash, or "hash^" for the parent of a commit.
func (s *Server) gitShow(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.repoDir(w, r)
	if !ok {
		return
	}
	rev, path := r.URL.Query().Get("rev"), r.URL.Query().Get("path")
	if !revPattern.MatchString(strings.TrimSuffix(rev, "^")) || !validRelPath(path) {
		http.Error(w, "invalid revision or path", http.StatusBadRequest)
		return
	}
	out, err := s.git(r.Context(), dir, "show", rev+":"+path)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if err != nil {
		w.Header().Set("X-Exists", "0")
		return
	}
	w.Write(out)
}

// gitDiscard throws away uncommitted changes of one file.
func (s *Server) gitDiscard(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.repoDir(w, r)
	if !ok {
		return
	}
	var req struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !validRelPath(req.Path) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if _, err := s.git(r.Context(), dir, "cat-file", "-e", "HEAD:"+req.Path); err != nil {
		// Not in HEAD: a new file; discarding means deleting it.
		_, _ = s.git(r.Context(), dir, "rm", "--cached", "--quiet", "--ignore-unmatch", "--", req.Path)
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(req.Path))); err != nil && !errors.Is(err, os.ErrNotExist) {
			httpError(w, err)
			return
		}
	} else if _, err := s.git(r.Context(), dir, "checkout", "HEAD", "--", req.Path); err != nil {
		httpError(w, err)
		return
	}
	s.mgr.Trigger(r.PathValue("name"))
	s.bus.PublishFiles(r.PathValue("name"))
	w.WriteHeader(http.StatusNoContent)
}

// gitRestore writes a file's content from a commit into the working tree
// (uncommitted, so it shows up as a change).
func (s *Server) gitRestore(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.repoDir(w, r)
	if !ok {
		return
	}
	var req struct {
		Path string `json:"path"`
		Rev  string `json:"rev"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !validRelPath(req.Path) || !revPattern.MatchString(req.Rev) {
		http.Error(w, "invalid path or revision", http.StatusBadRequest)
		return
	}
	if _, err := s.git(r.Context(), dir, "checkout", req.Rev, "--", req.Path); err != nil {
		httpError(w, err)
		return
	}
	// checkout also stages; keep it a plain working-tree change.
	_, _ = s.git(r.Context(), dir, "reset", "--quiet", "--", req.Path)
	s.mgr.Trigger(r.PathValue("name"))
	s.bus.PublishFiles(r.PathValue("name"))
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) repoDir(w http.ResponseWriter, r *http.Request) (string, bool) {
	dir, ok := s.projectDir(w, r)
	if !ok {
		return "", false
	}
	if !isRepo(dir) {
		http.Error(w, "not a git repository", http.StatusConflict)
		return "", false
	}
	return dir, true
}

// validRelPath mirrors filePath's rules: relative, clean, no hidden parts.
func validRelPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "-") {
		return false
	}
	if c := filepath.ToSlash(filepath.Clean(p)); c != p {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." || strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}
