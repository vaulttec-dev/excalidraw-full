package boards

import (
	"net/http"
	"os"
	"strings"

	"excalidraw-complete/handlers/auth"
	"excalidraw-complete/middleware"
)

// mayDelete tells whether the request's account may delete a board created by
// createdBy: its author, or an administrator of an allowed GitHub
// organisation. Anybody may edit any board; deleting is narrower because it
// takes the board away from the whole team.
func mayDelete(r *http.Request, createdBy string) bool {
	// Sign-in off: an open local instance has no accounts to tell apart.
	if os.Getenv("GITHUB_CLIENT_ID") == "" {
		return true
	}
	claims, ok := r.Context().Value(middleware.ClaimsContextKey).(*auth.AppClaims)
	if !ok || claims == nil || claims.Login == "" {
		return false
	}
	return strings.EqualFold(claims.Login, createdBy) || isOrgAdmin(claims.Login)
}

// isOrgAdmin is replaced in tests.
var isOrgAdmin = auth.IsOrgAdmin

func forbidDelete(w http.ResponseWriter) {
	http.Error(w, "only the board's author or an organisation admin can delete it", http.StatusForbidden)
}
