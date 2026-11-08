package auth

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gusimo/gohomeassistant/internal/hatest"
)

// setup returns a GoHomeAssistant-like server protected by Auth in HA mode.
func setup(t *testing.T) (*hatest.Fake, *httptest.Server) {
	t.Helper()
	fake := hatest.New()
	t.Cleanup(fake.Close)
	a := &Auth{Mode: HA, HAURL: fake.HTTPURL(), Log: slog.New(slog.DiscardHandler)}
	mux := http.NewServeMux()
	a.Register(mux)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		u, _ := UserFrom(r.Context())
		io.WriteString(w, "hello "+u.Name)
	})
	srv := httptest.NewServer(a.Wrap(mux))
	t.Cleanup(srv.Close)
	return fake, srv
}

func noRedirect() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// login runs the OAuth flow; HA's login page is simulated by sending the
// browser straight to the callback with the given code.
func login(t *testing.T, c *http.Client, srv *httptest.Server, code string) *http.Response {
	t.Helper()
	res, err := c.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "auth/login" {
		t.Fatalf("unauthenticated / should redirect to auth/login, got %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	res, err = c.Get(srv.URL + "/auth/login")
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := url.Parse(res.Header.Get("Location"))
	q := loc.Query()
	if loc.Path != "/auth/authorize" || q.Get("client_id") != srv.URL+"/" || q.Get("redirect_uri") != srv.URL+"/auth/callback" || q.Get("state") == "" {
		t.Fatalf("bad authorize redirect: %s", loc)
	}
	res, err = c.Get(srv.URL + "/auth/callback?" + url.Values{"code": {code}, "state": {q.Get("state")}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func get(t *testing.T, c *http.Client, u string) (int, string) {
	t.Helper()
	res, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestAdminLogin(t *testing.T) {
	fake, srv := setup(t)
	c := noRedirect()
	res := login(t, c, srv, hatest.AdminCode)
	if res.StatusCode != http.StatusFound || res.Header.Get("Location") != "../" {
		t.Fatalf("callback: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	if code, body := get(t, c, srv.URL+"/"); code != 200 || body != "hello Admin" {
		t.Fatalf("after login: %d %q", code, body)
	}
	if code, body := get(t, c, srv.URL+"/api/me"); code != 200 || !strings.Contains(body, `"name":"Admin"`) {
		t.Fatalf("/api/me: %d %q", code, body)
	}
	time.Sleep(100 * time.Millisecond) // revoke runs after the response
	if !slices.Contains(fake.Revoked(), "refresh-access-admin") {
		t.Errorf("HA refresh token not revoked: %v", fake.Revoked())
	}

	// Logout ends the session.
	req, _ := http.NewRequest("POST", srv.URL+"/auth/logout", nil)
	if res, _ := c.Do(req); res.StatusCode != http.StatusNoContent {
		t.Fatalf("logout: %d", res.StatusCode)
	}
	if code, _ := get(t, c, srv.URL+"/api/me"); code != http.StatusUnauthorized {
		t.Errorf("after logout: %d", code)
	}
}

func TestNonAdminRejected(t *testing.T) {
	_, srv := setup(t)
	c := noRedirect()
	if res := login(t, c, srv, hatest.UserCode); res.StatusCode != http.StatusForbidden {
		t.Fatalf("non-admin login: %d", res.StatusCode)
	}
	if code, _ := get(t, c, srv.URL+"/api/me"); code != http.StatusUnauthorized {
		t.Errorf("non-admin got a session: %d", code)
	}
}

func TestRejections(t *testing.T) {
	_, srv := setup(t)
	c := noRedirect()
	if code, _ := get(t, c, srv.URL+"/api/projects"); code != http.StatusUnauthorized {
		t.Errorf("API without session: %d", code)
	}
	// Callback without the state cookie (CSRF / replay).
	if code, _ := get(t, noRedirect(), srv.URL+"/auth/callback?code="+hatest.AdminCode+"&state=forged"); code != http.StatusBadRequest {
		t.Errorf("forged state: %d", code)
	}
	if code, _ := get(t, c, srv.URL+"/api/projects?session=x"); code != http.StatusUnauthorized {
		t.Errorf("fake session: %d", code)
	}

	login(t, c, srv, hatest.AdminCode)
	req, _ := http.NewRequest("POST", srv.URL+"/api/projects", strings.NewReader(`{}`))
	req.Header.Set("Origin", "http://evil.example")
	if res, _ := c.Do(req); res.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin POST: %d", res.StatusCode)
	}
	req, _ = http.NewRequest("POST", srv.URL+"/api/projects", strings.NewReader(`{}`))
	req.Header.Set("Origin", srv.URL)
	if res, _ := c.Do(req); res.StatusCode == http.StatusForbidden {
		t.Errorf("same-origin POST rejected")
	}
}

func TestIngressOnlyFromSupervisor(t *testing.T) {
	a := &Auth{Mode: Ingress, Log: slog.New(slog.DiscardHandler)}
	h := a.Wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := UserFrom(r.Context())
		io.WriteString(w, u.Name)
	}))
	for _, tc := range []struct {
		remote string
		want   int
	}{{SupervisorIP + ":41234", 200}, {"192.168.1.20:5555", 403}, {"127.0.0.1:5555", 403}} {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = tc.remote
		req.Header.Set("X-Remote-User-Display-Name", "Guido")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: got %d, want %d", tc.remote, rec.Code, tc.want)
		}
		if tc.want == 200 && rec.Body.String() != "Guido" {
			t.Errorf("user from ingress headers: %q", rec.Body.String())
		}
	}
}
