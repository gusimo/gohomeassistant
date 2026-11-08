package project_test

import (
	"context"
	"fmt"
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

const program = `package main

import ha "github.com/gusimo/gohomeassistant/sdk"

func main() {
	c := ha.MustConnect()
	c.OnStateChange("input_boolean.trigger", func(ch ha.StateChange) {
		c.CallService("light", "turn_on", ha.Target{EntityID: []string{%q}}, nil)
	})
	c.Run()
}
`

func TestHotReload(t *testing.T) {
	fake := hatest.New()
	defer fake.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	tmp, err := os.MkdirTemp("", "gha") // short path: unix socket length limit
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmp)
	socket := filepath.Join(tmp, "s.sock")
	root := filepath.Join(tmp, "projects")
	proj := filepath.Join(root, "demo")
	must(t, os.MkdirAll(proj, 0o755))
	must(t, os.WriteFile(filepath.Join(proj, "go.mod"), []byte("module demo\n\ngo 1.24\n"), 0o644))
	writeMain(t, proj, "light.v1")

	client := ha.New(fake.URL(), hatest.Token, log)
	go client.Run(ctx)
	go bridge.New(client, log).Serve(ctx, socket)
	sdk, _ := filepath.Abs("../../sdk")
	mgr, err := project.NewManager(project.Config{Root: root, BuildDir: filepath.Join(tmp, "build"), Modules: []string{sdk}, Socket: socket, Log: log})
	must(t, err)
	done := make(chan struct{})
	go func() { mgr.Run(ctx); close(done) }()

	t.Log("initial build")
	waitForLight(t, fake, "light.v1")

	t.Log("hot reload")
	writeMain(t, proj, "light.v2")
	waitForLight(t, fake, "light.v2")

	t.Log("broken build keeps v2 running")
	must(t, os.WriteFile(filepath.Join(proj, "main.go"), []byte("package main\nfunc main() { syntax error }\n"), 0o644))
	time.Sleep(3 * time.Second) // let the failed build happen
	waitForLight(t, fake, "light.v2")

	t.Log("removing the project stops it and drops its subscriptions")
	must(t, os.RemoveAll(proj))
	deadline := time.Now().Add(10 * time.Second)
	for fake.Subscriptions() != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("still %d subscriptions", fake.Subscriptions())
		}
		time.Sleep(50 * time.Millisecond)
	}

	cancel()
	<-done
}

// waitForLight toggles the trigger until the project calls light.turn_on for
// want (the project may still be building or restarting).
func waitForLight(t *testing.T, fake *hatest.Fake, want string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		fake.SetState("input_boolean.trigger", fmt.Sprint(i))
		select {
		case call := <-fake.Calls:
			if ids, _ := call.Target["entity_id"].([]any); len(ids) == 1 && ids[0] == want {
				return
			}
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Fatalf("project never turned on %s", want)
}

func writeMain(t *testing.T, dir, light string) {
	t.Helper()
	must(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(fmt.Sprintf(program, light)), 0o644))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
