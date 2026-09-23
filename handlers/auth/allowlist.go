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

	orgs := AllowedOrgs()
	if len(allowlist) == 0 && len(orgs) == 0 {
		logrus.Warnf("%s and %s are empty: nobody may sign in", allowedLoginsEnv, allowedOrgsEnv)
		return
	}
	logrus.WithFields(logrus.Fields{"logins": len(allowlist), "orgs": orgs}).Info("Login allowlist enabled")
}

const allowedOrgsEnv = "ALLOWED_GITHUB_ORGS"

// AllowedOrgs lists the GitHub organisations whose active members may sign in,
// from the comma separated ALLOWED_GITHUB_ORGS. It spares adding every
// teammate by hand: joining the organisation is enough.
func AllowedOrgs() []string {
	var orgs []string
	for _, entry := range strings.Split(os.Getenv(allowedOrgsEnv), ",") {
		if org := strings.ToLower(strings.TrimSpace(entry)); org != "" {
			orgs = append(orgs, org)
		}
	}
	return orgs
}

// IsAccessAllowed reports whether an account may use this instance, either by
// its login or through one of the organisations it was found to belong to when
// it signed in. Organisations are checked against the current configuration,
// so dropping one from ALLOWED_GITHUB_ORGS takes effect at once; someone who
// leaves an allowed organisation keeps access until their session expires,
// since membership is only asked of GitHub at sign-in.
func IsAccessAllowed(login string, orgs []string) bool {
	if IsLoginAllowed(login) {
		return true
	}
	for _, allowed := range AllowedOrgs() {
		for _, org := range orgs {
			if strings.EqualFold(org, allowed) {
				return true
			}
		}
	}
	return false
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
