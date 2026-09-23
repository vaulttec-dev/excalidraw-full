package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"os"
	"strings"
	"testing"

	"excalidraw-complete/handlers/auth"
)

func TestMain(m *testing.M) {
	// Client ids are signed with the instance's key.
	os.Setenv("JWT_SECRET", "test-secret")
	auth.InitAuth()
	os.Exit(m.Run())
}

func TestClientIDIsSigned(t *testing.T) {
	id := signClient(client{RedirectURIs: []string{"http://127.0.0.1:3000/callback"}, Name: "Claude"})

	c, ok := parseClient(id)
	if !ok || c.Name != "Claude" || !c.allows("http://127.0.0.1:3000/callback") {
		t.Fatalf("own client id not accepted: %+v %v", c, ok)
	}
	if c.allows("http://127.0.0.1:3000/other") {
		t.Fatal("client allows a redirect it did not register")
	}

	payload, mac, _ := strings.Cut(id, ".")
	forged := base64.RawURLEncoding.EncodeToString([]byte(`{"r":["https://evil.example/cb"]}`))
	for _, bad := range []string{forged + "." + mac, payload + ".x" + mac, payload, ""} {
		if _, ok := parseClient(bad); ok {
			t.Fatalf("accepted forged client id %q", bad)
		}
	}
}

func TestValidRedirect(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://claude.ai/api/mcp/auth_callback": true,
		"http://localhost:33418/callback":         true,
		"http://127.0.0.1/cb":                     true,
		"http://[::1]:8080/cb":                    true,
		"http://example.com/cb":                   false,
		"https://example.com/cb#fragment":         false,
		"javascript:alert(1)":                     false,
		"https:///no-host":                        false,
	} {
		if got := validRedirect(raw); got != want {
			t.Errorf("validRedirect(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestPKCE(t *testing.T) {
	verifier := "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	if !pkceMatches(verifier, challenge) {
		t.Fatal("matching verifier rejected")
	}
	if pkceMatches("another-verifier", challenge) || pkceMatches(verifier, "") {
		t.Fatal("wrong verifier accepted")
	}
}
