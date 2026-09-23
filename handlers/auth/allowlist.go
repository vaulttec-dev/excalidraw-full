package auth

import (
	"os"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
)

const allowedLoginsEnv = "ALLOWED_GITHUB_LOGINS"

// allowEveryone is the explicit value that opens the instance to every account
// the provider can authenticate.
const allowEveryone = "*"

var (
	allowlistOnce sync.Once
	allowlist     map[string]struct{}
	allowAll      bool
)

// loadAllowlist reads ALLOWED_GITHUB_LOGINS once and caches it.
// The variable holds a comma separated list of logins, for example:
//
//	ALLOWED_GITHUB_LOGINS=vaulttec-dev,some-teammate
//
// An empty or missing value lets nobody in. It used to let everybody in, which
// turned clearing the list — the natural way to revoke access — into opening
// the instance to every GitHub account. Opening it up now takes "*".
func loadAllowlist() {
	raw := strings.TrimSpace(os.Getenv(allowedLoginsEnv))
	if raw == allowEveryone {
		allowAll = true
		logrus.Warnf("%s is %q: every authenticated account may sign in", allowedLoginsEnv, allowEveryone)
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

	if len(allowlist) == 0 {
		logrus.Warnf("%s is empty: nobody may sign in", allowedLoginsEnv)
		return
	}
	logrus.WithField("count", len(allowlist)).Info("Login allowlist enabled")
}

// IsLoginAllowed reports whether the given account may use this instance.
// It is enforced both when a session is issued and on every request that
// carries a token, so removing a login from the allowlist takes effect
// immediately instead of when the token expires.
func IsLoginAllowed(login string) bool {
	allowlistOnce.Do(loadAllowlist)

	if allowAll {
		return true
	}

	_, ok := allowlist[strings.ToLower(strings.TrimSpace(login))]
	return ok
}
