package ha

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// trace describes one handler run. It is sent to the host when the handler
// returns.
type trace struct {
	Trigger string    `json:"trigger"`        // "state", "event", "go" (Client.Go) or "call" (a call outside a handler)
	Name    string    `json:"name,omitempty"` // entity id or event type
	From    *string   `json:"from,omitempty"` // state before and after; nil if the entity didn't exist
	To      *string   `json:"to,omitempty"`
	Src     string    `json:"src,omitempty"` // where the handler was registered, "main.go:12"
	Start   time.Time `json:"start"`
	DurUS   int64     `json:"dur_us"`
	Steps   []step    `json:"steps,omitempty"`
	Failed  bool      `json:"failed,omitempty"` // a panic or a failed call

	always bool // report it even if nothing happened in it
}

type step struct {
	AtUS  int64  `json:"at_us"` // since the start of the trace
	Kind  string `json:"kind"`  // "call", "log" or "panic"
	Msg   string `json:"msg"`   // "light.turn_on", the log line, the panic value
	Data  any    `json:"data,omitempty"`
	Src   string `json:"src,omitempty"` // line of project code that made the call
	DurUS int64  `json:"dur_us,omitempty"`
	Err   string `json:"err,omitempty"`
}

func (t *trace) add(s step) *step {
	s.AtUS = time.Since(t.Start).Microseconds()
	t.Steps = append(t.Steps, s)
	return &t.Steps[len(t.Steps)-1]
}

// invoke runs a handler on the dispatch goroutine and reports its trace. A
// panic is logged and ends only this run.
func (c *Client) invoke(t *trace, fn func()) {
	t.Start = time.Now()
	c.cur = t
	defer func() {
		r := recover()
		c.cur = nil
		if r != nil {
			stack := panicStack(debug.Stack())
			t.add(step{Kind: "panic", Msg: fmt.Sprint(r), Data: stack})
			t.Failed = true
			c.hostLog("ERROR", fmt.Sprintf("handler panic: %v\n%s", r, strings.TrimSpace(stack)))
		}
		t.DurUS = time.Since(t.Start).Microseconds()
		if t.always || len(t.Steps) > 0 {
			c.report(t)
		}
	}()
	fn()
}

// panicStack drops the frames above the panicking one (this file's recover
// and the runtime's panic) and below the handler, so the stack starts at the
// line that failed and ends in project code.
func panicStack(stack []byte) string {
	s := string(stack)
	if i := strings.Index(s, "\npanic("); i >= 0 {
		if lines := strings.SplitN(s[i+1:], "\n", 3); len(lines) == 3 {
			s = lines[2]
		}
	}
	// Below the handler there is only the SDK's dispatching.
	if i := strings.LastIndex(s, "\ngohomeassistant.local/sdk.(*Client).invoke("); i >= 0 {
		s = s[:i+1]
	}
	return s
}

// hostLog writes a line to the host's log with a level; what a project
// prints itself is always logged as INFO. Failures the project code didn't
// handle are reported this way, so they show up when filtering for problems.
func (c *Client) hostLog(level, msg string) {
	c.encMu.Lock()
	defer c.encMu.Unlock()
	_ = c.enc.Encode(map[string]any{"type": "gha/log", "project": os.Getenv("GHA_PROJECT"), "level": level, "msg": msg})
}

func (c *Client) report(t *trace) {
	c.encMu.Lock()
	defer c.encMu.Unlock()
	_ = c.enc.Encode(map[string]any{ // no id: the host doesn't answer
		"type": "gha/trace", "project": os.Getenv("GHA_PROJECT"), "trace": t,
		"failed": t.Failed, "noteworthy": len(t.Steps) > 0,
	})
}

// current returns the trace of the running handler if the calling goroutine
// is the one running it, else nil.
func (c *Client) current() *trace {
	if goid() != c.gid.Load() {
		return nil
	}
	return c.cur
}

// traceCall records msg as a step of the running handler. Service calls and
// events fired outside of handlers (e.g. in main or a timer goroutine) are
// reported as a trace of their own. The returned function finishes the step.
func (c *Client) traceCall(msg map[string]any) func(error) {
	typ, _ := msg["type"].(string)
	s := step{Kind: "call", Msg: typ, Src: callSite()}
	switch typ {
	case "call_service":
		s.Msg = fmt.Sprintf("%v.%v", msg["domain"], msg["service"])
		s.Data = map[string]any{"target": msg["target"], "data": msg["service_data"]}
	case "fire_event":
		s.Msg = fmt.Sprintf("fire event %v", msg["event_type"])
		s.Data = msg["event_data"]
	}
	t := c.current()
	alone := t == nil
	if alone {
		if typ != "call_service" && typ != "fire_event" {
			return func(error) {}
		}
		t = &trace{Trigger: "call", Name: s.Msg, Src: s.Src, Start: time.Now()}
	}
	start := time.Now()
	st := t.add(s)
	return func(err error) {
		st.DurUS = time.Since(start).Microseconds()
		if err != nil {
			st.Err = err.Error()
			t.Failed = true
			c.hostLog("WARN", fmt.Sprintf("%s failed: %v (%s)", s.Msg, err, s.Src))
		}
		if alone {
			t.DurUS = st.DurUS
			c.report(t)
		}
	}
}

// logTee is the output of package log: stderr (which the host collects),
// plus the trace of the running handler.
type logTee struct{ c *Client }

func (w logTee) Write(b []byte) (int, error) {
	if t := w.c.current(); t != nil {
		t.add(step{Kind: "log", Msg: strings.TrimRight(string(b), "\n")})
	}
	return os.Stderr.Write(b)
}

// goid returns the id of the calling goroutine. Go hides it on purpose; it
// is only used to tell whether a call or log line belongs to the running
// handler.
func goid() uint64 {
	var buf [64]byte
	s := strings.TrimPrefix(string(buf[:runtime.Stack(buf[:], false)]), "goroutine ")
	if i := strings.IndexByte(s, ' '); i > 0 {
		id, _ := strconv.ParseUint(s[:i], 10, 64)
		return id
	}
	return 0
}

// caller returns "file:line" of the caller of the function that calls it
// (skip = 1) or of a function further up.
func caller(skip int) string {
	_, file, line, ok := runtime.Caller(skip + 1)
	if !ok {
		return ""
	}
	return shortFile(file) + ":" + strconv.Itoa(line)
}

// callSite returns the first caller outside of this package and the
// generated entities: the line of project code that made a call.
func callSite() string {
	pc := make([]uintptr, 16)
	frames := runtime.CallersFrames(pc[:runtime.Callers(2, pc)])
	for {
		f, more := frames.Next()
		if !strings.Contains(f.Function, "gohomeassistant.local/") {
			return shortFile(f.File) + ":" + strconv.Itoa(f.Line)
		}
		if !more {
			return ""
		}
	}
}

// shortFile makes a source path relative to the project directory, which is
// the working directory of the process.
func shortFile(file string) string {
	if wd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(wd, file); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.Base(file)
}
