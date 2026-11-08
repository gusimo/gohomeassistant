// Command gohomeassistant runs Go automation projects against Home Assistant.
//
// Configuration (environment):
//
//	SUPERVISOR_TOKEN  set automatically when running as an HA add-on
//	HA_URL            e.g. http://homeassistant.local:8123 (standalone)
//	HA_TOKEN          long-lived access token (standalone)
//	GHA_PROJECTS_DIR  default /data/projects
//	GHA_DATA_DIR      default /data/gohomeassistant (build outputs)
//	GHA_SDK_DIR       default /opt/gohomeassistant/sdk
//	GHA_WATCH_POLL    e.g. 1s: also poll for file changes (needed for Docker
//	                  bind mounts on macOS/Windows, where file events don't arrive)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gusimo/gohomeassistant/internal/bridge"
	"github.com/gusimo/gohomeassistant/internal/ha"
	"github.com/gusimo/gohomeassistant/internal/project"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	wsURL, token, err := haEndpoint()
	if err != nil {
		return err
	}
	dataDir := env("GHA_DATA_DIR", "/data/gohomeassistant")
	sdkDir, err := filepath.Abs(env("GHA_SDK_DIR", "/opt/gohomeassistant/sdk"))
	if err != nil {
		return err
	}
	projectsDir, err := filepath.Abs(env("GHA_PROJECTS_DIR", "/data/projects"))
	if err != nil {
		return err
	}
	var poll time.Duration
	if v := os.Getenv("GHA_WATCH_POLL"); v != "" {
		if poll, err = time.ParseDuration(v); err != nil {
			return fmt.Errorf("GHA_WATCH_POLL: %w", err)
		}
	}
	// unix socket paths are limited to ~104 bytes, so keep it short
	socket := filepath.Join(os.TempDir(), fmt.Sprintf("gha-%d.sock", os.Getpid()))
	defer os.Remove(socket)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	client := ha.New(wsURL, token, log)
	go client.Run(ctx)

	br := bridge.New(client, log)
	go func() {
		if err := br.Serve(ctx, socket); err != nil {
			log.Error("bridge", "err", err)
			stop()
		}
	}()

	mgr, err := project.NewManager(project.Config{
		Root:     projectsDir,
		BuildDir: filepath.Join(dataDir, "build"),
		SDKDir:   sdkDir,
		Socket:   socket,
		Log:      log,
		Poll:     poll,
	})
	if err != nil {
		return err
	}
	log.Info("gohomeassistant started", "projects", projectsDir)
	return mgr.Run(ctx)
}

// haEndpoint returns the WebSocket URL and token, preferring the add-on
// environment.
func haEndpoint() (string, string, error) {
	if t := os.Getenv("SUPERVISOR_TOKEN"); t != "" {
		return "ws://supervisor/core/websocket", t, nil
	}
	raw, token := os.Getenv("HA_URL"), os.Getenv("HA_TOKEN")
	if raw == "" || token == "" {
		return "", "", fmt.Errorf("set HA_URL and HA_TOKEN (or run as add-on)")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("HA_URL: %w", err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/api/websocket"
	return u.String(), token, nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
