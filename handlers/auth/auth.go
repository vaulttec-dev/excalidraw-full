package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"excalidraw-complete/core"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/github"

	"encoding/hex"

	"github.com/coreos/go-oidc/v3/oidc"
)

var (
	loginHandler    http.HandlerFunc
	callbackHandler http.HandlerFunc
)

var (
	githubOauthConfig *oauth2.Config
	jwtSecret         []byte

	oidcOauthConfig *oauth2.Config
	oidcProvider    *oidc.Provider
	verifier        *oidc.IDTokenVerifier
)

// AppClaims represents the custom claims for the JWT.
type AppClaims struct {
	jwt.RegisteredClaims
	Login     string `json:"login"`
	Email     string `json:"email,omitempty"`
	AvatarURL string `json:"avatarUrl"`
	Name      string `json:"name"`
}

// OIDCClaims represents the claims from OIDC token
type OIDCClaims struct {
	Email             string `json:"email"`
	Name              string `json:"name"`
	PreferredUsername string `json:"preferred_username"`
	Picture           string `json:"picture"`
	Sub               string `json:"sub"`
}

func InitAuth() {
	oidcConfigured := os.Getenv("OIDC_ISSUER_URL") != "" && os.Getenv("OIDC_CLIENT_ID") != ""
	githubConfigured := os.Getenv("GITHUB_CLIENT_ID") != "" && os.Getenv("GITHUB_CLIENT_SECRET") != ""

	if oidcConfigured {
		logrus.Info("Initializing OIDC authentication provider.")
		initOIDC()
		loginHandler = HandleOIDCLogin
		callbackHandler = HandleOIDCCallback
	} else if githubConfigured {
		logrus.Info("Initializing GitHub authentication provider.")
		initGitHub()
		loginHandler = HandleGitHubLogin
		callbackHandler = HandleGitHubCallback
	} else {
		logrus.Warn("No authentication provider configured.")
		dummyHandler := func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "Authentication not configured", http.StatusInternalServerError)
		}
		loginHandler = dummyHandler
		callbackHandler = dummyHandler
	}

	jwtSecret = []byte(os.Getenv("JWT_SECRET"))
	if len(jwtSecret) == 0 {
		logrus.Warn("JWT_SECRET is not set. Authentication will not work.")
	}
}

func HandleLogin(w http.ResponseWriter, r *http.Request) {
	if loginHandler != nil {
		loginHandler(w, r)
	} else {
		http.Error(w, "Authentication not configured", http.StatusInternalServerError)
	}
}

func HandleCallback(w http.ResponseWriter, r *http.Request) {
	if callbackHandler != nil {
		callbackHandler(w, r)
	} else {
		http.Error(w, "Authentication not configured", http.StatusInternalServerError)
	}
}

func initGitHub() {
	githubOauthConfig = &oauth2.Config{
		ClientID:     os.Getenv("GITHUB_CLIENT_ID"),
		ClientSecret: os.Getenv("GITHUB_CLIENT_SECRET"),
		RedirectURL:  os.Getenv("GITHUB_REDIRECT_URL"),
		Scopes:       []string{"read:user", "user:email"},
		Endpoint:     github.Endpoint,
	}

	if githubOauthConfig.ClientID == "" || githubOauthConfig.ClientSecret == "" {
		logrus.Warn("GitHub OAuth credentials are not set. Authentication routes will not work.")
	}
}

func initOIDC() {
	providerURL := os.Getenv("OIDC_ISSUER_URL")
	clientID := os.Getenv("OIDC_CLIENT_ID")
	clientSecret := os.Getenv("OIDC_CLIENT_SECRET")
	redirectURL := os.Getenv("OIDC_REDIRECT_URL")

	if providerURL == "" || clientID == "" || clientSecret == "" {
		logrus.Warn("OIDC credentials are not set. OIDC authentication routes will not work.")
		return
	}

	var err error
	oidcProvider, err = oidc.NewProvider(context.Background(), providerURL)
	if err != nil {
		logrus.Errorf("Failed to create OIDC provider: %s", err.Error())
		return
	}

	oidcOauthConfig = &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		Endpoint:     oidcProvider.Endpoint(),
	}

	logrus.Info("OIDC provider initialized")

	verifier = oidcProvider.Verifier(&oidc.Config{
		ClientID: clientID,
	})
}

// Init function is deprecated, use InitAuth instead
func Init() {
	initGitHub()

	jwtSecret = []byte(os.Getenv("JWT_SECRET"))

	if githubOauthConfig.ClientID == "" || githubOauthConfig.ClientSecret == "" {
		logrus.Warn("GitHub OAuth credentials are not set. Authentication routes will not work.")
	}
	if len(jwtSecret) == 0 {
		logrus.Warn("JWT_SECRET is not set. Authentication routes will not work.")
	}
}

// sessionLifetime matches the JWT's own expiry, so the cookie and the token it
// carries stop being valid at the same moment.
const sessionLifetime = time.Hour * 24 * 7

// SessionCookieName carries the JWT issued after a successful login. The editor
// is upstream's own build and knows nothing about this instance's auth, so the
// session has to be a cookie the browser sends by itself.
const SessionCookieName = "excalidraw_session"

// returnCookieName remembers where the browser was headed before it was sent
// through the login, fragment included.
const returnCookieName = "excalidraw_return"

// rememberReturn stores the path the login was started from. Only a path is
// accepted, so the cookie cannot be used to bounce someone to another site.
func rememberReturn(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("return")
	if target == "" || !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") {
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     returnCookieName,
		Value:    url.QueryEscape(target),
		Path:     "/",
		Expires:  time.Now().Add(10 * time.Minute),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// takeReturn reads back the remembered path and clears it.
func takeReturn(w http.ResponseWriter, r *http.Request) string {
	cookie, err := r.Cookie(returnCookieName)
	if err != nil {
		return "/"
	}

	http.SetCookie(w, &http.Cookie{Name: returnCookieName, Value: "", Path: "/", MaxAge: -1})

	target, err := url.QueryUnescape(cookie.Value)
	if err != nil || !strings.HasPrefix(target, "/") || strings.HasPrefix(target, "//") {
		return "/"
	}

	return target
}

// startSession puts the JWT in a cookie so every later request — including the
// ones the editor makes on its own — carries it.
func startSession(w http.ResponseWriter, r *http.Request, jwtToken string) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    jwtToken,
		Path:     "/",
		Expires:  time.Now().Add(sessionLifetime),
		HttpOnly: true,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteLaxMode,
	})
}

// HandleLogout ends the session. It lands on a page of its own rather than on
// the editor: the editor is behind the login, and the browser still holds a
// GitHub session, so going back there would sign the user straight back in.
func HandleLogout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/auth/signed-out", http.StatusSeeOther)
}

// HandleSignedOut is the page shown after signing out.
func HandleSignedOut(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, signedOutPage)
}

const signedOutPage = `<!doctype html>
<html lang="uk">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Ви вийшли</title>
<style>
  :root { --bg: #f8f9fa; --surface: #fff; --text: #1b1b1f; --muted: #6b6b76; --border: #e4e4eb; --accent: #6965db; }
  @media (prefers-color-scheme: dark) {
    :root { --bg: #121214; --surface: #1c1c21; --text: #ececf1; --muted: #9a9aa6; --border: #2c2c34; --accent: #8b87ff; }
  }
  body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: var(--bg); color: var(--text);
         font: 15px/1.5 system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; padding: 16px; box-sizing: border-box; }
  .card { background: var(--surface); border: 1px solid var(--border); border-radius: 14px; padding: 32px 28px;
          max-width: 360px; width: 100%; text-align: center; }
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
    <h1>Ви вийшли</h1>
    <p>Щоб повернутися до дошок, увійдіть через GitHub.</p>
    <a href="/auth/login">
      <svg viewBox="0 0 16 16" aria-hidden="true"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0016 8c0-4.42-3.58-8-8-8z"/></svg>
      Увійти через GitHub
    </a>
  </div>
</body>
</html>
`

func generateStateOauthCookie(w http.ResponseWriter) string {
	b := make([]byte, 16)
	rand.Read(b)
	state := base64.URLEncoding.EncodeToString(b)
	cookie := &http.Cookie{
		Name:     "oauthstate",
		Value:    state,
		Expires:  time.Now().Add(10 * time.Minute),
		HttpOnly: true,
	}
	http.SetCookie(w, cookie)
	return state
}

func HandleGitHubLogin(w http.ResponseWriter, r *http.Request) {
	if githubOauthConfig.ClientID == "" {
		http.Error(w, "GitHub OAuth is not configured", http.StatusInternalServerError)
		return
	}
	rememberReturn(w, r)
	state := generateStateOauthCookie(w)
	http.Redirect(w, r, githubOauthConfig.AuthCodeURL(state), http.StatusTemporaryRedirect)
}

func HandleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	if githubOauthConfig.ClientID == "" {
		http.Error(w, "GitHub OAuth is not configured", http.StatusInternalServerError)
		return
	}

	token, err := githubOauthConfig.Exchange(context.Background(), r.FormValue("code"))
	if err != nil {
		logrus.Errorf("failed to exchange token: %s", err.Error())
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}

	client := githubOauthConfig.Client(context.Background(), token)
	resp, err := client.Get("https://api.github.com/user")
	if err != nil {
		logrus.Errorf("failed to get user from github: %s", err.Error())
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logrus.Errorf("failed to read github response body: %s", err.Error())
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}

	var githubUser struct {
		ID        int64  `json:"id"`
		Login     string `json:"login"`
		AvatarURL string `json:"avatar_url"`
		Name      string `json:"name"`
	}

	if err := json.Unmarshal(body, &githubUser); err != nil {
		logrus.Errorf("failed to unmarshal github user: %s", err.Error())
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}

	if !IsLoginAllowed(githubUser.Login) {
		logrus.WithField("login", githubUser.Login).Warn("rejected login: not in allowlist")
		http.Redirect(w, r, "/?error=access_denied", http.StatusTemporaryRedirect)
		return
	}

	// Create user object using Subject instead of GitHubID
	user := &core.User{
		Subject:   fmt.Sprintf("github:%d", githubUser.ID),
		Login:     githubUser.Login,
		AvatarURL: githubUser.AvatarURL,
		Name:      githubUser.Name,
	}

	jwtToken, err := createJWT(user)
	if err != nil {
		logrus.Errorf("failed to create JWT: %s", err.Error())
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}

	// The session lives in a cookie only. The token is not put in the URL: it
	// would stay in the browser history, and nothing reads it from there.
	startSession(w, r, jwtToken)
	http.Redirect(w, r, takeReturn(w, r), http.StatusTemporaryRedirect)
}

func HandleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	if oidcOauthConfig == nil {
		http.Error(w, "OIDC is not configured", http.StatusInternalServerError)
		return
	}

	// Generate random state
	stateBytes := make([]byte, 16)
	_, err := rand.Read(stateBytes)
	if err != nil {
		http.Error(w, "Failed to generate state for OIDC login", http.StatusInternalServerError)
		return
	}
	state := hex.EncodeToString(stateBytes)

	// Set state in a cookie
	http.SetCookie(w, &http.Cookie{
		Name:     "oidc_state",
		Value:    state,
		Path:     "/",
		Expires:  time.Now().Add(10 * time.Minute), // 10 minutes expiry
		HttpOnly: true,
		Secure:   r.Header.Get("X-Forwarded-Proto") == "https",
		SameSite: http.SameSiteLaxMode,
	})

	url := oidcOauthConfig.AuthCodeURL(state, oauth2.AccessTypeOffline)
	http.Redirect(w, r, url, http.StatusTemporaryRedirect)
}

func HandleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if oidcOauthConfig == nil {
		http.Error(w, "OIDC is not configured", http.StatusInternalServerError)
		return
	}

	code := r.FormValue("code")
	if code == "" {
		logrus.Error("no code in callback")
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}

	token, err := oidcOauthConfig.Exchange(context.Background(), code)
	if err != nil {
		logrus.Errorf("failed to exchange token: %s", err.Error())
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		logrus.Error("no id_token in token response")
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}

	idToken, err := verifier.Verify(context.Background(), rawIDToken)
	if err != nil {
		logrus.Errorf("failed to verify ID token: %s", err.Error())
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}

	var claims OIDCClaims
	if err := idToken.Claims(&claims); err != nil {
		logrus.Errorf("failed to extract claims from ID token: %s", err.Error())
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}

	// Create user from OIDC claims
	user := &core.User{
		Subject:   claims.Sub,
		Login:     claims.PreferredUsername,
		Email:     claims.Email,
		AvatarURL: claims.Picture,
		Name:      claims.Name,
	}

	// If preferred_username is not available, use email
	if user.Login == "" && user.Email != "" {
		user.Login = user.Email
	}

	if !IsLoginAllowed(user.Login) {
		logrus.WithField("login", user.Login).Warn("rejected login: not in allowlist")
		http.Redirect(w, r, "/?error=access_denied", http.StatusTemporaryRedirect)
		return
	}

	jwtToken, err := createJWT(user)
	if err != nil {
		logrus.Errorf("failed to create JWT: %s", err.Error())
		http.Redirect(w, r, "/", http.StatusTemporaryRedirect)
		return
	}

	// The session lives in a cookie only. The token is not put in the URL: it
	// would stay in the browser history, and nothing reads it from there.
	startSession(w, r, jwtToken)
	http.Redirect(w, r, takeReturn(w, r), http.StatusTemporaryRedirect)
}

func createJWT(user *core.User) (string, error) {
	claims := AppClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   user.Subject,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour * 24 * 7)), // 1 week
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		Login:     user.Login,
		AvatarURL: user.AvatarURL,
		Name:      user.Name,
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(jwtSecret)
}

func ParseJWT(tokenString string) (*AppClaims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &AppClaims{}, func(token *jwt.Token) (interface{}, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}
		return jwtSecret, nil
	})

	if err != nil {
		return nil, err
	}

	if claims, ok := token.Claims.(*AppClaims); ok && token.Valid {
		if !IsLoginAllowed(claims.Login) {
			return nil, fmt.Errorf("login %q is not allowed on this instance", claims.Login)
		}
		return claims, nil
	}

	return nil, fmt.Errorf("invalid token")
}
