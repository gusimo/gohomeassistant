// Package ha is the host's connection to the Home Assistant WebSocket API.
//
// The client reconnects forever. Subscriptions survive reconnects: they are
// re-sent after every successful authentication, so callers never notice.
package ha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"
)

var ErrNotConnected = errors.New("not connected to Home Assistant")

// Client is a reconnecting Home Assistant WebSocket client.
type Client struct {
	url, token string
	log        *slog.Logger

	mu      sync.Mutex
	conn    *websocket.Conn
	nextID  int64
	pending map[int64]chan json.RawMessage
	subs    map[*Subscription]struct{}
	byHAID  map[int64]*Subscription
}

// Subscription is an event subscription that is restored on reconnect.
type Subscription struct {
	c       *Client
	msg     map[string]any
	handler func(event json.RawMessage)
	haID    int64 // current id on the HA connection, 0 while disconnected
}

func New(url, token string, log *slog.Logger) *Client {
	return &Client{
		url:     url,
		token:   token,
		log:     log,
		pending: map[int64]chan json.RawMessage{},
		subs:    map[*Subscription]struct{}{},
		byHAID:  map[int64]*Subscription{},
	}
}

// Run connects and keeps the connection alive until ctx is cancelled.
func (c *Client) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := c.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		c.log.Warn("home assistant connection lost", "err", err, "retry_in", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// Call sends a command (without "id") and returns the full result message
// (type "result", including "success" and "result" or "error").
func (c *Client) Call(ctx context.Context, msg map[string]any) (json.RawMessage, error) {
	c.mu.Lock()
	conn := c.conn
	if conn == nil {
		c.mu.Unlock()
		return nil, ErrNotConnected
	}
	c.nextID++
	id := c.nextID
	ch := make(chan json.RawMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.write(ctx, conn, id, msg); err != nil {
		return nil, err
	}
	select {
	case res, ok := <-ch:
		if !ok {
			return nil, ErrNotConnected
		}
		return res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Subscribe sends a subscribe command (e.g. subscribe_events) and calls
// handler with the "event" field of every event message. It returns the
// result message of the first subscription attempt.
func (c *Client) Subscribe(ctx context.Context, msg map[string]any, handler func(json.RawMessage)) (*Subscription, json.RawMessage, error) {
	s := &Subscription{c: c, msg: msg, handler: handler}
	res, err := c.subscribe(ctx, s)
	if err != nil {
		return nil, nil, err
	}
	if !success(res) {
		return nil, res, nil
	}
	c.mu.Lock()
	c.subs[s] = struct{}{}
	c.mu.Unlock()
	return s, res, nil
}

// Close unsubscribes. It is safe to call while disconnected.
func (s *Subscription) Close(ctx context.Context) {
	c := s.c
	c.mu.Lock()
	delete(c.subs, s)
	id := s.haID
	delete(c.byHAID, id)
	s.haID = 0
	c.mu.Unlock()
	if id != 0 {
		_, _ = c.Call(ctx, map[string]any{"type": "unsubscribe_events", "subscription": id})
	}
}

// subscribe sends s.msg and registers s under the new HA id before sending,
// so no event is lost.
func (c *Client) subscribe(ctx context.Context, s *Subscription) (json.RawMessage, error) {
	c.mu.Lock()
	conn := c.conn
	if conn == nil {
		c.mu.Unlock()
		return nil, ErrNotConnected
	}
	c.nextID++
	id := c.nextID
	ch := make(chan json.RawMessage, 1)
	c.pending[id] = ch
	s.haID = id
	c.byHAID[id] = s
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	fail := func() {
		c.mu.Lock()
		if s.haID == id {
			delete(c.byHAID, id)
			s.haID = 0
		}
		c.mu.Unlock()
	}
	if err := c.write(ctx, conn, id, s.msg); err != nil {
		fail()
		return nil, err
	}
	select {
	case res, ok := <-ch:
		if !ok {
			fail()
			return nil, ErrNotConnected
		}
		if !success(res) {
			fail()
		}
		return res, nil
	case <-ctx.Done():
		fail()
		return nil, ctx.Err()
	}
}

func (c *Client) write(ctx context.Context, conn *websocket.Conn, id int64, msg map[string]any) error {
	out := make(map[string]any, len(msg)+1)
	for k, v := range msg {
		out[k] = v
	}
	out["id"] = id
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, b)
}

// session runs one connection from dial to failure.
func (c *Client) session(ctx context.Context) error {
	conn, _, err := websocket.Dial(ctx, c.url, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(64 << 20) // get_states can be large

	if err := c.auth(ctx, conn); err != nil {
		return err
	}
	c.log.Info("connected to home assistant", "url", c.url)

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	readErr := make(chan error, 1)
	go func() { readErr <- c.readLoop(sctx, conn) }()

	c.mu.Lock()
	c.conn = conn
	subs := make([]*Subscription, 0, len(c.subs))
	for s := range c.subs {
		subs = append(subs, s)
	}
	c.mu.Unlock()
	defer c.disconnect()

	for _, s := range subs {
		res, err := c.subscribe(sctx, s)
		if err != nil {
			return fmt.Errorf("resubscribe: %w", err)
		}
		if !success(res) {
			c.log.Error("resubscribe rejected", "msg", s.msg, "result", string(res))
		}
	}

	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case err := <-readErr:
			return err
		case <-ping.C:
			pctx, pcancel := context.WithTimeout(sctx, 10*time.Second)
			err := conn.Ping(pctx)
			pcancel()
			if err != nil {
				return fmt.Errorf("ping: %w", err)
			}
		case <-ctx.Done():
			conn.Close(websocket.StatusNormalClosure, "")
			return ctx.Err()
		}
	}
}

func (c *Client) auth(ctx context.Context, conn *websocket.Conn) error {
	actx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var m struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	read := func() error {
		_, b, err := conn.Read(actx)
		if err != nil {
			return fmt.Errorf("auth: %w", err)
		}
		return json.Unmarshal(b, &m)
	}
	if err := read(); err != nil {
		return err
	}
	if m.Type != "auth_required" {
		return fmt.Errorf("auth: unexpected %q", m.Type)
	}
	b, _ := json.Marshal(map[string]string{"type": "auth", "access_token": c.token})
	if err := conn.Write(actx, websocket.MessageText, b); err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	if err := read(); err != nil {
		return err
	}
	if m.Type != "auth_ok" {
		return fmt.Errorf("auth failed: %s %s", m.Type, m.Message)
	}
	return nil
}

func (c *Client) readLoop(ctx context.Context, conn *websocket.Conn) error {
	for {
		_, b, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var m struct {
			ID    int64           `json:"id"`
			Type  string          `json:"type"`
			Event json.RawMessage `json:"event"`
		}
		if err := json.Unmarshal(b, &m); err != nil {
			c.log.Warn("bad message from home assistant", "err", err)
			continue
		}
		c.mu.Lock()
		if ch := c.pending[m.ID]; m.Type == "result" && ch != nil {
			ch <- b // buffered; under lock so disconnect cannot close it concurrently
		}
		s := c.byHAID[m.ID]
		c.mu.Unlock()
		if m.Type == "event" && s != nil {
			s.handler(m.Event) // must not block
		}
	}
}

// disconnect fails all pending calls and marks subscriptions as inactive.
func (c *Client) disconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conn = nil
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	for id, s := range c.byHAID {
		s.haID = 0
		delete(c.byHAID, id)
	}
}

func success(res json.RawMessage) bool {
	var r struct {
		Success bool `json:"success"`
	}
	return json.Unmarshal(res, &r) == nil && r.Success
}
