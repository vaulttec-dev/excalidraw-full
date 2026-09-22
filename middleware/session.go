package middleware

import (
	"context"
	"excalidraw-complete/handlers/auth"
	"fmt"
	"html"
	"net/http"
	"os"
	"strings"
)

// apiTokenEnv lets non-browser clients — the MCP server writing scenes — through
// the gate without a login round trip.
const apiTokenEnv = "API_TOKEN"

// RequireSession keeps the whole instance behind a GitHub login whenever OAuth
// is configured. Without GITHUB_CLIENT_ID (a bare local run) it stays out of the
// way, so the container is still usable without credentials.
func RequireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if os.Getenv("GITHUB_CLIENT_ID") == "" {
			next.ServeHTTP(w, r)
			return
		}

		// The login round trip itself must stay reachable.
		if strings.HasPrefix(r.URL.Path, "/auth/") {
			next.ServeHTTP(w, r)
			return
		}

		if claims, ok := authorize(r); ok {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ClaimsContextKey, claims)))
			return
		}

		// A browser is sent through the login; anything else gets a plain 401,
		// so a failing MCP call reads as a failure instead of as HTML.
		if strings.Contains(r.Header.Get("Accept"), "text/html") {
			bounce(w, r)
			return
		}

		http.Error(w, "authentication required", http.StatusUnauthorized)
	})
}

func authorize(r *http.Request) (*auth.AppClaims, bool) {
	if token := strings.TrimSpace(os.Getenv(apiTokenEnv)); token != "" {
		header := r.Header.Get("Authorization")
		if strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(header, "Bearer")), token) {
			return nil, true
		}
	}

	cookie, err := r.Cookie(auth.SessionCookieName)
	if err != nil {
		return nil, false
	}

	claims, err := auth.ParseJWT(cookie.Value)
	if err != nil {
		return nil, false
	}

	return claims, true
}

// bounce hands the browser a page that restarts the request through the login.
// It is a page rather than a redirect because the room a link points at lives in
// the URL fragment, which never reaches the server: only a script running in the
// browser can read it and carry it across the login.
func bounce(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)

	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><title>Signing in…</title>
<script>
location.replace("/auth/login?return=" + encodeURIComponent(location.pathname + location.search + location.hash));
</script>
<noscript>Open <a href="/auth/login">the sign-in page</a> to continue.</noscript>
<!-- %s -->`, html.EscapeString(r.URL.Path))
}
