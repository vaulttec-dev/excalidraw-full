// Package oauth lets MCP clients — Claude Code, claude.ai — sign in to the
// instance the way the MCP authorization spec describes, with the GitHub login
// the editor already uses behind it.
//
// The instance is its own authorization server: clients register themselves
// (dynamic client registration), send the person through /oauth/authorize,
// where they sign in with GitHub and approve the client, and exchange the code
// for an access token with PKCE. Access tokens are ordinary session tokens, so
// the login gate accepts them as it does the browser's cookie, and they are
// held to the same allowlist.
//
// Nothing is stored: client ids, codes and refresh tokens are all signed with
// the instance's key, so a restart loses nothing.
package oauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"excalidraw-complete/handlers/auth"
	"excalidraw-complete/middleware"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/sirupsen/logrus"
)

const (
	codeTTL    = 5 * time.Minute
	accessTTL  = time.Hour
	refreshTTL = 30 * 24 * time.Hour
	scope      = "boards"
)

// ---------------------------------------------------------------------------
// Metadata

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}

// HandleProtectedResource tells an MCP client which authorization server
// guards /mcp (RFC 9728).
func HandleProtectedResource(w http.ResponseWriter, r *http.Request) {
	base := middleware.BaseURL(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":                 base + "/mcp",
		"authorization_servers":    []string{base},
		"bearer_methods_supported": []string{"header"},
		"scopes_supported":         []string{scope},
		"resource_name":            "Excalidraw команди",
	})
}

// HandleAuthorizationServer describes this instance as an authorization
// server (RFC 8414).
func HandleAuthorizationServer(w http.ResponseWriter, r *http.Request) {
	base := middleware.BaseURL(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/oauth/authorize",
		"token_endpoint":                        base + "/oauth/token",
		"registration_endpoint":                 base + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{scope},
	})
}

// ---------------------------------------------------------------------------
// Clients

// client is what a registered client id carries. The id is the client itself,
// signed, so registrations need no storage and survive restarts.
type client struct {
	RedirectURIs []string `json:"r"`
	Name         string   `json:"n,omitempty"`
}

func clientMAC(payload string) string {
	mac := hmac.New(sha256.New, append([]byte("oauth-client:"), auth.SigningKey()...))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func signClient(c client) string {
	data, _ := json.Marshal(c)
	payload := base64.RawURLEncoding.EncodeToString(data)
	return payload + "." + clientMAC(payload)
}

func parseClient(id string) (client, bool) {
	payload, mac, ok := strings.Cut(id, ".")
	if !ok || !hmac.Equal([]byte(mac), []byte(clientMAC(payload))) {
		return client{}, false
	}
	data, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return client{}, false
	}
	var c client
	if json.Unmarshal(data, &c) != nil || len(c.RedirectURIs) == 0 {
		return client{}, false
	}
	return c, true
}

// validRedirect admits https callbacks and loopback ones, which is what
// desktop clients such as Claude Code use.
func validRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" {
		return false
	}
	if u.Scheme == "https" && u.Host != "" {
		return true
	}
	host := u.Hostname()
	return u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")
}

func (c client) allows(redirect string) bool {
	for _, r := range c.RedirectURIs {
		if r == redirect {
			return true
		}
	}
	return false
}

// HandleRegister is dynamic client registration (RFC 7591).
func HandleRegister(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.RedirectURIs) == 0 {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "redirect_uris is required")
		return
	}
	for _, redirect := range body.RedirectURIs {
		if !validRedirect(redirect) {
			oauthError(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect URIs must be https or loopback: "+redirect)
			return
		}
	}
	name := strings.TrimSpace(body.ClientName)
	if len([]rune(name)) > 80 {
		name = string([]rune(name)[:80])
	}

	c := client{RedirectURIs: body.RedirectURIs, Name: name}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  signClient(c),
		"client_id_issued_at":        time.Now().Unix(),
		"client_name":                name,
		"redirect_uris":              c.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

// ---------------------------------------------------------------------------
// Signed codes and refresh tokens

type grantClaims struct {
	auth.AppClaims
	ClientID      string `json:"cid"`
	RedirectURI   string `json:"ru,omitempty"`
	CodeChallenge string `json:"cc,omitempty"`
}

func signGrant(g grantClaims) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, g).SignedString(auth.SigningKey())
}

func parseGrant(raw, use string) (*grantClaims, error) {
	token, err := jwt.ParseWithClaims(raw, &grantClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method")
		}
		return auth.SigningKey(), nil
	})
	if err != nil {
		return nil, err
	}
	g, ok := token.Claims.(*grantClaims)
	if !ok || !token.Valid || g.Use != use {
		return nil, fmt.Errorf("not a %s", use)
	}
	return g, nil
}

// usedCodes makes codes single-use for as long as they live.
var usedCodes sync.Map // code id → expiry

func spendCode(id string, expires time.Time) bool {
	now := time.Now()
	usedCodes.Range(func(k, v any) bool {
		if now.After(v.(time.Time)) {
			usedCodes.Delete(k)
		}
		return true
	})
	_, spent := usedCodes.LoadOrStore(id, expires)
	return !spent
}

func person(claims *auth.AppClaims) auth.AppClaims {
	return auth.AppClaims{
		RegisteredClaims: jwt.RegisteredClaims{Subject: claims.Subject},
		Login:            claims.Login,
		Name:             claims.Name,
		AvatarURL:        claims.AvatarURL,
		Orgs:             claims.Orgs,
	}
}

// ---------------------------------------------------------------------------
// Authorization

type authorizeRequest struct {
	client        client
	ClientID      string
	RedirectURI   string
	State         string
	CodeChallenge string
}

func readAuthorize(r *http.Request) (*authorizeRequest, string) {
	values := r.URL.Query()
	if r.Method == http.MethodPost {
		_ = r.ParseForm()
		values = r.PostForm
	}
	c, ok := parseClient(values.Get("client_id"))
	if !ok {
		return nil, "unknown client"
	}
	redirect := values.Get("redirect_uri")
	if redirect == "" && len(c.RedirectURIs) == 1 {
		redirect = c.RedirectURIs[0]
	}
	if !c.allows(redirect) {
		return nil, "redirect_uri is not registered for this client"
	}
	req := &authorizeRequest{
		client:        c,
		ClientID:      values.Get("client_id"),
		RedirectURI:   redirect,
		State:         values.Get("state"),
		CodeChallenge: values.Get("code_challenge"),
	}
	if values.Get("response_type") != "code" {
		return req, "unsupported_response_type"
	}
	if req.CodeChallenge == "" || values.Get("code_challenge_method") != "S256" {
		return req, "PKCE with S256 is required"
	}
	return req, ""
}

func redirectWith(w http.ResponseWriter, r *http.Request, req *authorizeRequest, params url.Values) {
	target, _ := url.Parse(req.RedirectURI)
	q := target.Query()
	for k, v := range params {
		q[k] = v
	}
	if req.State != "" {
		q.Set("state", req.State)
	}
	target.RawQuery = q.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

// HandleAuthorize signs the person in with GitHub, if needed, and asks them to
// approve the client before a code is issued. Approval matters: clients
// register themselves, so without it anyone could register a client and hand
// a signed-in teammate a link that silently gives the client their access.
func HandleAuthorize(w http.ResponseWriter, r *http.Request) {
	req, problem := readAuthorize(r)
	if req == nil {
		// Without a trusted redirect the error can only be shown here.
		http.Error(w, problem, http.StatusBadRequest)
		return
	}
	if problem != "" {
		redirectWith(w, r, req, url.Values{"error": {"invalid_request"}, "error_description": {problem}})
		return
	}

	cookie, err := r.Cookie(auth.SessionCookieName)
	var claims *auth.AppClaims
	if err == nil {
		claims, err = auth.ParseJWT(cookie.Value)
	}
	if err != nil || claims == nil {
		if r.Method != http.MethodGet {
			http.Error(w, "session expired, start again", http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, "/auth/login?return="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
		return
	}

	if r.Method == http.MethodGet {
		renderConsent(w, req, claims)
		return
	}

	// POST: the consent form. The session cookie is SameSite=Lax, so a form
	// posted from another site arrives without it and fails above.
	if r.PostForm.Get("decision") != "allow" {
		redirectWith(w, r, req, url.Values{"error": {"access_denied"}})
		return
	}

	g := grantClaims{
		AppClaims:     person(claims),
		ClientID:      req.ClientID,
		RedirectURI:   req.RedirectURI,
		CodeChallenge: req.CodeChallenge,
	}
	g.Use = "code"
	g.ID = base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
	g.ExpiresAt = jwt.NewNumericDate(time.Now().Add(codeTTL))
	code, err := signGrant(g)
	if err != nil {
		http.Error(w, "failed to issue code", http.StatusInternalServerError)
		return
	}
	logrus.WithFields(logrus.Fields{"login": claims.Login, "client": req.client.Name}).Info("MCP client authorized")
	redirectWith(w, r, req, url.Values{"code": {code}})
}

var consentPage = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="uk">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Дозволити доступ</title>
<style>
  :root { --bg: #f8f9fa; --surface: #fff; --text: #1b1b1f; --muted: #6b6b76; --border: #e4e4eb; --accent: #6965db; }
  @media (prefers-color-scheme: dark) {
    :root { --bg: #121214; --surface: #1c1c21; --text: #ececf1; --muted: #9a9aa6; --border: #2c2c34; --accent: #8b87ff; }
  }
  body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: var(--bg); color: var(--text);
         font: 15px/1.5 system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; padding: 16px; box-sizing: border-box; }
  .card { background: var(--surface); border: 1px solid var(--border); border-radius: 14px; padding: 28px;
          max-width: 400px; width: 100%; }
  h1 { font-size: 19px; margin: 0 0 12px; }
  p { color: var(--muted); margin: 0 0 12px; }
  strong { color: var(--text); }
  code { font-size: 13px; word-break: break-all; }
  .actions { display: flex; gap: 8px; justify-content: flex-end; margin-top: 20px; }
  button { font: inherit; font-weight: 600; border-radius: 10px; padding: 9px 16px; cursor: pointer;
           border: 1px solid var(--border); background: var(--surface); color: var(--text); }
  button.allow { background: var(--accent); border-color: var(--accent); color: #fff; }
</style>
</head>
<body>
  <form class="card" method="post" action="/oauth/authorize">
    <h1>Дозволити доступ до дошок?</h1>
    <p><strong>{{.ClientName}}</strong> отримає доступ до дошок команди від імені <strong>{{.Login}}</strong>: зможе читати, створювати й змінювати їх.</p>
    <p>Після дозволу вас поверне сюди: <code>{{.RedirectHost}}</code></p>
    <input type="hidden" name="client_id" value="{{.ClientID}}">
    <input type="hidden" name="redirect_uri" value="{{.RedirectURI}}">
    <input type="hidden" name="state" value="{{.State}}">
    <input type="hidden" name="code_challenge" value="{{.CodeChallenge}}">
    <input type="hidden" name="code_challenge_method" value="S256">
    <input type="hidden" name="response_type" value="code">
    <div class="actions">
      <button type="submit" name="decision" value="deny">Скасувати</button>
      <button type="submit" name="decision" value="allow" class="allow">Дозволити</button>
    </div>
  </form>
</body>
</html>
`))

func renderConsent(w http.ResponseWriter, req *authorizeRequest, claims *auth.AppClaims) {
	name := req.client.Name
	if name == "" {
		name = "MCP-клієнт"
	}
	redirectHost := req.RedirectURI
	if u, err := url.Parse(req.RedirectURI); err == nil {
		redirectHost = u.Scheme + "://" + u.Host
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// The page must not be framed: a framed consent button can be clickjacked.
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	_ = consentPage.Execute(w, map[string]string{
		"ClientName":    name,
		"Login":         claims.Login,
		"RedirectHost":  redirectHost,
		"ClientID":      req.ClientID,
		"RedirectURI":   req.RedirectURI,
		"State":         req.State,
		"CodeChallenge": req.CodeChallenge,
	})
}

// ---------------------------------------------------------------------------
// Tokens

func issueTokens(w http.ResponseWriter, who auth.AppClaims, clientID string) {
	access := who
	access.IssuedAt = jwt.NewNumericDate(time.Now())
	access.ExpiresAt = jwt.NewNumericDate(time.Now().Add(accessTTL))
	accessToken, err := auth.SignClaims(access)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "failed to sign token")
		return
	}

	refresh := grantClaims{AppClaims: who, ClientID: clientID}
	refresh.Use = "refresh"
	refresh.IssuedAt = jwt.NewNumericDate(time.Now())
	refresh.ExpiresAt = jwt.NewNumericDate(time.Now().Add(refreshTTL))
	refreshToken, err := signGrant(refresh)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "failed to sign token")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  accessToken,
		"token_type":    "Bearer",
		"expires_in":    int(accessTTL.Seconds()),
		"refresh_token": refreshToken,
		"scope":         scope,
	})
}

func pkceMatches(verifier, challenge string) bool {
	sum := sha256.Sum256([]byte(verifier))
	expected := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(expected), []byte(challenge)) == 1
}

// HandleToken exchanges a code, or a refresh token, for tokens.
func HandleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "malformed form")
		return
	}
	form := r.PostForm
	clientID := form.Get("client_id")
	if _, ok := parseClient(clientID); !ok {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "unknown client")
		return
	}

	switch form.Get("grant_type") {
	case "authorization_code":
		g, err := parseGrant(form.Get("code"), "code")
		if err != nil || g.ClientID != clientID ||
			(form.Get("redirect_uri") != "" && form.Get("redirect_uri") != g.RedirectURI) {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired code")
			return
		}
		if !pkceMatches(form.Get("code_verifier"), g.CodeChallenge) {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier does not match")
			return
		}
		if !spendCode(g.ID, g.ExpiresAt.Time) {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "code already used")
			return
		}
		issueTokens(w, person(&g.AppClaims), clientID)

	case "refresh_token":
		g, err := parseGrant(form.Get("refresh_token"), "refresh")
		if err != nil || g.ClientID != clientID {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "invalid or expired refresh token")
			return
		}
		// Refreshing is where a removed person loses access for good.
		if !auth.IsAccessAllowed(g.Login, g.Orgs) {
			oauthError(w, http.StatusBadRequest, "invalid_grant", "access has been revoked")
			return
		}
		issueTokens(w, person(&g.AppClaims), clientID)

	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "use authorization_code or refresh_token")
	}
}
