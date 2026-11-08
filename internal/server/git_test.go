package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitWorkflow(t *testing.T) {
	srv, demo := setup(t)
	root := filepath.Dir(demo)
	api := srv.URL + "/api/projects/garden"

	// New projects start as a repository with an initial commit.
	if code, body := do(t, "POST", srv.URL+"/api/projects", `{"name":"garden"}`); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var log []gitCommit
	_, body := do(t, "GET", api+"/git/log", "")
	json.Unmarshal([]byte(body), &log)
	if len(log) != 1 || log[0].Message != "Create project" {
		t.Fatalf("initial log: %s", body)
	}
	first := log[0].Hash

	// Change one file, add one: both show up.
	do(t, "PUT", api+"/files/main.go", "package main\n\nfunc main() {}\n")
	do(t, "PUT", api+"/files/lights.go", "package main\n")
	_, body = do(t, "GET", api+"/git/status", "")
	if !strings.Contains(body, `{"path":"main.go","status":"modified"}`) || !strings.Contains(body, `{"path":"lights.go","status":"untracked"}`) {
		t.Fatalf("status: %s", body)
	}
	// Original content for the diff view.
	if _, orig := do(t, "GET", api+"/git/show?rev=HEAD&path=main.go", ""); !strings.Contains(orig, "ha.MustConnect()") {
		t.Fatalf("show HEAD: %q", orig)
	}

	if code, body := do(t, "POST", api+"/git/commit", `{"message":"Add lights"}`); code != http.StatusNoContent {
		t.Fatalf("commit: %d %s", code, body)
	}
	_, body = do(t, "GET", api+"/git/status", "")
	if !strings.Contains(body, `"files":[]`) {
		t.Fatalf("clean after commit: %s", body)
	}
	_, body = do(t, "GET", api+"/git/log", "")
	json.Unmarshal([]byte(body), &log)
	if len(log) != 2 || log[0].Message != "Add lights" {
		t.Fatalf("log: %s", body)
	}
	_, body = do(t, "GET", api+"/git/commits/"+log[0].Hash, "")
	if !strings.Contains(body, `{"path":"lights.go","status":"added"}`) || !strings.Contains(body, `{"path":"main.go","status":"modified"}`) {
		t.Fatalf("commit files: %s", body)
	}

	// Discard: a modified file goes back to HEAD, a new file is deleted.
	do(t, "PUT", api+"/files/main.go", "broken")
	do(t, "PUT", api+"/files/scratch.go", "package main\n")
	do(t, "POST", api+"/git/discard", `{"path":"main.go"}`)
	do(t, "POST", api+"/git/discard", `{"path":"scratch.go"}`)
	if b, _ := os.ReadFile(filepath.Join(root, "garden", "main.go")); string(b) != "package main\n\nfunc main() {}\n" {
		t.Errorf("discard modified: %q", b)
	}
	if _, err := os.Stat(filepath.Join(root, "garden", "scratch.go")); !os.IsNotExist(err) {
		t.Errorf("discard new file: %v", err)
	}

	// Restore the first version of main.go as an uncommitted change.
	if code, body := do(t, "POST", api+"/git/restore", `{"path":"main.go","rev":"`+first+`"}`); code != http.StatusNoContent {
		t.Fatalf("restore: %d %s", code, body)
	}
	_, body = do(t, "GET", api+"/git/status", "")
	if !strings.Contains(body, `{"path":"main.go","status":"modified"}`) {
		t.Fatalf("restore should be an unstaged change: %s", body)
	}

	// Paths and revisions are validated.
	for _, req := range [][2]string{
		{"GET", api + "/git/show?rev=HEAD&path=../demo/main.go"},
		{"GET", api + "/git/show?rev=HEAD&path=.git/config"},
		{"GET", api + "/git/show?rev=--output=/tmp/x&path=main.go"},
		{"GET", api + "/git/commits/--all"},
	} {
		if code, _ := do(t, req[0], req[1], ""); code != http.StatusBadRequest {
			t.Errorf("%s: got %d", req[1], code)
		}
	}
	if code, _ := do(t, "POST", api+"/git/discard", `{"path":"-f"}`); code != http.StatusBadRequest {
		t.Errorf("option-like path accepted")
	}

	// Existing projects without git can be initialized.
	_, body = do(t, "GET", srv.URL+"/api/projects/demo/git/status", "")
	if body != "{\"repo\":false}\n" {
		t.Fatalf("demo status: %q", body)
	}
	if code, body := do(t, "POST", srv.URL+"/api/projects/demo/git/init", ""); code != http.StatusNoContent {
		t.Fatalf("init: %d %s", code, body)
	}
	_, body = do(t, "GET", srv.URL+"/api/projects/demo/git/log", "")
	if !strings.Contains(body, "Create project") {
		t.Fatalf("demo log: %s", body)
	}
}
