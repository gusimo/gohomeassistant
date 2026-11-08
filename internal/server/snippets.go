package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

// Snippet is a user-defined code snippet. Body uses the snippet syntax of
// the editor ($1, ${1:default}, $0; a literal $ is written \$). Imports are
// added to the file when the snippet is inserted, e.g. "time" or
// `ha gohomeassistant.local/sdk`.
type Snippet struct {
	Name        string   `json:"name"`
	Prefix      string   `json:"prefix,omitempty"`
	Description string   `json:"description,omitempty"`
	Body        string   `json:"body"`
	Imports     []string `json:"imports,omitempty"`
}

const maxSnippetsSize = 1 << 20

// getSnippets returns the user's snippets; built-in ones live in the UI.
func (s *Server) getSnippets(w http.ResponseWriter, r *http.Request) {
	s.snippetsMu.Lock()
	defer s.snippetsMu.Unlock()
	b, err := os.ReadFile(s.SnippetsFile)
	if os.IsNotExist(err) || (err == nil && len(b) == 0) {
		writeJSON(w, []Snippet{})
		return
	}
	if err != nil {
		httpError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

// putSnippets replaces the whole list.
func (s *Server) putSnippets(w http.ResponseWriter, r *http.Request) {
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxSnippetsSize))
	if err != nil {
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		return
	}
	var list []Snippet
	if err := json.Unmarshal(b, &list); err != nil {
		http.Error(w, "invalid snippets: "+err.Error(), http.StatusBadRequest)
		return
	}
	for i, sn := range list {
		if sn.Name == "" || sn.Body == "" {
			http.Error(w, fmt.Sprintf("snippet %d: name and body are required", i+1), http.StatusBadRequest)
			return
		}
	}
	if list == nil {
		list = []Snippet{}
	}
	out, _ := json.MarshalIndent(list, "", "  ")

	s.snippetsMu.Lock()
	defer s.snippetsMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.SnippetsFile), 0o755); err != nil {
		httpError(w, err)
		return
	}
	tmp := s.SnippetsFile + ".tmp"
	if err := os.WriteFile(tmp, append(out, '\n'), 0o644); err != nil {
		httpError(w, err)
		return
	}
	if err := os.Rename(tmp, s.SnippetsFile); err != nil {
		httpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
