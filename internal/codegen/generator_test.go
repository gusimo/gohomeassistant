package codegen

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gusimo/gohomeassistant/internal/ha"
	"github.com/gusimo/gohomeassistant/internal/hatest"
)

func TestGeneratorRegeneratesOnNewEntity(t *testing.T) {
	fake := hatest.New()
	defer fake.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := slog.New(slog.DiscardHandler)
	client := ha.New(fake.URL(), hatest.Token, log)
	go client.Run(ctx)

	changed := make(chan struct{}, 10)
	g := &Generator{Client: client, Dir: filepath.Join(t.TempDir(), "ent"), GoVersion: "1.24", Log: log,
		OnChange: func() { changed <- struct{}{} }}
	if err := g.EnsureModule(); err != nil {
		t.Fatal(err)
	}
	go g.Run(ctx)

	fake.SetState("light.kitchen", "on")
	waitFor(t, changed, g.Dir, "Kitchen LightEntity")

	fake.SetState("light.hall", "off") // new entity: old_state is null
	waitFor(t, changed, g.Dir, "Hall LightEntity")
}

func waitFor(t *testing.T, changed chan struct{}, dir, want string) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case <-changed:
			src, _ := os.ReadFile(filepath.Join(dir, "ent.go"))
			if strings.Contains(string(src), want) {
				return
			}
		case <-deadline:
			t.Fatalf("ent.go never contained %q", want)
		}
	}
}
