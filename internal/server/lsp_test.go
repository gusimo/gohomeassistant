package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/gusimo/gohomeassistant/internal/logbus"
	"github.com/gusimo/gohomeassistant/internal/project"
)

func TestLSPCompletionThroughWorkspace(t *testing.T) {
	if _, err := exec.LookPath("gopls"); err != nil {
		t.Skip("gopls not installed")
	}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	dir := filepath.Join(root, "projects", "demo")
	os.MkdirAll(dir, 0o755)
	src := "package main\n\nimport ha \"github.com/gusimo/gohomeassistant/sdk\"\n\nfunc main() {\n\tha.Mu\n\tundefinedThing()\n}\n"
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module demo\n\ngo 1.24\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644)
	sdk, _ := filepath.Abs("../../sdk")
	log := slog.New(slog.DiscardHandler)
	mgr, err := project.NewManager(project.Config{Root: filepath.Join(root, "projects"), BuildDir: filepath.Join(root, "build"), Modules: []string{sdk}, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(New(mgr, logbus.New(10), "1.24", log).Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/api/projects/demo/lsp", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(16 << 20)

	send := func(v map[string]any) {
		v["jsonrpc"] = "2.0"
		b, _ := json.Marshal(v)
		if err := conn.Write(ctx, websocket.MessageText, b); err != nil {
			t.Fatal(err)
		}
	}
	// next returns the response to id; server requests are answered with null.
	var diagnostics []string
	next := func(id int) json.RawMessage {
		for {
			_, b, err := conn.Read(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var m struct {
				ID     *json.RawMessage `json:"id"`
				Method string           `json:"method"`
				Result json.RawMessage  `json:"result"`
				Params json.RawMessage  `json:"params"`
			}
			json.Unmarshal(b, &m)
			switch {
			case m.Method == "textDocument/publishDiagnostics":
				diagnostics = append(diagnostics, string(m.Params))
			case m.Method != "" && m.ID != nil:
				send(map[string]any{"id": m.ID, "result": nil})
			case m.ID != nil && string(*m.ID) == strings.TrimSpace(string(mustJSON(id))):
				return m.Result
			}
		}
	}

	uri := "file://" + filepath.Join(dir, "main.go")
	send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{
		"processId": nil, "rootUri": "file://" + dir, "capabilities": map[string]any{},
	}})
	next(1)
	send(map[string]any{"method": "initialized", "params": map[string]any{}})
	send(map[string]any{"method": "textDocument/didOpen", "params": map[string]any{
		"textDocument": map[string]any{"uri": uri, "languageId": "go", "version": 1, "text": src},
	}})
	send(map[string]any{"id": 2, "method": "textDocument/completion", "params": map[string]any{
		"textDocument": map[string]any{"uri": uri}, "position": map[string]any{"line": 5, "character": 6},
	}})
	if res := next(2); !strings.Contains(string(res), `"MustConnect"`) {
		t.Fatalf("completion lacks MustConnect: %.500s", res)
	}

	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(strings.Join(diagnostics, ""), "undefinedThing") {
		if time.Now().After(deadline) {
			t.Fatalf("no diagnostic for undefinedThing: %v", diagnostics)
		}
		send(map[string]any{"id": 3, "method": "workspace/symbol", "params": map[string]any{"query": "x"}}) // keep reading
		next(3)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
