package auth

import (
	"os"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
)

const allowedLoginsEnv = "ALLOWED_GITHUB_LOGINS"

var (
	allowlistOnce sync.Once
	allowlist     map[string]struct{}
)

// loadAllowlist reads ALLOWED_GITHUB_LOGINS once and caches it.
// The variable holds a comma separated list of logins, for example:
//
//	ALLOWED_GITHUB_LOGINS=vaulttec-dev,some-teammate
//
// An empty or missing value keeps the instance open to every account that can
// authenticate with the configured provider.
func loadAllowlist() {
	raw := strings.TrimSpace(os.Getenv(allowedLoginsEnv))
	if raw == "" {
		logrus.Warnf("%s is not set: every authenticated account may sign in", allowedLoginsEnv)
		return
	}

	allowlist = make(map[string]struct{})
	for _, entry := range strings.Split(raw, ",") {
		login := strings.ToLower(strings.TrimSpace(entry))
		if login == "" {
			continue
		}
		allowlist[login] = struct{}{}
	}

	logrus.WithField("count", len(allowlist)).Info("Login allowlist enabled")
}

// IsLoginAllowed reports whether the given account may use this instance.
// It is enforced both when a session is issued and on every request that
// carries a token, so removing a login from the allowlist takes effect
// immediately instead of when the token expires.
func IsLoginAllowed(login string) bool {
	allowlistOnce.Do(loadAllowlist)

	if allowlist == nil {
		return true
	}

	_, ok := allowlist[strings.ToLower(strings.TrimSpace(login))]
	return ok
}
