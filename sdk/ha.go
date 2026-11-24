// Package ha is the SDK used by GoHomeAssistant projects.
//
// A project is a normal Go main package. It connects to the GoHomeAssistant
// host (which owns the Home Assistant connection), registers handlers and
// blocks in Run:
//
//	func main() {
//		c := ha.MustConnect()
//		c.OnStateChange("binary_sensor.front_door", func(ch ha.StateChange) {
//			if ch.New.State == "on" {
//				c.CallService("light", "turn_on", ha.Target{EntityID: []string{"light.hall"}}, nil)
//			}
//		})
//		c.Run()
//	}
//
// Handlers run one at a time on a single goroutine, so project code does not
// need locking. A panicking handler is logged and does not stop the project.
//
// Every handler run is reported to the host as an execution trace: what
// triggered it, what it logged (package log), which services it called and
// what they returned. The UI shows them in the Traces tab.
package ha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// State is the state of a single entity.
type State struct {
	EntityID    string         `json:"entity_id"`
	State       string         `json:"state"`
	Attributes  map[string]any `json:"attributes"`
	LastChanged time.Time      `json:"last_changed"`
	LastUpdated time.Time      `json:"last_updated"`
}

// StateChange is delivered to OnStateChange handlers. Old or New is nil when
// the entity was added or removed.
type StateChange struct {
	EntityID string
	Old, New *State
}

// Event is a raw Home Assistant event.
type Event struct {
	EventType string          `json:"event_type"`
	Data      json.RawMessage `json:"data"`
	TimeFired time.Time       `json:"time_fired"`
	Origin    string          `json:"origin"`
}

// Target selects what a service call acts on.
type Target struct {
	EntityID []string `json:"entity_id,omitempty"`
	DeviceID []string `json:"device_id,omitempty"`
	AreaID   []string `json:"area_id,omitempty"`
}

// Client is the connection of a project to the GoHomeAssistant host.
type Client struct {
	conn  net.Conn
	encMu sync.Mutex
	enc   *json.Encoder

	mu      sync.Mutex
	nextID  int
	pending map[int]chan message
	events  map[int]func(json.RawMessage) // subscription id -> handler (called on dispatch goroutine)
	states  map[string]*State
	stateFn map[string][]stateHandler // entity id ("" = all) -> handlers

	queue  queue         // handler runs, executed one at a time by dispatchLoop
	gid    atomic.Uint64 // goroutine id of dispatchLoop
	cur    *trace        // trace of the running handler; only used on that goroutine
	closed chan struct{}
	err    error
}

type stateHandler struct {
	fn  func(StateChange)
	src string // where it was registered, "main.go:12"
}

type message struct {
	ID      int             `json:"id"`
	Type    string          `json:"type"`
	Success bool            `json:"success"`
	Result  json.RawMessage `json:"result"`
	Event   json.RawMessage `json:"event"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// MustConnect is Connect that exits the process on error.
func MustConnect() *Client {
	c, err := Connect()
	if err != nil {
		log.Fatalf("gohomeassistant: %v", err)
	}
	return c
}

// Connect connects to the host via the socket in $GHA_SOCKET, subscribes to
// state changes and loads the current states into the local cache.
func Connect() (*Client, error) {
	path := os.Getenv("GHA_SOCKET")
	if path == "" {
		return nil, errors.New("GHA_SOCKET not set; projects must be started by GoHomeAssistant")
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, fmt.Errorf("connect to host: %w", err)
	}
	c := &Client{
		conn:    conn,
		enc:     json.NewEncoder(conn),
		pending: map[int]chan message{},
		events:  map[int]func(json.RawMessage){},
		states:  map[string]*State{},
		stateFn: map[string][]stateHandler{},
		closed:  make(chan struct{}),
	}
	c.queue.cond = sync.NewCond(&c.queue.mu)
	// The host timestamps every line, and traces record what handlers log.
	log.SetFlags(0)
	log.SetOutput(logTee{c})
	go c.readLoop()
	go c.dispatchLoop()

	// Subscribe first, then load: no change can be missed in between.
	if err := c.subscribe(map[string]any{"type": "subscribe_events", "event_type": "state_changed"}, c.handleStateChanged); err != nil {
		conn.Close()
		return nil, err
	}
	res, err := c.call(map[string]any{"type": "get_states"})
	if err != nil {
		conn.Close()
		return nil, err
	}
	var states []*State
	if err := json.Unmarshal(res, &states); err != nil {
		conn.Close()
		return nil, fmt.Errorf("decode states: %w", err)
	}
	c.mu.Lock()
	for _, s := range states {
		if _, ok := c.states[s.EntityID]; !ok { // a newer state may already have arrived
			c.states[s.EntityID] = s
		}
	}
	c.mu.Unlock()
	defaultClient.CompareAndSwap(nil, c)
	return c, nil
}

// State returns the cached state of an entity, or nil if it does not exist.
func (c *Client) State(entityID string) *State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.states[entityID]
}

// States returns all cached states.
func (c *Client) States() []*State {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*State, 0, len(c.states))
	for _, s := range c.states {
		out = append(out, s)
	}
	return out
}

// OnStateChange calls fn whenever the given entity changes. An empty entityID
// matches all entities.
func (c *Client) OnStateChange(entityID string, fn func(StateChange)) {
	c.onStateChange(entityID, fn, caller(1))
}

func (c *Client) onStateChange(entityID string, fn func(StateChange), src string) {
	c.mu.Lock()
	c.stateFn[entityID] = append(c.stateFn[entityID], stateHandler{fn, src})
	c.mu.Unlock()
}

// OnEvent calls fn for every event of the given type ("" = all events).
func (c *Client) OnEvent(eventType string, fn func(Event)) error {
	msg := map[string]any{"type": "subscribe_events"}
	if eventType != "" {
		msg["event_type"] = eventType
	}
	src := caller(1)
	return c.subscribe(msg, func(raw json.RawMessage) {
		var ev Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			log.Printf("gohomeassistant: decode event: %v", err)
			return
		}
		// "All events" handlers run far too often to report each run.
		c.invoke(&trace{Trigger: "event", Name: ev.EventType, Src: src, always: eventType != ""}, func() { fn(ev) })
	})
}

// CallService calls a Home Assistant service. data may be nil.
func (c *Client) CallService(domain, service string, target Target, data map[string]any) error {
	msg := map[string]any{"type": "call_service", "domain": domain, "service": service, "target": target}
	if data != nil {
		msg["service_data"] = data
	}
	_, err := c.call(msg)
	return err
}

// FireEvent fires a custom event on the Home Assistant event bus.
func (c *Client) FireEvent(eventType string, data map[string]any) error {
	msg := map[string]any{"type": "fire_event", "event_type": eventType}
	if data != nil {
		msg["event_data"] = data
	}
	_, err := c.call(msg)
	return err
}

// Call sends an arbitrary Home Assistant WebSocket command (without "id")
// and returns its result. Use it for commands the SDK does not wrap.
func (c *Client) Call(msg map[string]any) (json.RawMessage, error) {
	return c.call(msg)
}

// Run blocks until the process receives SIGINT/SIGTERM (the host stops or
// reloads the project) or the connection to the host is lost.
func (c *Client) Run() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
	case <-c.closed:
		log.Fatalf("gohomeassistant: connection to host lost: %v", c.err)
	}
	c.conn.Close()
}

// Go runs fn on the handler goroutine, e.g. from a timer:
//
//	time.AfterFunc(5*time.Minute, func() { c.Go(func() { ... }) })
func (c *Client) Go(fn func()) {
	src := caller(1)
	c.queue.push(func() { c.invoke(&trace{Trigger: "go", Src: src}, fn) })
}

func (c *Client) call(msg map[string]any) (json.RawMessage, error) {
	return c.send(msg, nil)
}

// send is roundTrip, recorded in the trace of the running handler.
func (c *Client) send(msg map[string]any, handler func(json.RawMessage)) (json.RawMessage, error) {
	done := c.traceCall(msg)
	res, err := c.roundTrip(msg, handler)
	done(err)
	return res, err
}

// roundTrip sends msg and waits for its result. If handler is set, it is
// registered for events with the message id before sending.
func (c *Client) roundTrip(msg map[string]any, handler func(json.RawMessage)) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan message, 1)
	c.pending[id] = ch
	if handler != nil {
		c.events[id] = handler
	}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	msg["id"] = id
	c.encMu.Lock()
	err := c.enc.Encode(msg)
	c.encMu.Unlock()
	if err != nil {
		return nil, err
	}
	select {
	case m := <-ch:
		if !m.Success {
			if m.Error != nil {
				return nil, fmt.Errorf("%s: %s: %s", msg["type"], m.Error.Code, m.Error.Message)
			}
			return nil, fmt.Errorf("%s failed", msg["type"])
		}
		return m.Result, nil
	case <-c.closed:
		return nil, fmt.Errorf("connection closed: %w", c.err)
	case <-ctx.Done():
		return nil, fmt.Errorf("%s: timeout", msg["type"])
	}
}

func (c *Client) subscribe(msg map[string]any, handler func(json.RawMessage)) error {
	_, err := c.send(msg, handler)
	if err != nil {
		c.mu.Lock()
		delete(c.events, msg["id"].(int))
		c.mu.Unlock()
	}
	return err
}

func (c *Client) readLoop() {
	dec := json.NewDecoder(c.conn)
	for {
		var m message
		if err := dec.Decode(&m); err != nil {
			c.err = err
			close(c.closed)
			return
		}
		c.mu.Lock()
		ch := c.pending[m.ID]
		h := c.events[m.ID]
		c.mu.Unlock()
		switch {
		case m.Type == "result" && ch != nil:
			ch <- m // buffered, one result per id
		case m.Type == "event" && h != nil:
			raw := m.Event
			c.queue.push(func() { h(raw) })
		case m.Type == "gha/reconnected":
			c.queue.push(c.resync)
		}
	}
}

// queue is an unbounded FIFO. The read loop must never wait for a handler:
// a handler that calls a service waits for the read loop to deliver the result.
type queue struct {
	mu    sync.Mutex
	cond  *sync.Cond
	items []func()
}

// maxQueue is how far handlers may fall behind before the project gives up
// (and is restarted by the host) instead of eating memory.
const maxQueue = 100_000

func (q *queue) push(fn func()) {
	q.mu.Lock()
	n := len(q.items)
	q.items = append(q.items, fn)
	q.mu.Unlock()
	q.cond.Signal()
	if n >= maxQueue {
		log.Fatalf("gohomeassistant: %d events are waiting for a handler that doesn't return; giving up", n)
	}
}

func (q *queue) pop() func() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 {
		q.cond.Wait()
	}
	fn := q.items[0]
	q.items[0] = nil
	q.items = q.items[1:]
	return fn
}

func (c *Client) dispatchLoop() {
	c.gid.Store(goid())
	for {
		c.queue.pop()()
	}
}

// resync reloads all states after the host lost and regained its connection
// to Home Assistant (e.g. HA was restarted). Changes that happened meanwhile
// are delivered to the handlers, so no automation keeps acting on old states.
func (c *Client) resync() {
	res, err := c.call(map[string]any{"type": "get_states"})
	var states []*State
	if err == nil {
		err = json.Unmarshal(res, &states)
	}
	if err != nil {
		log.Printf("gohomeassistant: reload states after reconnect: %v", err)
		return
	}
	fresh := make(map[string]*State, len(states))
	for _, s := range states {
		fresh[s.EntityID] = s
	}
	c.mu.Lock()
	old := c.states
	c.states = fresh
	c.mu.Unlock()
	for id, n := range fresh {
		if o := old[id]; o == nil || o.State != n.State || !o.LastUpdated.Equal(n.LastUpdated) {
			c.fire(StateChange{EntityID: id, Old: o, New: n})
		}
	}
	for id, o := range old {
		if fresh[id] == nil {
			c.fire(StateChange{EntityID: id, Old: o})
		}
	}
}

func (c *Client) handleStateChanged(raw json.RawMessage) {
	var ev struct {
		Data struct {
			EntityID string `json:"entity_id"`
			OldState *State `json:"old_state"`
			NewState *State `json:"new_state"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		log.Printf("gohomeassistant: decode state_changed: %v", err)
		return
	}
	d := ev.Data
	c.mu.Lock()
	if d.NewState == nil {
		delete(c.states, d.EntityID)
	} else {
		c.states[d.EntityID] = d.NewState
	}
	c.mu.Unlock()
	c.fire(StateChange{EntityID: d.EntityID, Old: d.OldState, New: d.NewState})
}

// fire runs the handlers of a state change, each with its own trace.
func (c *Client) fire(ch StateChange) {
	c.mu.Lock()
	specific := len(c.stateFn[ch.EntityID])
	handlers := append(append([]stateHandler{}, c.stateFn[ch.EntityID]...), c.stateFn[""]...)
	c.mu.Unlock()
	for i, h := range handlers {
		// "All entities" handlers run far too often to report each run.
		t := &trace{Trigger: "state", Name: ch.EntityID, Src: h.src, always: i < specific && ch.EntityID != ""}
		if ch.Old != nil {
			t.From = &ch.Old.State
		}
		if ch.New != nil {
			t.To = &ch.New.State
		}
		c.invoke(t, func() { h.fn(ch) })
	}
}
