package ha_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/gusimo/gohomeassistant/internal/ha"
	"github.com/gusimo/gohomeassistant/internal/hatest"
)

func TestCallSubscribeAndReconnect(t *testing.T) {
	fake := hatest.New()
	defer fake.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := ha.New(fake.URL(), hatest.Token, slog.New(slog.DiscardHandler))
	go c.Run(ctx)
	waitConnected(t, c)

	res, err := c.Call(ctx, map[string]any{"type": "call_service", "domain": "light", "service": "turn_on"})
	if err != nil || !strings.Contains(string(res), `"success":true`) {
		t.Fatalf("call: %v %s", err, res)
	}
	if call := <-fake.Calls; call.Domain != "light" || call.Service != "turn_on" {
		t.Fatalf("unexpected call %+v", call)
	}

	events := make(chan string, 10)
	sub, _, err := c.Subscribe(ctx, map[string]any{"type": "subscribe_events", "event_type": "state_changed"}, func(ev json.RawMessage) {
		var e struct {
			Data struct {
				EntityID string `json:"entity_id"`
			}
		}
		_ = json.Unmarshal(ev, &e)
		events <- e.Data.EntityID
	})
	if err != nil || sub == nil {
		t.Fatalf("subscribe: %v", err)
	}
	fake.SetState("light.a", "on")
	expect(t, events, "light.a")

	// Subscriptions must survive a dropped connection.
	fake.DropConnections()
	deadline := time.Now().Add(5 * time.Second)
	for fake.Subscriptions() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("not resubscribed after reconnect")
		}
		time.Sleep(20 * time.Millisecond)
	}
	fake.SetState("light.b", "on")
	expect(t, events, "light.b")

	sub.Close(ctx)
	if n := fake.Subscriptions(); n != 0 {
		t.Fatalf("subscriptions after close: %d", n)
	}
}

func TestBadToken(t *testing.T) {
	fake := hatest.New()
	defer fake.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := ha.New(fake.URL(), "wrong", slog.New(slog.DiscardHandler))
	go c.Run(ctx)
	time.Sleep(200 * time.Millisecond)
	if _, err := c.Call(ctx, map[string]any{"type": "get_states"}); err != ha.ErrNotConnected {
		t.Fatalf("want ErrNotConnected, got %v", err)
	}
}

func waitConnected(t *testing.T, c *ha.Client) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := c.Call(context.Background(), map[string]any{"type": "get_states"}); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("not connected")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func expect(t *testing.T, ch chan string, want string) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout waiting for %q", want)
	}
}
