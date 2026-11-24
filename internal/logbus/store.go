package logbus

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Disk limits: a segment is closed at segmentSize, and the oldest segments
// are deleted once all of them exceed totalSize.
const (
	segmentSize = 4 << 20
	totalSize   = 64 << 20
)

// store appends entries as JSON lines to segment files "log-<first seq>.jsonl".
// Every start begins a new segment, so a line cut off by a crash never gets
// glued to the next one.
type store struct {
	dir  string
	f    *os.File
	size int64
	err  bool // the last write failed (reported once)
}

// Open returns a bus that also keeps logs and traces in dir. What was written
// before the last restart is loaded into the history.
func Open(dir string, history int) (*Bus, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	b := New(history)
	b.store = &store{dir: dir}

	// Newest segments first, until both rings are full.
	var logs, traces []Entry // newest first
	segs := b.store.segments()
	for i := len(segs) - 1; i >= 0 && (len(logs) < history || len(traces) < traceHistory); i-- {
		entries := readSegment(segs[i].path)
		for j := len(entries) - 1; j >= 0; j-- {
			e := entries[j]
			b.seq = max(b.seq, e.Seq)
			if e.Trace != nil && len(traces) < traceHistory {
				traces = append(traces, e)
			} else if e.Trace == nil && len(logs) < history {
				logs = append(logs, e)
			}
		}
	}
	for i := len(logs) - 1; i >= 0; i-- {
		b.logs.add(logs[i])
	}
	for i := len(traces) - 1; i >= 0; i-- {
		b.traces.add(traces[i])
	}
	return b, nil
}

// Filter selects entries in Query.
type Filter struct {
	Before  int64      // only entries with a smaller Seq; 0 = newest
	Limit   int        // maximum number of entries
	Traces  bool       // traces instead of log lines
	Project *string    // nil = all, "" = host
	Level   slog.Level // minimum level
	Text    string     // must occur in the message, attributes or trace (case-insensitive)
}

func (f *Filter) match(e *Entry) bool {
	if (e.Trace != nil) != f.Traces || (f.Before > 0 && e.Seq >= f.Before) {
		return false
	}
	if f.Project != nil && e.Project != *f.Project {
		return false
	}
	var l slog.Level
	if l.UnmarshalText([]byte(e.Level)) == nil && l < f.Level {
		return false
	}
	if f.Text == "" {
		return true
	}
	text := strings.ToLower(f.Text)
	if strings.Contains(strings.ToLower(e.Msg), text) || bytes.Contains(bytes.ToLower(e.Trace), []byte(text)) {
		return true
	}
	for _, v := range e.Attrs {
		if strings.Contains(strings.ToLower(v), text) {
			return true
		}
	}
	return false
}

// Query returns the newest entries on disk that match f, oldest first. It
// reaches further back than History; without a disk store it returns nothing.
func (b *Bus) Query(f Filter) []Entry {
	if b.store == nil {
		return nil
	}
	b.mu.Lock()
	segs := b.store.segments()
	b.mu.Unlock()
	var out []Entry // newest first
	for i := len(segs) - 1; i >= 0 && len(out) < f.Limit; i-- {
		if f.Before > 0 && segs[i].first >= f.Before {
			continue
		}
		data, err := os.ReadFile(segs[i].path)
		if err != nil {
			continue // deleted meanwhile
		}
		lines := bytes.Split(data, []byte("\n"))
		for j := len(lines) - 1; j >= 0 && len(out) < f.Limit; j-- {
			var e Entry
			if len(lines[j]) == 0 || json.Unmarshal(lines[j], &e) != nil {
				continue
			}
			if f.match(&e) {
				out = append(out, e)
			}
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

type segment struct {
	path  string
	first int64 // Seq of its first entry
	size  int64
}

// segments lists the segment files, oldest first.
func (s *store) segments() []segment {
	names, _ := filepath.Glob(filepath.Join(s.dir, "log-*.jsonl"))
	segs := make([]segment, 0, len(names))
	for _, n := range names {
		base := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(n), "log-"), ".jsonl")
		first, err := strconv.ParseInt(base, 10, 64)
		fi, err2 := os.Stat(n)
		if err != nil || err2 != nil {
			continue
		}
		segs = append(segs, segment{path: n, first: first, size: fi.Size()})
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].first < segs[j].first })
	return segs
}

func readSegment(path string) []Entry {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		var e Entry
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Seq > 0 {
			out = append(out, e)
		}
	}
	return out
}

// append writes e. Logging must never fail because the disk does: errors are
// reported once on stderr and writing is retried with the next entry.
func (s *store) append(e Entry) {
	line, err := json.Marshal(e)
	if err == nil {
		err = s.write(e.Seq, append(line, '\n'))
	}
	if err != nil && !s.err {
		fmt.Fprintf(os.Stderr, "gohomeassistant: cannot write log to %s: %v\n", s.dir, err)
	}
	s.err = err != nil
}

func (s *store) write(seq int64, line []byte) error {
	if s.f == nil || s.size+int64(len(line)) > segmentSize {
		if s.f != nil {
			s.f.Close()
			s.f = nil
		}
		s.prune()
		f, err := os.OpenFile(filepath.Join(s.dir, fmt.Sprintf("log-%020d.jsonl", seq)), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		s.f, s.size = f, 0
	}
	n, err := s.f.Write(line)
	s.size += int64(n)
	return err
}

// prune deletes the oldest segments until there is room for a new one.
func (s *store) prune() {
	segs := s.segments()
	var total int64
	for _, seg := range segs {
		total += seg.size
	}
	for i := 0; i < len(segs) && total > totalSize-segmentSize; i++ {
		os.Remove(segs[i].path)
		total -= segs[i].size
	}
}
