package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"excalidraw-complete/handlers/auth"

	"github.com/golang-jwt/jwt/v5"
)

func gate(t *testing.T) http.Handler {
	t.Helper()
	t.Setenv("GITHUB_CLIENT_ID", "client")
	t.Setenv("GITHUB_CLIENT_SECRET", "secret")
	t.Setenv("JWT_SECRET", "test-secret")
	t.Setenv("ALLOWED_GITHUB_LOGINS", "*")
	auth.InitAuth()
	return RequireSession(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
}

func serve(h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestPublicPathsSkipTheGate(t *testing.T) {
	h := gate(t)
	for _, path := range []string{
		"/healthz", "/sw.js", "/manifest.webmanifest", "/auth/login",
		"/.well-known/oauth-protected-resource", "/oauth/token",
	} {
		if rec := serve(h, httptest.NewRequest(http.MethodGet, path, nil)); rec.Code != http.StatusTeapot {
			t.Errorf("%s: status %d, want it passed through", path, rec.Code)
		}
	}
}

func TestGateWithoutSession(t *testing.T) {
	h := gate(t)

	page := httptest.NewRequest(http.MethodGet, "/", nil)
	page.Header.Set("Accept", "text/html")
	rec := serve(h, page)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "/auth/login") {
		t.Fatalf("browser got %d without the sign-in screen", rec.Code)
	}

	rec = serve(h, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("MCP got %d, want 401", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("WWW-Authenticate"), "/.well-known/oauth-protected-resource") {
		t.Fatalf("MCP 401 does not say where to sign in: %q", rec.Header().Get("WWW-Authenticate"))
	}

	bogus := httptest.NewRequest(http.MethodGet, "/api/boards", nil)
	bogus.Header.Set("Authorization", "Bearer not-a-token")
	if rec := serve(h, bogus); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bogus bearer got %d, want 401", rec.Code)
	}
}

func TestGateWithSession(t *testing.T) {
	h := gate(t)
	token, err := auth.SignClaims(auth.AppClaims{
		Login:            "alice",
		RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))},
	})
	if err != nil {
		t.Fatal(err)
	}

	withCookie := httptest.NewRequest(http.MethodGet, "/api/boards", nil)
	withCookie.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: token})
	if rec := serve(h, withCookie); rec.Code != http.StatusTeapot {
		t.Fatalf("session cookie got %d", rec.Code)
	}

	withBearer := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	withBearer.Header.Set("Authorization", "Bearer "+token)
	if rec := serve(h, withBearer); rec.Code != http.StatusTeapot {
		t.Fatalf("access token got %d", rec.Code)
	}
}
