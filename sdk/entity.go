package ha

import (
	"errors"
	"strconv"
	"sync/atomic"
)

// ErrNotConnected is returned by package-level helpers before Connect.
var ErrNotConnected = errors.New("gohomeassistant: not connected (call ha.Connect first)")

var defaultClient atomic.Pointer[Client]

// Default returns the client created by the first successful Connect, or nil.
// Generated entity code uses it, so projects don't pass the client around.
func Default() *Client { return defaultClient.Load() }

// CallService calls a service using the default client.
func CallService(domain, service string, target Target, data map[string]any) error {
	c := Default()
	if c == nil {
		return ErrNotConnected
	}
	return c.CallService(domain, service, target, data)
}

// Entity is a reference to a Home Assistant entity. Generated code embeds it
// in domain-specific types (ent.LightEntity, ...).
type Entity struct {
	ID string
}

// State returns the cached state, or nil if the entity doesn't exist.
func (e Entity) State() *State {
	if c := Default(); c != nil {
		return c.State(e.ID)
	}
	return nil
}

// Value returns the state string ("on", "21.5", "unavailable", ...), or ""
// if the entity doesn't exist.
func (e Entity) Value() string {
	if s := e.State(); s != nil {
		return s.State
	}
	return ""
}

// IsOn reports whether the state is "on".
func (e Entity) IsOn() bool { return e.Value() == "on" }

// Float parses the state as a number. ok is false for non-numeric states
// such as "unavailable" or "unknown".
func (e Entity) Float() (v float64, ok bool) {
	v, err := strconv.ParseFloat(e.Value(), 64)
	return v, err == nil
}

// Attr returns an attribute, or nil.
func (e Entity) Attr(name string) any {
	if s := e.State(); s != nil {
		return s.Attributes[name]
	}
	return nil
}

// OnChange calls fn whenever this entity's state or attributes change.
func (e Entity) OnChange(fn func(StateChange)) {
	if c := Default(); c != nil {
		c.OnStateChange(e.ID, fn)
		return
	}
	panic(ErrNotConnected)
}

// CallService calls domain.service targeting this entity.
func (e Entity) CallService(domain, service string, data map[string]any) error {
	return CallService(domain, service, Target{EntityID: []string{e.ID}}, data)
}
