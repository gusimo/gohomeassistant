package logbus

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestHistorySurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	b, err := Open(dir, 3)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(NewHandler(slog.NewTextHandler(io.Discard, nil), b))
	for _, m := range []string{"one", "two", "three", "four"} {
		log.With("project", "demo").Info(m)
	}
	log.Error("host broke", "err", "disk full")
	b.PublishTrace("demo", time.Now(), true, true, json.RawMessage(`{"trigger":"state"}`))
	b.PublishTrace("demo", time.Now(), false, false, json.RawMessage(`{"trigger":"empty"}`))

	b, err = Open(dir, 3) // restart
	if err != nil {
		t.Fatal(err)
	}
	h := b.History()
	if len(h) != 3 || h[0].Msg != "three" || h[2].Msg != "host broke" || h[2].Seq != 5 {
		t.Fatalf("history after restart: %+v", h)
	}
	if tr := b.Traces(); len(tr) != 1 || string(tr[0].Trace) != `{"trigger":"state"}` || tr[0].Level != "ERROR" {
		t.Fatalf("traces after restart (the empty one is not kept): %+v", tr)
	}
	slog.New(NewHandler(slog.NewTextHandler(io.Discard, nil), b)).Info("after restart")
	if h := b.History(); h[2].Seq != 7 {
		t.Fatalf("seq must continue after the highest one on disk, got %d", h[2].Seq)
	}

	// Query reaches further back than the ring, across segments.
	demo := "demo"
	got := b.Query(Filter{Limit: 2, Before: 4, Project: &demo})
	if len(got) != 2 || got[0].Msg != "two" || got[1].Msg != "three" {
		t.Fatalf("query before 4: %+v", got)
	}
	if got := b.Query(Filter{Limit: 10, Level: slog.LevelWarn}); len(got) != 1 || got[0].Msg != "host broke" {
		t.Fatalf("query by level: %+v", got)
	}
	if got := b.Query(Filter{Limit: 10, Text: "DISK"}); len(got) != 1 {
		t.Fatalf("query by text in attributes: %+v", got)
	}
	if got := b.Query(Filter{Limit: 10, Traces: true}); len(got) != 1 {
		t.Fatalf("query traces: %+v", got)
	}
}

func TestStoreRotatesAndPrunes(t *testing.T) {
	dir := t.TempDir()
	b, err := Open(dir, 10)
	if err != nil {
		t.Fatal(err)
	}
	msg := string(make([]byte, 1<<20)) // ~6 MB per line as JSON (\u0000)
	for range 20 {
		b.publish(Entry{Msg: msg}, true)
	}
	var total int64
	segs := b.store.segments()
	for _, s := range segs {
		total += s.size
	}
	if len(segs) < 2 || total > totalSize {
		t.Fatalf("%d segments, %d bytes", len(segs), total)
	}
	if got := b.Query(Filter{Limit: 1}); len(got) != 1 || got[0].Seq != 20 {
		t.Fatalf("newest entry lost: %d entries", len(got))
	}
}
