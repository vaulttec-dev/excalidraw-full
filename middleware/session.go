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

// BaseURL is the public origin of the instance as the client reached it.
func BaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// HealthPath is the health check endpoint; it tells nothing about the boards.
const HealthPath = "/healthz"

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

		// The login round trip itself must stay reachable, and so must the health
		// check, which the container runtime calls without a session, and the
		// service worker script: browsers fetch it without a session to update
		// the worker, and it is what removes a stale one.
		// The OAuth endpoints MCP clients sign in through are public by nature;
		// the one that needs a person checks the session itself.
		if strings.HasPrefix(r.URL.Path, "/auth/") || r.URL.Path == HealthPath ||
			r.URL.Path == "/sw.js" || r.URL.Path == "/service-worker.js" ||
			strings.HasPrefix(r.URL.Path, "/.well-known/") || strings.HasPrefix(r.URL.Path, "/oauth/") {
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

		// MCP clients discover where to sign in from this header.
		if strings.HasPrefix(r.URL.Path, "/mcp") {
			w.Header().Set("WWW-Authenticate",
				`Bearer resource_metadata="`+BaseURL(r)+`/.well-known/oauth-protected-resource"`)
		}
		http.Error(w, "authentication required", http.StatusUnauthorized)
	})
}

func authorize(r *http.Request) (*auth.AppClaims, bool) {
	if bearer := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer")); bearer != "" {
		if token := strings.TrimSpace(os.Getenv(apiTokenEnv)); token != "" && bearer == token {
			return nil, true
		}
		// Access tokens this instance issued through OAuth, to MCP clients.
		if claims, err := auth.ParseJWT(bearer); err == nil {
			return claims, true
		}
		// Clients without a browser — the MCP server — sign in with the
		// person's own GitHub token, held to the same allowlist as a browser
		// login, so no secret has to be shared across the team.
		if claims, ok := auth.VerifyGitHubToken(r.Context(), bearer); ok {
			return claims, true
		}
		return nil, false
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

// bounce hands the browser the sign-in screen. It asks for a click instead of
// redirecting straight to GitHub: the browser usually still holds a GitHub
// session, so an automatic redirect would sign a user back in the moment they
// open a board after signing out. The button's target is filled in by a script
// because the room a link points at lives in the URL fragment, which never
// reaches the server and would otherwise be lost across the login.
func bounce(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)

	fmt.Fprintf(w, signInPage, html.EscapeString(r.URL.Path))
}

const signInPage = `<!doctype html>
<html lang="uk">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Вхід</title>
<style>
  :root { --bg: #f8f9fa; --surface: #fff; --text: #1b1b1f; --muted: #6b6b76; --border: #e4e4eb; --accent: #6965db; }
  @media (prefers-color-scheme: dark) {
    :root { --bg: #121214; --surface: #1c1c21; --text: #ececf1; --muted: #9a9aa6; --border: #2c2c34; --accent: #8b87ff; }
  }
  body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: var(--bg); color: var(--text);
         font: 15px/1.5 system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; padding: 16px; box-sizing: border-box; }
  .card { background: var(--surface); border: 1px solid var(--border); border-radius: 14px; padding: 32px 28px;
          max-width: 360px; width: 100%%; text-align: center; }
  h1 { font-size: 20px; margin: 0 0 8px; }
  p { color: var(--muted); margin: 0 0 24px; }
  a { display: inline-flex; align-items: center; gap: 8px; background: var(--accent); color: #fff; text-decoration: none;
      padding: 10px 18px; border-radius: 10px; font-weight: 600; }
  a:hover { filter: brightness(1.08); }
  svg { width: 18px; height: 18px; fill: currentColor; }
</style>
</head>
<body>
  <div class="card">
    <h1>Дошки команди</h1>
    <p>Увійдіть через GitHub, щоб відкрити дошки.</p>
    <a id="login" href="/auth/login">
      <svg viewBox="0 0 16 16" aria-hidden="true"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.290 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.150-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z"/></svg>
      Увійти через GitHub
    </a>
  </div>
<script>
document.getElementById("login").href =
  "/auth/login?return=" + encodeURIComponent(location.pathname + location.search + location.hash);
</script>
<!-- %s -->
</body>
</html>
`
