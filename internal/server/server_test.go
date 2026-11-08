package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gusimo/gohomeassistant/internal/logbus"
	"github.com/gusimo/gohomeassistant/internal/project"
)

func setup(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	root := t.TempDir()
	projects := filepath.Join(root, "projects")
	demo := filepath.Join(projects, "demo")
	os.MkdirAll(filepath.Join(demo, ".git"), 0o755)
	os.WriteFile(filepath.Join(demo, "go.mod"), []byte("module demo\n"), 0o644)
	os.WriteFile(filepath.Join(demo, "main.go"), []byte("package main\n"), 0o644)
	os.WriteFile(filepath.Join(demo, ".git", "config"), []byte("secret"), 0o644)
	os.WriteFile(filepath.Join(root, "outside.txt"), []byte("outside"), 0o644)

	log := slog.New(slog.DiscardHandler)
	mgr, err := project.NewManager(project.Config{Root: projects, BuildDir: filepath.Join(root, "build"), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(mgr, logbus.New(10), "1.24", log).Handler())
	t.Cleanup(srv.Close)
	return srv, demo
}

func do(t *testing.T, method, url, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestFileAccessStaysInsideProject(t *testing.T) {
	srv, _ := setup(t)
	for _, p := range []string{
		"/api/projects/demo/files/../outside.txt",
		"/api/projects/demo/files/%2e%2e/outside.txt",
		"/api/projects/demo/files/..%2foutside.txt",
		"/api/projects/demo/files/.git/config",
		"/api/projects/demo/files/sub/../../outside.txt",
		"/api/projects/demo/files//etc/passwd",
		"/api/projects/..%2f/files/outside.txt",
	} {
		code, body := do(t, "GET", srv.URL+p, "")
		if code == http.StatusOK || strings.Contains(body, "outside") || strings.Contains(body, "secret") {
			t.Errorf("%s: got %d %q", p, code, body)
		}
	}
}

func TestFileRoundTrip(t *testing.T) {
	srv, demo := setup(t)
	if code, _ := do(t, "PUT", srv.URL+"/api/projects/demo/files/lib/util.go", "package main\n// x\n"); code != http.StatusNoContent {
		t.Fatalf("put: %d", code)
	}
	if b, _ := os.ReadFile(filepath.Join(demo, "lib", "util.go")); string(b) != "package main\n// x\n" {
		t.Fatalf("content %q", b)
	}
	code, body := do(t, "GET", srv.URL+"/api/projects/demo/files", "")
	if code != 200 || !strings.Contains(body, `"lib/util.go"`) || strings.Contains(body, ".git") || strings.Contains(body, ".gha-tmp") {
		t.Fatalf("list: %d %s", code, body)
	}
	if code, _ := do(t, "DELETE", srv.URL+"/api/projects/demo/files/go.mod", ""); code != http.StatusBadRequest {
		t.Errorf("deleting go.mod should be refused, got %d", code)
	}
	if code, _ := do(t, "DELETE", srv.URL+"/api/projects/demo/files/lib/util.go", ""); code != http.StatusNoContent {
		t.Errorf("delete: %d", code)
	}
}

func TestCreateProject(t *testing.T) {
	srv, demo := setup(t)
	root := filepath.Dir(demo)
	if code, _ := do(t, "POST", srv.URL+"/api/projects", `{"name":"Bad Name"}`); code != http.StatusBadRequest {
		t.Errorf("bad name accepted: %d", code)
	}
	if code, _ := do(t, "POST", srv.URL+"/api/projects", `{"name":"demo"}`); code != http.StatusConflict {
		t.Errorf("duplicate accepted: %d", code)
	}
	if code, body := do(t, "POST", srv.URL+"/api/projects", `{"name":"garden"}`); code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "garden", "go.mod")); !strings.Contains(string(b), "module garden") {
		t.Errorf("go.mod: %q", b)
	}
}
