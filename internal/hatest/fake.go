// Package hatest provides a fake Home Assistant WebSocket server for tests.
package hatest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"github.com/coder/websocket"
)

const Token = "test-token"

type ServiceCall struct {
	Domain, Service string
	Target          map[string]any
	Data            map[string]any
}

type Fake struct {
	srv   *httptest.Server
	Calls chan ServiceCall

	mu     sync.Mutex
	states map[string]map[string]any
	conns  map[*fconn]struct{}
}

type fconn struct {
	ws   *websocket.Conn
	mu   sync.Mutex
	subs map[int64]string // id -> event_type ("" = all)
}

func New() *Fake {
	f := &Fake{Calls: make(chan ServiceCall, 100), states: map[string]map[string]any{}, conns: map[*fconn]struct{}{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *Fake) URL() string { return "ws" + strings.TrimPrefix(f.srv.URL, "http") + "/api/websocket" }
func (f *Fake) Close()      { f.DropConnections(); f.srv.Close() }

// SetState sets an entity state and fires state_changed.
func (f *Fake) SetState(entityID, state string) {
	f.mu.Lock()
	old := f.states[entityID]
	n := map[string]any{"entity_id": entityID, "state": state, "attributes": map[string]any{}}
	f.states[entityID] = n
	f.mu.Unlock()
	f.Fire("state_changed", map[string]any{"entity_id": entityID, "old_state": old, "new_state": n})
}

// Fire sends an event to all matching subscriptions.
func (f *Fake) Fire(eventType string, data any) {
	f.mu.Lock()
	conns := make([]*fconn, 0, len(f.conns))
	for c := range f.conns {
		conns = append(conns, c)
	}
	f.mu.Unlock()
	for _, c := range conns {
		c.mu.Lock()
		for id, et := range c.subs {
			if et == "" || et == eventType {
				c.send(map[string]any{"id": id, "type": "event", "event": map[string]any{"event_type": eventType, "data": data}})
			}
		}
		c.mu.Unlock()
	}
}

// Subscriptions returns the number of active subscriptions over all connections.
func (f *Fake) Subscriptions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for c := range f.conns {
		c.mu.Lock()
		n += len(c.subs)
		c.mu.Unlock()
	}
	return n
}

// DropConnections closes all client connections (to test reconnects).
func (f *Fake) DropConnections() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for c := range f.conns {
		c.ws.CloseNow()
		delete(f.conns, c)
	}
}

func (c *fconn) send(v any) {
	b, _ := json.Marshal(v)
	_ = c.ws.Write(context.Background(), websocket.MessageText, b)
}

func (f *Fake) handle(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ctx := r.Context()
	c := &fconn{ws: ws, subs: map[int64]string{}}

	c.send(map[string]any{"type": "auth_required"})
	var auth struct {
		AccessToken string `json:"access_token"`
	}
	if _, b, err := ws.Read(ctx); err != nil || json.Unmarshal(b, &auth) != nil || auth.AccessToken != Token {
		c.send(map[string]any{"type": "auth_invalid", "message": "bad token"})
		return
	}
	c.send(map[string]any{"type": "auth_ok"})

	f.mu.Lock()
	f.conns[c] = struct{}{}
	f.mu.Unlock()
	defer func() { f.mu.Lock(); delete(f.conns, c); f.mu.Unlock() }()

	for {
		_, b, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var m struct {
			ID           int64          `json:"id"`
			Type         string         `json:"type"`
			EventType    string         `json:"event_type"`
			Subscription int64          `json:"subscription"`
			Domain       string         `json:"domain"`
			Service      string         `json:"service"`
			Target       map[string]any `json:"target"`
			ServiceData  map[string]any `json:"service_data"`
		}
		_ = json.Unmarshal(b, &m)
		ok := func(res any) { c.send(map[string]any{"id": m.ID, "type": "result", "success": true, "result": res}) }
		switch m.Type {
		case "get_states":
			f.mu.Lock()
			out := []any{}
			for _, s := range f.states {
				out = append(out, s)
			}
			f.mu.Unlock()
			ok(out)
		case "get_services":
			ok(map[string]any{"light": map[string]any{"turn_on": map[string]any{
				"fields": map[string]any{}, "target": map[string]any{"entity": []any{map[string]any{"domain": []string{"light"}}}}}}})
		case "subscribe_events":
			c.mu.Lock()
			c.subs[m.ID] = m.EventType
			c.mu.Unlock()
			ok(nil)
		case "unsubscribe_events":
			c.mu.Lock()
			delete(c.subs, m.Subscription)
			c.mu.Unlock()
			ok(nil)
		case "call_service":
			f.Calls <- ServiceCall{m.Domain, m.Service, m.Target, m.ServiceData}
			ok(map[string]any{"context": map[string]any{}})
		default:
			c.send(map[string]any{"id": m.ID, "type": "result", "success": false,
				"error": map[string]string{"code": "unknown_command", "message": "Unknown command."}})
		}
	}
}
