package server

import (
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

func TestSnippets(t *testing.T) {
	root := t.TempDir()
	log := slog.New(slog.DiscardHandler)
	mgr, err := project.NewManager(project.Config{Root: filepath.Join(root, "projects"), BuildDir: filepath.Join(root, "build"), Log: log})
	if err != nil {
		t.Fatal(err)
	}
	s := New(mgr, logbus.New(10), "1.24", log)
	s.SnippetsFile = filepath.Join(root, "data", "snippets.json")
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	if code, body := do(t, "GET", srv.URL+"/api/snippets", ""); code != 200 || strings.TrimSpace(body) != "[]" {
		t.Fatalf("empty list: %d %q", code, body)
	}
	if code, _ := do(t, "PUT", srv.URL+"/api/snippets", `[{"name":"x"}]`); code != http.StatusBadRequest {
		t.Fatalf("snippet without body accepted: %d", code)
	}
	if code, _ := do(t, "PUT", srv.URL+"/api/snippets", `{"name":"x"}`); code != http.StatusBadRequest {
		t.Fatalf("non-list accepted: %d", code)
	}
	list := `[{"name":"Wait","prefix":"wait","body":"time.Sleep(${1:time.Second})","imports":["time"]}]`
	if code, body := do(t, "PUT", srv.URL+"/api/snippets", list); code != http.StatusNoContent {
		t.Fatalf("put: %d %s", code, body)
	}
	code, body := do(t, "GET", srv.URL+"/api/snippets", "")
	if code != 200 || !strings.Contains(body, `"time.Sleep(${1:time.Second})"`) || !strings.Contains(body, `"imports": [`) {
		t.Fatalf("get after put: %d %s", code, body)
	}
	if _, err := os.Stat(s.SnippetsFile + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temp file left behind")
	}
}
