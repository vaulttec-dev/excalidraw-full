package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"golang.org/x/oauth2"
)

// Verified tokens are remembered for a while so a busy client does not cost a
// GitHub round trip per request; refusals for less, so a token that was just
// granted access starts working soon.
const (
	githubTokenAllowTTL = 10 * time.Minute
	githubTokenDenyTTL  = time.Minute
)

type githubTokenVerdict struct {
	claims  *AppClaims
	expires time.Time
}

var githubTokens sync.Map // sha256(token) → githubTokenVerdict

// VerifyGitHubToken signs in a client that has no browser — the MCP server —
// with a person's own GitHub token. The token is checked with GitHub and held
// to the same rules as a browser login: a login on ALLOWED_GITHUB_LOGINS or
// membership in one of ALLOWED_GITHUB_ORGS.
func VerifyGitHubToken(ctx context.Context, token string) (*AppClaims, bool) {
	sum := sha256.Sum256([]byte(token))
	key := hex.EncodeToString(sum[:])
	if cached, ok := githubTokens.Load(key); ok {
		verdict := cached.(githubTokenVerdict)
		if time.Now().Before(verdict.expires) {
			// Checked against the current configuration each time, so taking
			// a login or an organisation off the lists applies at once.
			if verdict.claims != nil && IsAccessAllowed(verdict.claims.Login, verdict.claims.Orgs) {
				return verdict.claims, true
			}
			return nil, false
		}
	}

	claims, err := lookUpGitHubToken(ctx, token)
	if err != nil || !IsAccessAllowed(claims.Login, claims.Orgs) {
		if err != nil {
			logrus.WithError(err).Debug("GitHub token rejected")
		} else {
			logrus.WithField("login", claims.Login).Warn("rejected token: not in the login allowlist or an allowed organisation")
		}
		githubTokens.Store(key, githubTokenVerdict{expires: time.Now().Add(githubTokenDenyTTL)})
		return nil, false
	}

	githubTokens.Store(key, githubTokenVerdict{claims: claims, expires: time.Now().Add(githubTokenAllowTTL)})
	return claims, true
}

func lookUpGitHubToken(ctx context.Context, token string) (*AppClaims, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client := oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token}))

	resp, err := client.Get("https://api.github.com/user")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github answered %d", resp.StatusCode)
	}

	var user struct {
		ID        int64  `json:"id"`
		Login     string `json:"login"`
		AvatarURL string `json:"avatar_url"`
		Name      string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, err
	}

	claims := &AppClaims{
		Login:     user.Login,
		AvatarURL: user.AvatarURL,
		Name:      user.Name,
		Orgs:      githubMemberships(client, user.Login),
	}
	claims.Subject = fmt.Sprintf("github:%d", user.ID)
	return claims, nil
}
