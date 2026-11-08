// Package bridge lets project processes use the host's Home Assistant
// connection. Projects speak a subset of the HA WebSocket protocol as
// newline-delimited JSON over a unix socket; the bridge maps their ids to
// the shared HA connection and cleans up their subscriptions when they exit.
package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/gusimo/gohomeassistant/internal/ha"
)

type Bridge struct {
	ha  *ha.Client
	log *slog.Logger
}

func New(client *ha.Client, log *slog.Logger) *Bridge {
	return &Bridge{ha: client, log: log}
}

// Serve listens on the unix socket at path until ctx is cancelled.
func (b *Bridge) Serve(ctx context.Context, path string) error {
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go b.handle(ctx, conn)
	}
}

type session struct {
	b    *Bridge
	out  chan []byte
	mu   sync.Mutex
	subs map[int64]*ha.Subscription // project id -> subscription
	done chan struct{}
}

func (b *Bridge) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	s := &session{b: b, out: make(chan []byte, 4096), subs: map[int64]*ha.Subscription{}, done: make(chan struct{})}
	defer s.cleanup()

	go func() { // single writer
		for {
			select {
			case m := <-s.out:
				if _, err := conn.Write(m); err != nil {
					conn.Close()
					return
				}
			case <-s.done:
				conn.Close()
				return
			}
		}
	}()

	dec := json.NewDecoder(conn)
	for {
		var msg map[string]any
		if err := dec.Decode(&msg); err != nil {
			return
		}
		go s.request(ctx, msg)
	}
}

func (s *session) request(ctx context.Context, msg map[string]any) {
	idf, ok := msg["id"].(float64)
	if !ok {
		return
	}
	id := int64(idf)
	delete(msg, "id")
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	switch msg["type"] {
	case "subscribe_events", "subscribe_trigger", "subscribe_entities":
		sub, res, err := s.b.ha.Subscribe(ctx, msg, func(ev json.RawMessage) {
			s.send(map[string]any{"id": id, "type": "event", "event": ev})
		})
		if err != nil {
			s.sendErr(id, err)
			return
		}
		if sub != nil {
			s.mu.Lock()
			select {
			case <-s.done: // project went away while subscribing
				s.mu.Unlock()
				sub.Close(ctx)
				return
			default:
			}
			s.subs[id] = sub
			s.mu.Unlock()
		}
		s.sendResult(id, res)

	case "unsubscribe_events":
		subID, _ := msg["subscription"].(float64)
		s.mu.Lock()
		sub := s.subs[int64(subID)]
		delete(s.subs, int64(subID))
		s.mu.Unlock()
		if sub == nil {
			s.sendErr(id, errors.New("unknown subscription"))
			return
		}
		sub.Close(ctx)
		s.send(map[string]any{"id": id, "type": "result", "success": true, "result": nil})

	default:
		res, err := s.b.ha.Call(ctx, msg)
		if err != nil {
			s.sendErr(id, err)
			return
		}
		s.sendResult(id, res)
	}
}

// sendResult forwards an HA result message with the project's id.
func (s *session) sendResult(id int64, res json.RawMessage) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(res, &m); err != nil {
		s.sendErr(id, err)
		return
	}
	m["id"], _ = json.Marshal(id)
	s.send(m)
}

func (s *session) sendErr(id int64, err error) {
	s.send(map[string]any{"id": id, "type": "result", "success": false,
		"error": map[string]string{"code": "gohomeassistant", "message": err.Error()}})
}

func (s *session) send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	b = append(b, '\n')
	select {
	case s.out <- b:
	case <-s.done:
	default:
		// The project stopped reading. Dropping events silently would be
		// worse than failing loudly; the SDK exits and the host restarts it.
		// send runs on the HA read loop, so clean up asynchronously.
		s.b.log.Error("project not reading messages, disconnecting")
		go s.cleanup()
	}
}

func (s *session) cleanup() {
	s.mu.Lock()
	select {
	case <-s.done:
		s.mu.Unlock()
		return
	default:
	}
	close(s.done)
	subs := s.subs
	s.subs = map[int64]*ha.Subscription{}
	s.mu.Unlock()
	for _, sub := range subs {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		sub.Close(ctx)
		cancel()
	}
}
