// Package auth restricts the UI to Home Assistant administrators.
//
// Modes:
//   - ingress: running as an add-on; only requests proxied by the Supervisor
//     (HA Ingress, which already authenticated the user) are accepted.
//   - ha: standalone; users log in with Home Assistant's OAuth flow and must
//     be HA administrators. A server-side session is kept in memory.
//   - none: no authentication (development only).
//
// The UI can run code with full access to Home Assistant, so non-admins are
// always rejected.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

type Mode string

const (
	None    Mode = "none"
	Ingress Mode = "ingress"
	HA      Mode = "ha"
)

// SupervisorIP is the address HA Ingress requests come from.
const SupervisorIP = "172.30.32.2"

const (
	sessionCookie = "gha_session"
	stateCookie   = "gha_oauth_state"
	sessionTTL    = 30 * 24 * time.Hour
)

type User struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Admin bool   `json:"admin"`
}

type Auth struct {
	Mode        Mode
	HAURL       string // base URL of HA for server-side calls, e.g. http://192.168.1.5:8123
	HAPublicURL string // optional: HA's URL as the browser reaches it, if different
	PublicURL   string // optional: fixed base URL of GoHomeAssistant (otherwise derived from the request)
	Log         *slog.Logger
	Client      *http.Client

	mu       sync.Mutex
	sessions map[string]session
}

type session struct {
	user    User
	expires time.Time
}

type ctxKey struct{}

// UserFrom returns the authenticated user of a request.
func UserFrom(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(ctxKey{}).(User)
	return u, ok
}

// Register adds the login routes and /api/me.
func (a *Auth) Register(mux *http.ServeMux) {
	if a.Mode == HA {
		mux.HandleFunc("GET /auth/login", a.login)
		mux.HandleFunc("GET /auth/callback", a.callback)
		mux.HandleFunc("POST /auth/logout", a.logout)
	}
	mux.HandleFunc("GET /api/me", func(w http.ResponseWriter, r *http.Request) {
		u, _ := UserFrom(r.Context())
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"user": u, "mode": a.Mode})
	})
}

// Wrap authenticates every request except the login routes.
func (a *Auth) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Mode == HA && (r.URL.Path == "/auth/login" || r.URL.Path == "/auth/callback") {
			next.ServeHTTP(w, r)
			return
		}
		user, ok := a.authenticate(w, r)
		if !ok {
			return
		}
		// Session cookies are also sent for requests from other ports of the
		// same host (same site), so check the origin of every change. (Under
		// Ingress, HA's own session protects us and Origin is HA's host.)
		if a.Mode == HA && r.Method != http.MethodGet && r.Method != http.MethodHead && !sameOrigin(r) {
			http.Error(w, "cross-origin request rejected", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, user)))
	})
}

func (a *Auth) authenticate(w http.ResponseWriter, r *http.Request) (User, bool) {
	switch a.Mode {
	case None:
		return User{ID: "local", Name: "local (no authentication)", Admin: true}, true

	case Ingress:
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		if host != SupervisorIP {
			http.Error(w, "only reachable through Home Assistant (Ingress)", http.StatusForbidden)
			return User{}, false
		}
		name := r.Header.Get("X-Remote-User-Display-Name")
		if name == "" {
			name = r.Header.Get("X-Remote-User-Name")
		}
		return User{ID: r.Header.Get("X-Remote-User-Id"), Name: name, Admin: true}, true

	case HA:
		if c, err := r.Cookie(sessionCookie); err == nil {
			if u, ok := a.session(c.Value); ok {
				return u, true
			}
		}
		if r.Method == http.MethodGet && !strings.HasPrefix(r.URL.Path, "/api/") {
			redirectRelative(w, relative(r.URL.Path, "auth/login"))
		} else {
			http.Error(w, "not logged in", http.StatusUnauthorized)
		}
		return User{}, false
	}
	http.Error(w, "auth misconfigured", http.StatusInternalServerError)
	return User{}, false
}

func (a *Auth) login(w http.ResponseWriter, r *http.Request) {
	state := randomToken()
	http.SetCookie(w, &http.Cookie{
		Name: stateCookie, Value: state, Path: "/auth/", MaxAge: 600,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.secure(r),
	})
	base := a.baseURL(r)
	q := url.Values{
		"client_id":    {base + "/"},
		"redirect_uri": {base + "/auth/callback"},
		"state":        {state},
	}
	haURL := a.HAURL
	if a.HAPublicURL != "" {
		haURL = a.HAPublicURL
	}
	http.Redirect(w, r, strings.TrimSuffix(haURL, "/")+"/auth/authorize?"+q.Encode(), http.StatusFound)
}

func (a *Auth) callback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(stateCookie)
	state := r.URL.Query().Get("state")
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		http.Error(w, "login expired or invalid, please try again", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Path: "/auth/", MaxAge: -1})
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing code", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	tok, err := a.exchange(ctx, code, a.baseURL(r)+"/")
	if err != nil {
		a.Log.Warn("login: token exchange failed", "err", err)
		http.Error(w, "login failed", http.StatusBadGateway)
		return
	}
	// We only need to know who logged in; don't keep HA's tokens around.
	defer a.revoke(tok.RefreshToken)
	user, err := a.currentUser(ctx, tok.AccessToken)
	if err != nil {
		a.Log.Warn("login: cannot read user", "err", err)
		http.Error(w, "login failed", http.StatusBadGateway)
		return
	}
	if !user.Admin {
		a.Log.Warn("login rejected: not an administrator", "user", user.Name)
		http.Error(w, "Only Home Assistant administrators can use GoHomeAssistant.", http.StatusForbidden)
		return
	}

	id := randomToken()
	a.mu.Lock()
	if a.sessions == nil {
		a.sessions = map[string]session{}
	}
	a.sessions[id] = session{user: user, expires: time.Now().Add(sessionTTL)}
	a.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: id, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: a.secure(r),
	})
	a.Log.Info("logged in", "user", user.Name)
	redirectRelative(w, "../")
}

func (a *Auth) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.mu.Lock()
		delete(a.sessions, c.Value)
		a.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	w.WriteHeader(http.StatusNoContent)
}

func (a *Auth) session(id string) (User, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[id]
	if !ok {
		return User{}, false
	}
	if time.Now().After(s.expires) {
		delete(a.sessions, id)
		return User{}, false
	}
	return s.user, true
}

type tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

func (a *Auth) exchange(ctx context.Context, code, clientID string) (tokens, error) {
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(a.HAURL, "/")+"/auth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := a.client().Do(req)
	if err != nil {
		return tokens{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return tokens{}, fmt.Errorf("token endpoint: %s", res.Status)
	}
	var t tokens
	if err := json.NewDecoder(res.Body).Decode(&t); err != nil || t.AccessToken == "" {
		return tokens{}, errors.New("token endpoint: bad response")
	}
	return t, nil
}

func (a *Auth) revoke(refreshToken string) {
	if refreshToken == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	form := url.Values{"token": {refreshToken}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(a.HAURL, "/")+"/auth/revoke", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if res, err := a.client().Do(req); err == nil {
		res.Body.Close()
	}
}

// currentUser asks HA who owns the access token (auth/current_user).
func (a *Auth) currentUser(ctx context.Context, accessToken string) (User, error) {
	u, err := url.Parse(strings.TrimSuffix(a.HAURL, "/") + "/api/websocket")
	if err != nil {
		return User{}, err
	}
	u.Scheme = map[string]string{"https": "wss"}[u.Scheme]
	if u.Scheme == "" {
		u.Scheme = "ws"
	}
	conn, _, err := websocket.Dial(ctx, u.String(), nil)
	if err != nil {
		return User{}, err
	}
	defer conn.CloseNow()
	read := func(v any) error {
		_, b, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		return json.Unmarshal(b, v)
	}
	write := func(v any) error {
		b, _ := json.Marshal(v)
		return conn.Write(ctx, websocket.MessageText, b)
	}
	var m struct {
		Type    string `json:"type"`
		Success bool   `json:"success"`
		Result  struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			IsAdmin bool   `json:"is_admin"`
			IsOwner bool   `json:"is_owner"`
		} `json:"result"`
	}
	if err := read(&m); err != nil {
		return User{}, err
	}
	if err := write(map[string]string{"type": "auth", "access_token": accessToken}); err != nil {
		return User{}, err
	}
	if err := read(&m); err != nil || m.Type != "auth_ok" {
		return User{}, fmt.Errorf("websocket auth failed: %v %s", err, m.Type)
	}
	if err := write(map[string]any{"id": 1, "type": "auth/current_user"}); err != nil {
		return User{}, err
	}
	if err := read(&m); err != nil || !m.Success {
		return User{}, fmt.Errorf("auth/current_user failed: %v", err)
	}
	return User{ID: m.Result.ID, Name: m.Result.Name, Admin: m.Result.IsAdmin || m.Result.IsOwner}, nil
}

func (a *Auth) baseURL(r *http.Request) string {
	if a.PublicURL != "" {
		return strings.TrimSuffix(a.PublicURL, "/")
	}
	scheme := "http"
	if a.secure(r) {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (a *Auth) secure(r *http.Request) bool {
	if a.PublicURL != "" {
		return strings.HasPrefix(a.PublicURL, "https://")
	}
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
}

func (a *Auth) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}
	return http.DefaultClient
}

// sameOrigin reports whether a state-changing request comes from our own
// page. Browsers always send Origin on such fetches; tools like curl don't,
// and can't carry the user's cookie either.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

// relative returns a URL relative to the request path, so redirects also
// work under a path prefix.
func relative(path, target string) string {
	depth := strings.Count(strings.TrimPrefix(path, "/"), "/")
	return strings.Repeat("../", depth) + target
}

// redirectRelative keeps the target relative (http.Redirect would make it
// absolute, which breaks behind a reverse proxy with a path prefix).
func redirectRelative(w http.ResponseWriter, target string) {
	w.Header().Set("Location", target)
	w.WriteHeader(http.StatusFound)
}

func randomToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
