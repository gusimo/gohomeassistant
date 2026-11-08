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
//	GHA_HTTP_ADDR     default :8099 (web UI)
//	GHA_AUTH          standalone: "ha" (default, log in with HA, admins only)
//	                  or "none" (no authentication, development only)
//	GHA_PUBLIC_URL    optional: URL the browser uses for GoHomeAssistant
//	GHA_HA_PUBLIC_URL optional: URL the browser uses for HA, if not HA_URL
//	GHA_WATCH_POLL    e.g. 1s: also poll for file changes (needed for Docker
//	                  bind mounts on macOS/Windows, where file events don't arrive)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gusimo/gohomeassistant/internal/auth"
	"github.com/gusimo/gohomeassistant/internal/bridge"
	"github.com/gusimo/gohomeassistant/internal/codegen"
	"github.com/gusimo/gohomeassistant/internal/ha"
	"github.com/gusimo/gohomeassistant/internal/logbus"
	"github.com/gusimo/gohomeassistant/internal/project"
	"github.com/gusimo/gohomeassistant/internal/server"
)

func main() {
	bus := logbus.New(5000)
	log := slog.New(logbus.NewHandler(slog.NewTextHandler(os.Stderr, nil), bus))
	if err := run(log, bus); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, bus *logbus.Bus) error {
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

	goVersion, err := project.GoVersion()
	if err != nil {
		return err
	}
	gen := &codegen.Generator{Client: client, Dir: filepath.Join(dataDir, "gen", "ent"), GoVersion: goVersion, Log: log}
	if err := gen.EnsureModule(); err != nil {
		return err
	}

	mgr, err := project.NewManager(project.Config{
		Root:     projectsDir,
		BuildDir: filepath.Join(dataDir, "build"),
		Modules:  []string{sdkDir, gen.Dir},
		Socket:   socket,
		Log:      log,
		Poll:     poll,
		OnStatus: func(st project.Status) { bus.PublishStatus(st) },
		OnFiles:  bus.PublishFiles,
	})
	if err != nil {
		return err
	}
	gen.OnChange = mgr.TriggerAll
	go gen.Run(ctx)

	authn, err := authConfig(log)
	if err != nil {
		return err
	}
	web := server.New(mgr, bus, goVersion, log)
	web.Auth = authn
	web.SkipOriginCheck = authn.Mode == auth.Ingress
	web.HA, web.Refs = client, gen.Refs
	addr := env("GHA_HTTP_ADDR", ":8099")
	srv := &http.Server{Addr: addr, Handler: web.Handler()}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http server", "err", err)
			stop()
		}
	}()
	go func() { <-ctx.Done(); srv.Close() }()
	log.Info("web UI", "addr", addr, "auth", authn.Mode)

	log.Info("gohomeassistant started", "projects", projectsDir)
	return mgr.Run(ctx)
}

// authConfig picks the authentication mode: Ingress inside the add-on,
// otherwise HA login unless explicitly disabled.
func authConfig(log *slog.Logger) (*auth.Auth, error) {
	a := &auth.Auth{
		HAURL:       os.Getenv("HA_URL"),
		HAPublicURL: os.Getenv("GHA_HA_PUBLIC_URL"),
		PublicURL:   os.Getenv("GHA_PUBLIC_URL"),
		Log:         log,
	}
	switch mode := os.Getenv("GHA_AUTH"); {
	case os.Getenv("SUPERVISOR_TOKEN") != "":
		a.Mode = auth.Ingress
	case mode == "" || mode == "ha":
		a.Mode = auth.HA
	case mode == "none":
		a.Mode = auth.None
		log.Warn("GHA_AUTH=none: the web UI has NO authentication; anyone who can reach it controls Home Assistant")
	default:
		return nil, fmt.Errorf("GHA_AUTH: unknown mode %q (use ha or none)", mode)
	}
	return a, nil
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
