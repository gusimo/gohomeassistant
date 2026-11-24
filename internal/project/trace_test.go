package project_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gusimo/gohomeassistant/internal/bridge"
	"github.com/gusimo/gohomeassistant/internal/ha"
	"github.com/gusimo/gohomeassistant/internal/hatest"
	"github.com/gusimo/gohomeassistant/internal/project"
)

const traceProgram = `package main

import (
	"log"

	ha "gohomeassistant.local/sdk"
)

func main() {
	c := ha.MustConnect()
	c.OnStateChange("input_boolean.trigger", func(ch ha.StateChange) {
		log.Println("triggered", ch.New.State)
		c.CallService("light", "turn_on", ha.Target{EntityID: []string{"light.a"}}, map[string]any{"brightness_pct": 50})
		if ch.New.State == "boom" {
			panic("boom")
		}
	})
	c.Run()
}
`

type traceMsg struct {
	Trigger, Name, Src string
	To                 *string
	Failed             bool
	Steps              []struct{ Kind, Msg, Src, Err string }
}

// Every handler run is reported as a trace with its log lines and calls; a
// panic fails only that run; and after Home Assistant was away, the project
// learns about state changes it missed.
func TestTracesAndResync(t *testing.T) {
	fake := hatest.New()
	defer fake.Close()
	go func() {
		for range fake.Calls {
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	tmp, _ := os.MkdirTemp("", "gha")
	defer os.RemoveAll(tmp)
	tmp, _ = filepath.EvalSymlinks(tmp)
	proj := filepath.Join(tmp, "projects", "traced")
	must(t, os.MkdirAll(proj, 0o755))
	must(t, os.WriteFile(filepath.Join(proj, "go.mod"), []byte("module traced\n\ngo 1.24\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(proj, "main.go"), []byte(traceProgram), 0o644))

	traces := make(chan traceMsg, 1000)
	socket := filepath.Join(tmp, "s.sock")
	client := ha.New(fake.URL(), hatest.Token, log)
	br := bridge.New(client, log)
	br.OnTrace = func(project string, start time.Time, failed, noteworthy bool, raw json.RawMessage) {
		var m traceMsg
		if err := json.Unmarshal(raw, &m); err != nil || project != "traced" || !noteworthy || start.IsZero() {
			t.Errorf("bad trace from %q: %v: %s", project, err, raw)
		}
		traces <- m
	}
	client.OnReconnect = br.Reconnected
	go client.Run(ctx)
	go br.Serve(ctx, socket)
	sdk, _ := filepath.Abs("../../sdk")
	mgr, err := project.NewManager(project.Config{Root: filepath.Join(tmp, "projects"), BuildDir: filepath.Join(tmp, "build"), Modules: []string{sdk}, Socket: socket, Log: log})
	must(t, err)
	go mgr.Run(ctx)

	// waitFor sets the trigger to state (unless empty) until a trace for it arrives.
	waitFor := func(state string, set bool) traceMsg {
		t.Helper()
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			if set {
				fake.SetState("input_boolean.trigger", state)
			}
			timeout := time.After(500 * time.Millisecond)
			for waiting := true; waiting; {
				select {
				case m := <-traces:
					if m.To != nil && *m.To == state {
						return m
					}
				case <-timeout:
					waiting = false
				}
			}
		}
		t.Fatalf("no trace for state %q", state)
		panic("unreachable")
	}

	m := waitFor("on", true)
	if m.Trigger != "state" || m.Name != "input_boolean.trigger" || m.Src != "main.go:11" || m.Failed || len(m.Steps) != 2 {
		t.Fatalf("trace: %+v", m)
	}
	if s := m.Steps[0]; s.Kind != "log" || s.Msg != "triggered on" {
		t.Fatalf("log step: %+v", s)
	}
	if s := m.Steps[1]; s.Kind != "call" || s.Msg != "light.turn_on" || s.Src != "main.go:13" || s.Err != "" {
		t.Fatalf("call step: %+v", s)
	}

	m = waitFor("boom", true)
	if last := m.Steps[len(m.Steps)-1]; !m.Failed || last.Kind != "panic" || last.Msg != "boom" {
		t.Fatalf("panic trace: %+v", m)
	}
	waitFor("alive", true) // the panic didn't stop the project

	// Home Assistant goes away; the state changes while nobody listens.
	fake.DropConnections()
	fake.SetState("input_boolean.trigger", "missed")
	m = waitFor("missed", false)
	if !strings.HasPrefix(m.Src, "main.go:") {
		t.Fatalf("resync trace: %+v", m)
	}
}
