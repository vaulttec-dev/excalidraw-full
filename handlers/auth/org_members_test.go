package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeGitHub answers the member list with listed, and the single-account
// check with 204 for concealed and 404 for anyone else; the admin list with
// admins. Setting failUser makes the account lookup fail.
type fakeGitHub struct {
	listed, concealed, admins []int64
	failUser                  bool
}

func (f *fakeGitHub) serve(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.URL.Path == "/orgs/eloicompany/members":
			out := []map[string]any{}
			ids := f.listed
			if r.URL.Query().Get("role") == "admin" {
				ids = f.admins
			}
			for _, id := range ids {
				out = append(out, map[string]any{"id": id, "login": fmt.Sprintf("u%d", id)})
			}
			_ = json.NewEncoder(w).Encode(out)
		case strings.HasPrefix(r.URL.Path, "/user/"):
			if f.failUser {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"login": "u" + strings.TrimPrefix(r.URL.Path, "/user/")})
		case strings.HasPrefix(r.URL.Path, "/orgs/eloicompany/members/u"):
			id := strings.TrimPrefix(r.URL.Path, "/orgs/eloicompany/members/u")
			for _, c := range f.concealed {
				if fmt.Sprint(c) == id {
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	previous := githubAPI
	githubAPI = srv.URL
	t.Cleanup(func() { githubAPI = previous })
}

func withMembers(t *testing.T) *orgMembership {
	t.Helper()
	resetAllowlist(t, "", "eloicompany")
	previous := members
	members = newOrgMembership()
	t.Cleanup(func() { members = previous })
	return members
}

func runSync(t *testing.T, m *orgMembership) ([]string, error) {
	t.Helper()
	return m.sync(context.Background(), http.DefaultClient, "token", []string{"eloicompany"})
}

var orgs = []string{"eloicompany"}

func TestSyncRevokesWhoLeftAndRestoresOnSignIn(t *testing.T) {
	m := withMembers(t)
	(&fakeGitHub{listed: []int64{1}}).serve(t)
	for _, subject := range []string{"github:1", "github:2"} {
		if !IsAccessAllowed(subject, "someone", orgs) {
			t.Fatalf("%s refused before the sync", subject)
		}
	}

	departed, err := runSync(t, m)
	if err != nil || len(departed) != 1 || departed[0] != "github:2" {
		t.Fatalf("sync = %v, %v; want github:2 revoked", departed, err)
	}
	if !IsAccessAllowed("github:1", "someone", orgs) {
		t.Fatal("a member was refused")
	}
	if IsAccessAllowed("github:2", "someone", orgs) {
		t.Fatal("an account that left is still let in")
	}

	// Signing in again as a member, with membership just checked.
	ConfirmMembership("github:2", orgs)
	if !IsAccessAllowed("github:2", "someone", orgs) {
		t.Fatal("a member who rejoined is still refused")
	}
}

func TestSyncKeepsAMemberTheListLeavesOut(t *testing.T) {
	m := withMembers(t)
	(&fakeGitHub{concealed: []int64{3}}).serve(t)
	IsAccessAllowed("github:3", "someone", orgs)

	if departed, err := runSync(t, m); err != nil || len(departed) != 0 {
		t.Fatalf("sync = %v, %v; want nobody revoked", departed, err)
	}
	if !IsAccessAllowed("github:3", "someone", orgs) {
		t.Fatal("a concealed member was refused")
	}
}

func TestSyncRevokesNobodyWhenGitHubFails(t *testing.T) {
	m := withMembers(t)
	(&fakeGitHub{failUser: true}).serve(t)
	IsAccessAllowed("github:4", "someone", orgs)

	if _, err := runSync(t, m); err == nil {
		t.Fatal("sync succeeded although GitHub failed")
	}
	if !IsAccessAllowed("github:4", "someone", orgs) {
		t.Fatal("an account was refused after a failed sync")
	}
}

func TestSyncRevokesNobodyWhenTooManyLeaveAtOnce(t *testing.T) {
	m := withMembers(t)
	(&fakeGitHub{}).serve(t)
	for _, subject := range []string{"github:5", "github:6", "github:7"} {
		IsAccessAllowed(subject, "someone", orgs)
	}

	if _, err := runSync(t, m); err == nil {
		t.Fatal("sync revoked three accounts at once")
	}
	if !IsAccessAllowed("github:5", "someone", orgs) {
		t.Fatal("an account was refused after a refused sync")
	}
}

func TestAdminsComeFromGitHub(t *testing.T) {
	(&fakeGitHub{listed: []int64{1, 2}, admins: []int64{2}}).serve(t)
	t.Cleanup(func() { orgAdmins.logins = map[string]struct{}{} })

	if err := refreshAdmins(context.Background(), http.DefaultClient, "token", orgs); err != nil {
		t.Fatal(err)
	}
	if !IsOrgAdmin("U2") || IsOrgAdmin("u1") || IsOrgAdmin("") {
		t.Fatal("admin list does not match GitHub's")
	}

	// A failed refresh keeps the list it had.
	if err := refreshAdmins(context.Background(), http.DefaultClient, "wrong", orgs); err == nil {
		t.Fatal("refresh succeeded with a rejected token")
	}
	if !IsOrgAdmin("u2") {
		t.Fatal("a failed refresh dropped the admins")
	}
}

func TestListedLoginsAreNotRevoked(t *testing.T) {
	m := withMembers(t)
	resetAllowlist(t, "alice", "eloicompany")
	m.revoked["github:8"] = struct{}{}
	if !IsAccessAllowed("github:8", "alice", orgs) {
		t.Fatal("a login on ALLOWED_GITHUB_LOGINS was refused")
	}
}

func TestSubjectFromHeaders(t *testing.T) {
	jwtSecret = []byte("test-secret")
	resetAllowlist(t, "alice", "")
	withSubject := AppClaims{Login: "alice"}
	withSubject.Subject = "github:9"
	token := signed(t, withSubject)
	headers := map[string][]string{"cookie": {SessionCookieName + "=" + token}}
	if got := SubjectFromHeaders(headers); got != "github:9" {
		t.Fatalf("SubjectFromHeaders = %q, want github:9", got)
	}
	if got := SubjectFromHeaders(map[string][]string{}); got != "" {
		t.Fatalf("SubjectFromHeaders without credentials = %q", got)
	}
}
