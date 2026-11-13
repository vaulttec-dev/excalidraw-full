package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// Organisation membership is asked of GitHub at sign-in, but a session lasts a
// week and an MCP refresh token a month. With GITHUB_ORG_MEMBERS_TOKEN set —
// a fine-grained token owned by the organisation with Members: Read-only — a
// sync every few minutes finds the accounts that have left every allowed
// organisation, and IsAccessAllowed refuses them from then on.
//
// It only revokes what GitHub confirms: an account the member list leaves out
// is checked one by one, any answer GitHub doesn't document ends the run with
// nobody revoked, and so does a run that would revoke more than a couple of
// accounts at once, which looks like the token losing sight of the members
// rather than people leaving. The state is in memory: after a restart an
// account that left is refused within one sync of its next request.

const orgMembersTokenEnv = "GITHUB_ORG_MEMBERS_TOKEN"

const (
	membersSyncInterval   = 5 * time.Minute
	maxRevocationsPerSync = 2
	// The longest an account can go on without signing in again: an MCP
	// refresh token's lifetime. Accounts unseen for longer are forgotten.
	seenFor = 30 * 24 * time.Hour
)

// githubAPI is GitHub's REST API; tests point it at a fake.
var githubAPI = "https://api.github.com"

type orgMembership struct {
	mu sync.Mutex
	// seen holds the subjects let in through an organisation, and when.
	seen map[string]time.Time
	// revoked holds the subjects GitHub confirmed have left.
	revoked map[string]struct{}
}

var members = newOrgMembership()

func newOrgMembership() *orgMembership {
	return &orgMembership{seen: map[string]time.Time{}, revoked: map[string]struct{}{}}
}

// admit reports whether an account let in through an organisation still may
// be, and notes it for the next sync.
func (m *orgMembership) admit(subject string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, gone := m.revoked[subject]; gone {
		return false
	}
	if subject != "" {
		m.seen[subject] = time.Now()
	}
	return true
}

// confirm lifts a revocation once GitHub has just shown the account to be a
// member again, at sign-in or with its own token.
func (m *orgMembership) confirm(subject string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.revoked, subject)
}

// sync asks GitHub about every account seen lately and revokes those that have
// left every allowed organisation. It returns the subjects it revoked.
func (m *orgMembership) sync(ctx context.Context, client *http.Client, token string, orgs []string) ([]string, error) {
	listed := map[string]bool{}
	for _, org := range orgs {
		ids, err := listMemberIDs(ctx, client, token, org)
		if err != nil {
			return nil, err
		}
		for _, id := range ids {
			listed[fmt.Sprintf("github:%d", id)] = true
		}
	}

	m.mu.Lock()
	cutoff := time.Now().Add(-seenFor)
	var candidates []string
	for subject, at := range m.seen {
		if at.Before(cutoff) {
			delete(m.seen, subject)
			continue
		}
		if _, gone := m.revoked[subject]; !gone && !listed[subject] {
			candidates = append(candidates, subject)
		}
	}
	for subject := range m.revoked {
		if listed[subject] {
			delete(m.revoked, subject)
		}
	}
	m.mu.Unlock()

	var departed []string
	for _, subject := range candidates {
		id, ok := strings.CutPrefix(subject, "github:")
		if !ok {
			continue
		}
		stillMember := false
		for _, org := range orgs {
			member, err := isOrgMember(ctx, client, token, org, id)
			if err != nil {
				return nil, err
			}
			if member {
				stillMember = true
				break
			}
		}
		if !stillMember {
			departed = append(departed, subject)
		}
	}

	if len(departed) > maxRevocationsPerSync {
		return nil, fmt.Errorf("%d accounts seem to have left %v at once, more than %d: nobody revoked, check %s",
			len(departed), orgs, maxRevocationsPerSync, orgMembersTokenEnv)
	}

	m.mu.Lock()
	for _, subject := range departed {
		m.revoked[subject] = struct{}{}
	}
	m.mu.Unlock()
	return departed, nil
}

// StartMembershipSync runs the sync in the background when
// GITHUB_ORG_MEMBERS_TOKEN is set. onRevoked is called with the subject of
// every account revoked, to close what is already open, such as sockets.
func StartMembershipSync(onRevoked func(subject string)) {
	token := strings.TrimSpace(os.Getenv(orgMembersTokenEnv))
	orgs := AllowedOrgs()
	if len(orgs) == 0 {
		return
	}
	if token == "" {
		logrus.Warnf("%s is not set: organisation membership is only checked at sign-in", orgMembersTokenEnv)
		return
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
		// GitHub redirects the membership check of a requester outside the
		// organisation; that answer is read, not followed.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	refresh := func() {
		if err := refreshAdmins(context.Background(), client, token, AllowedOrgs()); err != nil {
			logrus.WithError(err).Error("organisation admin list not refreshed: keeping the previous one")
		}
	}
	go func() {
		// Admins decide who may delete others' boards, so they are known from
		// the start rather than after the first interval.
		refresh()
		ticker := time.NewTicker(membersSyncInterval)
		defer ticker.Stop()
		for range ticker.C {
			refresh()
			departed, err := members.sync(context.Background(), client, token, AllowedOrgs())
			if err != nil {
				logrus.WithError(err).Error("organisation membership sync failed: nobody was signed out")
				continue
			}
			for _, subject := range departed {
				logrus.WithField("subject", subject).Warn("signed out an account that left the organisation")
				if onRevoked != nil {
					onRevoked(subject)
				}
			}
		}
	}()
}

func githubGet(ctx context.Context, client *http.Client, token, endpoint string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return client.Do(req)
}

// listMemberIDs lists the organisation's members, private memberships included
// as long as the token's owner is a member.
// https://docs.github.com/en/rest/orgs/members#list-organization-members
func listMemberIDs(ctx context.Context, client *http.Client, token, org string) ([]int64, error) {
	var ids []int64
	for page := 1; ; page++ {
		endpoint := fmt.Sprintf("%s/orgs/%s/members?per_page=100&page=%d", githubAPI, url.PathEscape(org), page)
		resp, err := githubGet(ctx, client, token, endpoint)
		if err != nil {
			return nil, err
		}
		var batch []struct {
			ID int64 `json:"id"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&batch)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("GitHub answered %d listing the members of %s", resp.StatusCode, org)
		}
		if decodeErr != nil {
			return nil, decodeErr
		}
		for _, member := range batch {
			ids = append(ids, member.ID)
		}
		if len(batch) < 100 {
			return ids, nil
		}
	}
}

// isOrgMember asks about one account the member list left out: the list alone
// can't tell a departure from a membership the token doesn't see. It returns
// false only when GitHub says the account is not a member or no longer exists.
func isOrgMember(ctx context.Context, client *http.Client, token, org, githubID string) (bool, error) {
	// https://docs.github.com/en/rest/users/users#get-a-user-using-their-id
	resp, err := githubGet(ctx, client, token, githubAPI+"/user/"+url.PathEscape(githubID))
	if err != nil {
		return false, err
	}
	var user struct {
		Login string `json:"login"`
	}
	decodeErr := json.NewDecoder(resp.Body).Decode(&user)
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return false, nil
	case resp.StatusCode != http.StatusOK:
		return false, fmt.Errorf("GitHub answered %d looking up account %s", resp.StatusCode, githubID)
	case decodeErr != nil || user.Login == "":
		return false, fmt.Errorf("unreadable GitHub account %s: %v", githubID, decodeErr)
	}

	// https://docs.github.com/en/rest/orgs/members#check-organization-membership-for-a-user
	// 204 a member, 404 not one; 302 means the token's owner isn't a member
	// and so can't tell, which fails like any other answer.
	resp, err = githubGet(ctx, client, token,
		fmt.Sprintf("%s/orgs/%s/members/%s", githubAPI, url.PathEscape(org), url.PathEscape(user.Login)))
	if err != nil {
		return false, err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNoContent:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("GitHub answered %d checking whether %s is in %s", resp.StatusCode, user.Login, org)
	}
}

// SubjectFromHeaders identifies the account behind a socket.io handshake, by
// its session cookie or bearer token, so that its sockets can be closed when
// it is revoked. It returns "" when there is none.
func SubjectFromHeaders(headers map[string][]string) string {
	h := http.Header{}
	for key, values := range headers {
		for _, value := range values {
			h.Add(key, value)
		}
	}
	if cookie, err := (&http.Request{Header: h}).Cookie(SessionCookieName); err == nil {
		if claims, err := ParseJWT(cookie.Value); err == nil {
			return claims.Subject
		}
	}
	if header := h.Get("Authorization"); header != "" {
		if claims := ClaimsFromBearer(context.Background(), header); claims != nil {
			return claims.Subject
		}
	}
	return ""
}
