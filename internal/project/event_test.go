package project_test

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gusimo/gohomeassistant/internal/bridge"
	"github.com/gusimo/gohomeassistant/internal/ha"
	"github.com/gusimo/gohomeassistant/internal/hatest"
	"github.com/gusimo/gohomeassistant/internal/project"
)

// A project reacts to a custom event fired on the bus (like an HA
// automation's "event: goevent" action) and can read its data.
const eventProgram = `package main

import (
	"encoding/json"

	ha "gohomeassistant.local/sdk"
)

func main() {
	c := ha.MustConnect()
	err := c.OnEvent("goevent", func(ev ha.Event) {
		var data struct {
			MyData []string ` + "`json:\"mydata\"`" + `
		}
		json.Unmarshal(ev.Data, &data)
		c.CallService("light", "toggle", ha.Target{EntityID: []string{"light." + data.MyData[0]}}, nil)
	})
	if err != nil {
		panic(err)
	}
	c.Run()
}
`

func TestCustomEvent(t *testing.T) {
	fake := hatest.New()
	defer fake.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	tmp, _ := os.MkdirTemp("", "gha")
	defer os.RemoveAll(tmp)
	tmp, _ = filepath.EvalSymlinks(tmp)
	proj := filepath.Join(tmp, "projects", "events")
	must(t, os.MkdirAll(proj, 0o755))
	must(t, os.WriteFile(filepath.Join(proj, "go.mod"), []byte("module events\n\ngo 1.24\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(proj, "main.go"), []byte(eventProgram), 0o644))

	socket := filepath.Join(tmp, "s.sock")
	client := ha.New(fake.URL(), hatest.Token, log)
	go client.Run(ctx)
	go bridge.New(client, log).Serve(ctx, socket)
	sdk, _ := filepath.Abs("../../sdk")
	mgr, err := project.NewManager(project.Config{Root: filepath.Join(tmp, "projects"), BuildDir: filepath.Join(tmp, "build"), Modules: []string{sdk}, Socket: socket, Log: log})
	must(t, err)
	go mgr.Run(ctx)

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		fake.Fire("goevent", map[string]any{"mydata": []string{"test"}})
		select {
		case call := <-fake.Calls:
			if ids, _ := call.Target["entity_id"].([]any); call.Service == "toggle" && len(ids) == 1 && ids[0] == "light.test" {
				return
			}
			t.Fatalf("unexpected call %+v", call)
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Fatal("project never reacted to goevent")
}
