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
	"runtime/debug"
	"sync"
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
	stateFn map[string][]func(StateChange) // entity id ("" = all) -> handlers

	dispatch chan func()
	closed   chan struct{}
	err      error
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
		conn:     conn,
		enc:      json.NewEncoder(conn),
		pending:  map[int]chan message{},
		events:   map[int]func(json.RawMessage){},
		states:   map[string]*State{},
		stateFn:  map[string][]func(StateChange){},
		dispatch: make(chan func(), 1024),
		closed:   make(chan struct{}),
	}
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
	c.mu.Lock()
	c.stateFn[entityID] = append(c.stateFn[entityID], fn)
	c.mu.Unlock()
}

// OnEvent calls fn for every event of the given type ("" = all events).
func (c *Client) OnEvent(eventType string, fn func(Event)) error {
	msg := map[string]any{"type": "subscribe_events"}
	if eventType != "" {
		msg["event_type"] = eventType
	}
	return c.subscribe(msg, func(raw json.RawMessage) {
		var ev Event
		if err := json.Unmarshal(raw, &ev); err != nil {
			log.Printf("gohomeassistant: decode event: %v", err)
			return
		}
		fn(ev)
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
	c.dispatch <- fn
}

func (c *Client) call(msg map[string]any) (json.RawMessage, error) {
	return c.send(msg, nil)
}

// send sends msg and waits for its result. If handler is set, it is
// registered for events with the message id before sending.
func (c *Client) send(msg map[string]any, handler func(json.RawMessage)) (json.RawMessage, error) {
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
			c.dispatch <- func() { h(raw) }
		}
	}
}

func (c *Client) dispatchLoop() {
	for fn := range c.dispatch {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("gohomeassistant: handler panic: %v\n%s", r, debug.Stack())
				}
			}()
			fn()
		}()
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
	handlers := append(append([]func(StateChange){}, c.stateFn[d.EntityID]...), c.stateFn[""]...)
	c.mu.Unlock()

	ch := StateChange{EntityID: d.EntityID, Old: d.OldState, New: d.NewState}
	for _, h := range handlers {
		h(ch)
	}
}
