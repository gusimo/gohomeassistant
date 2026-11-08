package project_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gusimo/gohomeassistant/internal/bridge"
	"github.com/gusimo/gohomeassistant/internal/ha"
	"github.com/gusimo/gohomeassistant/internal/hatest"
	"github.com/gusimo/gohomeassistant/internal/project"
)

const debugProgram = `package main

import ha "gohomeassistant.local/sdk"

func main() {
	c := ha.MustConnect()
	c.OnStateChange("input_boolean.trigger", func(ch ha.StateChange) {
		target := "light.debug"
		c.CallService("light", "turn_on", ha.Target{EntityID: []string{target}}, nil)
	})
	c.Run()
}
`

const breakpointLine = 9 // c.CallService(...)

func TestDebugger(t *testing.T) {
	if _, err := exec.LookPath("dlv"); err != nil {
		t.Skip("dlv not installed")
	}
	fake := hatest.New()
	defer fake.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))

	tmp, _ := os.MkdirTemp("", "gha")
	defer os.RemoveAll(tmp)
	tmp, _ = filepath.EvalSymlinks(tmp)
	root := filepath.Join(tmp, "projects")
	proj := filepath.Join(root, "demo")
	must(t, os.MkdirAll(proj, 0o755))
	must(t, os.WriteFile(filepath.Join(proj, "go.mod"), []byte("module demo\n\ngo 1.24\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(proj, "main.go"), []byte(debugProgram), 0o644))

	socket := filepath.Join(tmp, "s.sock")
	client := ha.New(fake.URL(), hatest.Token, log)
	go client.Run(ctx)
	go bridge.New(client, log).Serve(ctx, socket)
	sdk, _ := filepath.Abs("../../sdk")
	mgr, err := project.NewManager(project.Config{Root: root, BuildDir: filepath.Join(tmp, "build"), Modules: []string{sdk}, Socket: socket, Log: log})
	must(t, err)
	done := make(chan struct{})
	go func() { mgr.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	waitForLight(t, fake, "light.debug") // running normally
	must(t, mgr.SetDebug("demo", true))
	port := waitFor(t, func() int {
		st := mgr.Statuses()
		if len(st) == 1 && st[0].Running && st[0].DebugPort != 0 {
			return st[0].DebugPort
		}
		return 0
	})
	waitForLight(t, fake, "light.debug") // runs on under Delve (--continue)

	d := dialDAP(t, port)
	d.request("initialize", map[string]any{"adapterID": "go", "linesStartAt1": true, "columnsStartAt1": true, "pathFormat": "path"})
	d.request("attach", map[string]any{"mode": "remote"})
	d.waitEvent("initialized")
	res := d.request("setBreakpoints", map[string]any{
		"source":      map[string]any{"path": filepath.Join(proj, "main.go")},
		"breakpoints": []map[string]any{{"line": breakpointLine}},
	})
	var bps struct {
		Breakpoints []struct{ Verified bool } `json:"breakpoints"`
	}
	json.Unmarshal(res, &bps)
	if len(bps.Breakpoints) != 1 || !bps.Breakpoints[0].Verified {
		t.Fatalf("breakpoint not verified: %s", res)
	}
	d.request("configurationDone", nil)

	fake.SetState("input_boolean.trigger", "hit")
	stopped := d.waitEvent("stopped")
	var ev struct {
		Reason   string `json:"reason"`
		ThreadID int    `json:"threadId"`
	}
	json.Unmarshal(stopped, &ev)
	if ev.Reason != "breakpoint" {
		t.Fatalf("stopped for %q: %s", ev.Reason, stopped)
	}

	var stack struct {
		StackFrames []struct {
			ID     int `json:"id"`
			Line   int `json:"line"`
			Source struct {
				Path string `json:"path"`
			} `json:"source"`
		} `json:"stackFrames"`
	}
	json.Unmarshal(d.request("stackTrace", map[string]any{"threadId": ev.ThreadID, "levels": 5}), &stack)
	top := stack.StackFrames[0]
	if top.Line != breakpointLine || top.Source.Path != filepath.Join(proj, "main.go") {
		t.Fatalf("stopped at %s:%d", top.Source.Path, top.Line)
	}
	var scopes struct {
		Scopes []struct {
			Name string `json:"name"`
			Ref  int    `json:"variablesReference"`
		} `json:"scopes"`
	}
	json.Unmarshal(d.request("scopes", map[string]any{"frameId": top.ID}), &scopes)
	var vars struct {
		Variables []struct{ Name, Value string } `json:"variables"`
	}
	json.Unmarshal(d.request("variables", map[string]any{"variablesReference": scopes.Scopes[0].Ref}), &vars)
	found := false
	for _, v := range vars.Variables {
		found = found || (v.Name == "target" && v.Value == `"light.debug"`)
	}
	if !found {
		t.Fatalf("local variable target not found: %+v", vars.Variables)
	}

	select {
	case call := <-fake.Calls:
		t.Fatalf("service called while paused: %+v", call)
	default:
	}
	d.request("continue", map[string]any{"threadId": ev.ThreadID})
	waitForLight(t, fake, "light.debug") // the paused call goes through

	// Detaching leaves the program running.
	d.request("disconnect", map[string]any{"terminateDebuggee": false})
	d.conn.Close()
	time.Sleep(500 * time.Millisecond)
	d2 := dialDAP(t, port) // and it can be attached again
	d2.request("initialize", map[string]any{"adapterID": "go", "pathFormat": "path"})
	d2.request("attach", map[string]any{"mode": "remote"})
	d2.waitEvent("initialized")
	d2.request("setBreakpoints", map[string]any{"source": map[string]any{"path": filepath.Join(proj, "main.go")}, "breakpoints": []any{}})
	d2.request("configurationDone", nil)
	d2.request("disconnect", map[string]any{"terminateDebuggee": false})
	d2.conn.Close()
	drain(fake)
	waitForLight(t, fake, "light.debug")

	// Debug mode off while a debugger is attached (like the UI): back to a
	// normal process, and the debugged one must be gone.
	d3 := dialDAP(t, port)
	d3.request("initialize", map[string]any{"adapterID": "go", "pathFormat": "path"})
	d3.request("attach", map[string]any{"mode": "remote"})
	d3.waitEvent("initialized")
	d3.request("configurationDone", nil)
	defer d3.conn.Close()
	must(t, mgr.SetDebug("demo", false))
	waitFor(t, func() int {
		if st := mgr.Statuses(); len(st) == 1 && st[0].Running && !st[0].Debug && st[0].DebugPort == 0 {
			return 1
		}
		return 0
	})
	waitForLight(t, fake, "light.debug")

	// Stopping leaves no project process behind (Delve runs the program in
	// its own process group).
	cancel()
	<-done
	done = make(chan struct{})
	close(done)
	if left := processesUnder(filepath.Join(tmp, "build")); len(left) > 0 {
		t.Fatalf("processes left behind: %v", left)
	}
}

// processesUnder lists running processes whose executable is below dir.
func processesUnder(dir string) []string {
	entries, _ := os.ReadDir("/proc")
	var out []string
	for _, e := range entries {
		exe, err := os.Readlink("/proc/" + e.Name() + "/exe")
		if err == nil && strings.HasPrefix(exe, dir) {
			out = append(out, e.Name()+" "+exe)
		}
	}
	return out
}

func waitFor(t *testing.T, f func() int) int {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if v := f(); v != 0 {
			return v
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("timeout")
	return 0
}

func drain(f *hatest.Fake) {
	for {
		select {
		case <-f.Calls:
		default:
			return
		}
	}
}

// dap is a minimal DAP client for the test.
type dap struct {
	t    *testing.T
	conn net.Conn
	rd   *bufio.Reader
	seq  int
	// events seen while waiting for responses
	events []map[string]json.RawMessage
}

func dialDAP(t *testing.T, port int) *dap {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(60 * time.Second))
	return &dap{t: t, conn: conn, rd: bufio.NewReader(conn)}
}

func (d *dap) read() map[string]json.RawMessage {
	d.t.Helper()
	hdr, err := textproto.NewReader(d.rd).ReadMIMEHeader()
	if err != nil {
		d.t.Fatal(err)
	}
	n, _ := strconv.Atoi(hdr.Get("Content-Length"))
	b := make([]byte, n)
	if _, err := io.ReadFull(d.rd, b); err != nil {
		d.t.Fatal(err)
	}
	var m map[string]json.RawMessage
	json.Unmarshal(b, &m)
	return m
}

func (d *dap) request(cmd string, args any) json.RawMessage {
	d.t.Helper()
	d.seq++
	b, _ := json.Marshal(map[string]any{"seq": d.seq, "type": "request", "command": cmd, "arguments": args})
	fmt.Fprintf(d.conn, "Content-Length: %d\r\n\r\n%s", len(b), b)
	for {
		m := d.read()
		if string(m["type"]) == `"event"` {
			d.events = append(d.events, m)
			continue
		}
		if string(m["request_seq"]) == strconv.Itoa(d.seq) {
			if string(m["success"]) != "true" {
				d.t.Fatalf("%s failed: %s", cmd, m["message"])
			}
			return m["body"]
		}
	}
}

func (d *dap) waitEvent(name string) json.RawMessage {
	d.t.Helper()
	for i, m := range d.events {
		if string(m["event"]) == `"`+name+`"` {
			d.events = append(d.events[:i], d.events[i+1:]...)
			return m["body"]
		}
	}
	for {
		m := d.read()
		if string(m["type"]) == `"event"` && string(m["event"]) == `"`+name+`"` {
			return m["body"]
		}
	}
}
