package auth

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// resetAllowlist makes the next check read the environment again.
func resetAllowlist(t *testing.T, logins, orgs string) {
	t.Helper()
	t.Setenv(allowedLoginsEnv, logins)
	t.Setenv(allowedOrgsEnv, orgs)
	allowlistOnce = sync.Once{}
	allowlist = nil
	allowAll = false
}

func TestAccessAllowlist(t *testing.T) {
	cases := []struct {
		name, logins, orgs, login string
		memberOf                  []string
		want                      bool
	}{
		{"empty lets nobody in", "", "", "someone", nil, false},
		{"star lets everyone in", "*", "", "someone", nil, true},
		{"listed login", "alice, Bob", "", "bob", nil, true},
		{"unlisted login", "alice", "", "mallory", nil, false},
		{"member of an allowed org", "", "eloicompany", "carol", []string{"EloiCompany"}, true},
		{"member of another org", "", "eloicompany", "carol", []string{"other"}, false},
		{"org claimed but no longer allowed", "", "", "carol", []string{"eloicompany"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resetAllowlist(t, c.logins, c.orgs)
			if got := IsAccessAllowed(c.login, c.memberOf); got != c.want {
				t.Fatalf("IsAccessAllowed(%q, %v) = %v, want %v", c.login, c.memberOf, got, c.want)
			}
		})
	}
}

func signed(t *testing.T, claims AppClaims) string {
	t.Helper()
	token, err := SignClaims(claims)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestParseJWT(t *testing.T) {
	jwtSecret = []byte("test-secret")
	resetAllowlist(t, "alice", "")
	future := jwt.NewNumericDate(time.Now().Add(time.Hour))

	access := signed(t, AppClaims{Login: "alice", RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: future}})
	if _, err := ParseJWT(access); err != nil {
		t.Fatalf("access token rejected: %v", err)
	}

	refresh := signed(t, AppClaims{Login: "alice", Use: "refresh", RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: future}})
	if _, err := ParseJWT(refresh); err == nil {
		t.Fatal("refresh token accepted as an access token")
	}

	outsider := signed(t, AppClaims{Login: "mallory", RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: future}})
	if _, err := ParseJWT(outsider); err == nil {
		t.Fatal("token of a login outside the allowlist accepted")
	}

	expired := signed(t, AppClaims{Login: "alice", RegisteredClaims: jwt.RegisteredClaims{
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Minute))}})
	if _, err := ParseJWT(expired); err == nil {
		t.Fatal("expired token accepted")
	}

	jwtSecret = []byte("another-secret")
	if _, err := ParseJWT(access); err == nil {
		t.Fatal("token signed with another key accepted")
	}
}

func TestLoginStateMustMatch(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_ID", "client")
	t.Setenv("GITHUB_CLIENT_SECRET", "secret")
	t.Setenv("JWT_SECRET", "test-secret")
	InitAuth()

	login := httptest.NewRecorder()
	HandleLogin(login, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	location, err := url.Parse(login.Header().Get("Location"))
	if err != nil || !strings.HasPrefix(location.String(), "https://github.com/") {
		t.Fatalf("login did not redirect to GitHub: %q", login.Header().Get("Location"))
	}
	state := location.Query().Get("state")
	var cookie *http.Cookie
	for _, c := range login.Result().Cookies() {
		if c.Name == stateCookieName {
			cookie = c
		}
	}
	if state == "" || cookie == nil || cookie.Value != state {
		t.Fatal("login did not bind the state to the browser")
	}

	for name, req := range map[string]*http.Request{
		"no cookie":     httptest.NewRequest(http.MethodGet, "/auth/callback?code=x&state="+state, nil),
		"other state":   withCookie(httptest.NewRequest(http.MethodGet, "/auth/callback?code=x&state=forged", nil), cookie),
		"state missing": withCookie(httptest.NewRequest(http.MethodGet, "/auth/callback?code=x", nil), cookie),
	} {
		rec := httptest.NewRecorder()
		HandleCallback(rec, req)
		if got := rec.Header().Get("Location"); got != "/auth/signed-out" {
			t.Fatalf("%s: callback went on to %q", name, got)
		}
	}

	matching := withCookie(httptest.NewRequest(http.MethodGet, "/auth/callback?state="+state, nil), cookie)
	if !stateMatches(httptest.NewRecorder(), matching) {
		t.Fatal("matching state rejected")
	}
}

func withCookie(r *http.Request, c *http.Cookie) *http.Request {
	r.AddCookie(c)
	return r
}

func TestReturnPathStaysOnSite(t *testing.T) {
	for target, want := range map[string]string{
		"/boards#room=abc":     "/boards#room=abc",
		"//evil.example/":      "/",
		"https://evil.example": "/",
	} {
		set := httptest.NewRecorder()
		rememberReturn(set, httptest.NewRequest(http.MethodGet, "/auth/login?return="+url.QueryEscape(target), nil))

		req := httptest.NewRequest(http.MethodGet, "/auth/callback", nil)
		for _, c := range set.Result().Cookies() {
			req.AddCookie(c)
		}
		if got := takeReturn(httptest.NewRecorder(), req); got != want {
			t.Fatalf("return %q came back as %q, want %q", target, got, want)
		}
	}
}
