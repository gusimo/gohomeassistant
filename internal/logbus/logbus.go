// Package logbus collects log records, execution traces and project status
// changes and fans them out to UI clients, with a bounded history for late
// joiners. With Open, logs and traces are also kept on disk, so they survive
// restarts and can be read long after they were written.
package logbus

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Entry is one log line or, if Trace is set, one execution trace.
type Entry struct {
	Seq     int64             `json:"seq"`
	Time    time.Time         `json:"time"`
	Level   string            `json:"level"`
	Project string            `json:"project,omitempty"` // "" = host
	Output  bool              `json:"output,omitempty"`  // stdout/stderr of the project
	Msg     string            `json:"msg,omitempty"`
	Attrs   map[string]string `json:"attrs,omitempty"`
	Trace   json.RawMessage   `json:"trace,omitempty"` // as sent by the SDK
}

// Event is sent to subscribers: a log entry, a trace or a status (any JSON).
type Event struct {
	Type    string `json:"type"` // "log", "trace", "status", "files" or "entities"
	Log     *Entry `json:"log,omitempty"`
	Status  any    `json:"status,omitempty"`
	Project string `json:"project,omitempty"` // for "files": files of this project changed on disk
}

// traceHistory is how many traces are kept in memory. They have their own
// ring, so chatty logs don't push them out (and the other way round).
const traceHistory = 500

type Bus struct {
	mu     sync.Mutex
	logs   ring
	traces ring
	seq    int64
	subs   map[chan Event]struct{}
	store  *store // nil: memory only
}

// ring is a bounded buffer of the newest entries.
type ring struct {
	buf  []Entry
	next int // write position once full
	max  int
}

func (r *ring) add(e Entry) {
	if len(r.buf) < r.max {
		r.buf = append(r.buf, e)
		return
	}
	r.buf[r.next] = e
	r.next = (r.next + 1) % r.max
}

// list returns the entries, oldest first.
func (r *ring) list() []Entry {
	if len(r.buf) < r.max {
		return append([]Entry(nil), r.buf...)
	}
	return append(append([]Entry(nil), r.buf[r.next:]...), r.buf[:r.next]...)
}

// New returns a bus that keeps the last history log entries in memory.
func New(history int) *Bus {
	return &Bus{logs: ring{max: history}, traces: ring{max: traceHistory}, subs: map[chan Event]struct{}{}}
}

// History returns the buffered log entries, oldest first.
func (b *Bus) History() []Entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.logs.list()
}

// Snapshot returns the buffered log entries and traces, each oldest first,
// as of the same moment.
func (b *Bus) Snapshot() (logs, traces []Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.logs.list(), b.traces.list()
}

// Traces returns the buffered traces, oldest first.
func (b *Bus) Traces() []Entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.traces.list()
}

// PublishTrace records an execution trace of a project. Traces in which
// nothing happened (noteworthy = false) are only kept in memory.
func (b *Bus) PublishTrace(project string, start time.Time, failed, noteworthy bool, trace json.RawMessage) {
	level := slog.LevelInfo
	if failed {
		level = slog.LevelError
	}
	b.publish(Entry{Time: start, Level: level.String(), Project: project, Trace: trace}, noteworthy)
}

// Subscribe returns a channel of future events. A subscriber that doesn't
// keep up loses events rather than blocking logging.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 1024)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
	}
}

func (b *Bus) PublishStatus(status any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fanout(Event{Type: "status", Status: status})
}

// PublishEntities tells clients that the generated entities changed.
func (b *Bus) PublishEntities() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fanout(Event{Type: "entities"})
}

// PublishFiles tells clients that files of a project changed on disk.
func (b *Bus) PublishFiles(project string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fanout(Event{Type: "files", Project: project})
}

func (b *Bus) publish(e Entry, persist bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	e.Seq = b.seq
	typ := "log"
	if e.Trace != nil {
		typ = "trace"
		b.traces.add(e)
	} else {
		b.logs.add(e)
	}
	if b.store != nil && persist {
		b.store.append(e)
	}
	b.fanout(Event{Type: typ, Log: &e})
}

func (b *Bus) fanout(ev Event) {
	for ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Handler is an slog.Handler that passes records to next and to the bus.
// The "project" and "source" attributes become Entry fields.
type Handler struct {
	next  slog.Handler
	bus   *Bus
	attrs []slog.Attr
}

func NewHandler(next slog.Handler, bus *Bus) *Handler {
	return &Handler{next: next, bus: bus}
}

func (h *Handler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	e := Entry{Time: r.Time, Level: r.Level.String(), Msg: r.Message}
	add := func(a slog.Attr) bool {
		switch a.Key {
		case "project":
			e.Project = a.Value.String()
		case "source":
			e.Output = a.Value.String() == "output"
		default:
			if e.Attrs == nil {
				e.Attrs = map[string]string{}
			}
			e.Attrs[a.Key] = fmt.Sprint(a.Value.Any())
		}
		return true
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(add)
	h.bus.publish(e, true)
	return h.next.Handle(ctx, r)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{next: h.next.WithAttrs(attrs), bus: h.bus, attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...)}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{next: h.next.WithGroup(name), bus: h.bus, attrs: h.attrs}
}
