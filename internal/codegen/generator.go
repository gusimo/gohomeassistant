package codegen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gusimo/gohomeassistant/internal/ha"
)

// Generator keeps the generated module in Dir in sync with Home Assistant.
type Generator struct {
	Client    *ha.Client
	Dir       string // module directory
	GoVersion string
	Log       *slog.Logger
	OnChange  func() // called after ent.go changed (rebuild projects)

	kick       chan struct{}
	subscribed map[string]bool // event types already subscribed

	mu   sync.Mutex
	refs map[string]string
}

// Refs returns entity ID -> Go expression of the last generation.
func (g *Generator) Refs() map[string]string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.refs
}

// EnsureModule writes go.mod and a stub ent.go if they don't exist yet, so
// projects build before the first generation.
func (g *Generator) EnsureModule() error {
	if err := os.MkdirAll(g.Dir, 0o755); err != nil {
		return err
	}
	mod := fmt.Appendf(nil, "module %s\n\ngo %s\n", ModulePath, g.GoVersion)
	if _, err := writeIfChanged(filepath.Join(g.Dir, "go.mod"), mod); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(g.Dir, "p"), 0o755); err != nil {
		return err
	}
	for path, stub := range map[string][]byte{"ent.go": Stub(), "p/p.go": StubParams()} {
		full := filepath.Join(g.Dir, filepath.FromSlash(path))
		if _, err := os.Stat(full); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(full, stub, 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

// Run regenerates on start, whenever entities or services are added or
// removed, and every 10 minutes as a safety net (e.g. after reconnects).
func (g *Generator) Run(ctx context.Context) {
	g.kick = make(chan struct{}, 1)
	g.subscribed = map[string]bool{}
	subscribed := false
	safety := time.NewTicker(10 * time.Minute)
	defer safety.Stop()
	for {
		if !subscribed {
			subscribed = g.subscribe(ctx) == nil
		}
		err := g.generate(ctx)
		if err != nil {
			g.Log.Warn("entity generation failed, retrying", "err", err)
		}
		wait := time.Duration(0) // wait for a kick
		if err != nil || !subscribed {
			wait = 2 * time.Second
		}
		if !g.waitForKick(ctx, safety.C, wait) {
			return
		}
	}
}

// waitForKick blocks until something changed (debounced), the safety ticker
// fires or, if retry > 0, after retry. It returns false when ctx is done.
func (g *Generator) waitForKick(ctx context.Context, safety <-chan time.Time, retry time.Duration) bool {
	var retryC <-chan time.Time
	if retry > 0 {
		retryC = time.After(retry)
	}
	select {
	case <-ctx.Done():
		return false
	case <-safety:
		return true
	case <-retryC:
		return true
	case <-g.kick:
	}
	for { // let bursts settle, e.g. an integration adding 50 entities
		select {
		case <-ctx.Done():
			return false
		case <-g.kick:
		case <-time.After(3 * time.Second):
			return true
		}
	}
}

func (g *Generator) subscribe(ctx context.Context) error {
	kick := func(json.RawMessage) {
		select {
		case g.kick <- struct{}{}:
		default:
		}
	}
	handlers := map[string]func(json.RawMessage){
		"entity_registry_updated": kick,
		"service_registered":      kick,
		"service_removed":         kick,
		// Entities without a registry entry (YAML, templates) only show up
		// as state_changed with a missing old or new state.
		"state_changed": func(ev json.RawMessage) {
			var e struct {
				Data struct {
					Old json.RawMessage `json:"old_state"`
					New json.RawMessage `json:"new_state"`
				} `json:"data"`
			}
			if json.Unmarshal(ev, &e) == nil && (isNull(e.Data.Old) || isNull(e.Data.New)) {
				kick(nil)
			}
		},
	}
	for et, h := range handlers {
		if g.subscribed[et] {
			continue
		}
		if _, _, err := g.Client.Subscribe(ctx, map[string]any{"type": "subscribe_events", "event_type": et}, h); err != nil {
			return err
		}
		g.subscribed[et] = true
	}
	return nil
}

func (g *Generator) generate(ctx context.Context) error {
	var states []State
	if err := g.call(ctx, "get_states", &states); err != nil {
		return err
	}
	var services Services
	if err := g.call(ctx, "get_services", &services); err != nil {
		return err
	}
	files, refs, err := Generate(states, services)
	if err != nil {
		return err
	}
	g.mu.Lock()
	g.refs = refs
	g.mu.Unlock()
	// p first: the new ent.go may refer to new parameter types.
	changedP, err := writeIfChanged(filepath.Join(g.Dir, "p", "p.go"), files.Params)
	if err != nil {
		return err
	}
	changedEnt, err := writeIfChanged(filepath.Join(g.Dir, "ent.go"), files.Ent)
	if err != nil || !(changedP || changedEnt) {
		return err
	}
	g.Log.Info("entities regenerated", "entities", len(states), "service_domains", len(services))
	if g.OnChange != nil {
		g.OnChange()
	}
	return nil
}

func (g *Generator) call(ctx context.Context, typ string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := g.Client.Call(ctx, map[string]any{"type": typ})
	if err != nil {
		return err
	}
	var res struct {
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	if !res.Success {
		return fmt.Errorf("%s failed: %s", typ, raw)
	}
	return json.Unmarshal(res.Result, out)
}

// writeIfChanged writes atomically (rename), so a concurrent build never
// sees a half-written file.
func writeIfChanged(path string, data []byte) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return false, nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return false, err
	}
	return true, os.Rename(tmp, path)
}

func isNull(raw json.RawMessage) bool {
	return len(raw) == 0 || string(raw) == "null"
}
