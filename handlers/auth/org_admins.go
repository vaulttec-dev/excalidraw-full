package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// orgAdmins holds the logins of the administrators of ALLOWED_GITHUB_ORGS,
// refreshed with the membership sync. Until the first refresh succeeds — or
// without GITHUB_ORG_MEMBERS_TOKEN — nobody counts as an administrator.
var orgAdmins = struct {
	mu     sync.RWMutex
	logins map[string]struct{}
}{logins: map[string]struct{}{}}

// IsOrgAdmin tells whether login administers one of the allowed organisations.
func IsOrgAdmin(login string) bool {
	if login == "" {
		return false
	}
	orgAdmins.mu.RLock()
	defer orgAdmins.mu.RUnlock()
	_, ok := orgAdmins.logins[strings.ToLower(login)]
	return ok
}

// refreshAdmins replaces the administrator list. On any failure the previous
// list is kept, so a GitHub outage neither grants nor takes away anything.
func refreshAdmins(ctx context.Context, client *http.Client, token string, orgs []string) error {
	next := map[string]struct{}{}
	for _, org := range orgs {
		logins, err := listAdminLogins(ctx, client, token, org)
		if err != nil {
			return err
		}
		for _, login := range logins {
			next[strings.ToLower(login)] = struct{}{}
		}
	}
	orgAdmins.mu.Lock()
	orgAdmins.logins = next
	orgAdmins.mu.Unlock()
	return nil
}

// listAdminLogins lists the organisation's members with the admin role.
// https://docs.github.com/en/rest/orgs/members#list-organization-members
func listAdminLogins(ctx context.Context, client *http.Client, token, org string) ([]string, error) {
	var logins []string
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("%s/orgs/%s/members?role=admin&per_page=100&page=%d", githubAPI, url.PathEscape(org), page)
		resp, err := githubGet(ctx, client, token, endpoint)
		if err != nil {
			return nil, err
		}
		var batch []struct {
			Login string `json:"login"`
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("GitHub answered %d listing the admins of %s", resp.StatusCode, org)
		}
		err = json.NewDecoder(resp.Body).Decode(&batch)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, member := range batch {
			logins = append(logins, member.Login)
		}
		if len(batch) < 100 {
			return logins, nil
		}
	}
}
