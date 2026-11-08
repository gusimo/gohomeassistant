// Package logbus collects log records and project status changes and fans
// them out to UI clients, with a bounded history for late joiners.
package logbus

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Entry is one log line.
type Entry struct {
	Seq     int64             `json:"seq"`
	Time    time.Time         `json:"time"`
	Level   string            `json:"level"`
	Project string            `json:"project,omitempty"` // "" = host
	Output  bool              `json:"output,omitempty"`  // stdout/stderr of the project
	Msg     string            `json:"msg"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// Event is sent to subscribers: either a log entry or a status (any JSON).
type Event struct {
	Type    string `json:"type"` // "log", "status" or "files"
	Log     *Entry `json:"log,omitempty"`
	Status  any    `json:"status,omitempty"`
	Project string `json:"project,omitempty"` // for "files": files of this project changed on disk
}

type Bus struct {
	mu   sync.Mutex
	ring []Entry
	next int // ring write position once full
	max  int
	seq  int64
	subs map[chan Event]struct{}
}

func New(history int) *Bus {
	return &Bus{max: history, subs: map[chan Event]struct{}{}}
}

// History returns the buffered entries, oldest first.
func (b *Bus) History() []Entry {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.ring) < b.max {
		return append([]Entry(nil), b.ring...)
	}
	return append(append([]Entry(nil), b.ring[b.next:]...), b.ring[:b.next]...)
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

// PublishFiles tells clients that files of a project changed on disk.
func (b *Bus) PublishFiles(project string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fanout(Event{Type: "files", Project: project})
}

func (b *Bus) publish(e Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	e.Seq = b.seq
	if len(b.ring) < b.max {
		b.ring = append(b.ring, e)
	} else {
		b.ring[b.next] = e
		b.next = (b.next + 1) % b.max
	}
	b.fanout(Event{Type: "log", Log: &e})
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
	h.bus.publish(e)
	return h.next.Handle(ctx, r)
}

func (h *Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &Handler{next: h.next.WithAttrs(attrs), bus: h.bus, attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...)}
}

func (h *Handler) WithGroup(name string) slog.Handler {
	return &Handler{next: h.next.WithGroup(name), bus: h.bus, attrs: h.attrs}
}
