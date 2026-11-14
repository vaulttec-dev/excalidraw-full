package boards

import (
	"context"
	"net/http"
	"os"
	"strings"

	"excalidraw-complete/core"
	"excalidraw-complete/handlers/auth"
	"excalidraw-complete/middleware"
)

// Who may do what.
//
// Boards and folders both have an author and an access: what everybody else
// may do, look only ("view", the default) or change and delete too ("edit").
// The administrators of an allowed GitHub organisation may do anything, and so
// may a folder's author with the boards in it. Anybody else may change a board
// when both the board and its folder allow them. Setting the access, and
// renaming or deleting a folder, stays with the author and the administrators.

const (
	accessView = "view"
	accessEdit = "edit"
)

// account is who a request, a socket or an MCP call acts for.
type account struct {
	login string
	// open is an instance with sign-in off: it has no accounts to tell apart,
	// so everybody may do everything.
	open bool
}

func signInOff() bool { return os.Getenv("GITHUB_CLIENT_ID") == "" }

func accountOf(r *http.Request) account {
	if signInOff() {
		return account{open: true}
	}
	if claims, ok := r.Context().Value(middleware.ClaimsContextKey).(*auth.AppClaims); ok && claims != nil {
		return account{login: claims.Login}
	}
	return account{}
}

func accountFor(login string) account {
	if signInOff() {
		return account{open: true}
	}
	return account{login: login}
}

// isOrgAdmin is replaced in tests.
var isOrgAdmin = auth.IsOrgAdmin

// privileged is the author of something, or an administrator.
func (a account) privileged(author string) bool {
	if a.open {
		return true
	}
	if a.login == "" {
		return false
	}
	return strings.EqualFold(a.login, author) || isOrgAdmin(a.login)
}

func (a account) mayManageFolder(f *folder) bool {
	return a.privileged(f.CreatedBy)
}

func (a account) mayEditFolder(f *folder) bool {
	if f.Access == accessEdit && a.login != "" {
		return true
	}
	return a.privileged(f.CreatedBy)
}

// folderOfLocked returns the folder a board is in, or nil when it is in none
// (or in one deleted since). The caller holds registry.mu.
func folderOfLocked(folderID string) *folder {
	if folderID == "" {
		return nil
	}
	return registry.folders[folderID]
}

// mayPutInLocked tells whether the account may put a board into folderID
// (empty for none): the folder lets them change its boards.
func (a account) mayPutInLocked(folderID string) bool {
	f := folderOfLocked(folderID)
	return f == nil || a.mayEditFolder(f)
}

// mayEditLocked tells whether the account may change a board: draw on it,
// rename it, restore a version, delete it. The folder's author and the
// administrators may; anybody else needs both the board and its folder to
// allow it — the board's author, or a board open to everybody, in a folder
// they may change.
func (a account) mayEditLocked(e entry) bool {
	f := folderOfLocked(e.Folder)
	if f != nil && a.privileged(f.CreatedBy) {
		return true
	}
	board := a.privileged(e.CreatedBy) || (e.Access == accessEdit && a.login != "")
	return board && (f == nil || a.mayEditFolder(f))
}

// mayManageBoard tells whether the account may set who may change a board.
func (a account) mayManageBoard(e entry) bool {
	return a.privileged(e.CreatedBy)
}

// mayMoveLocked tells whether the account may take a board out of where it is:
// its author, the author of its folder or an administrator. Anybody else could
// put a board into a folder of their own and lock its author out of it. Where
// it goes takes mayPutInLocked too.
func (a account) mayMoveLocked(e entry) bool {
	if f := folderOfLocked(e.Folder); f != nil && a.privileged(f.CreatedBy) {
		return true
	}
	return a.privileged(e.CreatedBy)
}

// mayPurge tells whether the account may delete a board in the trash for good:
// only its author or an administrator, wherever it was.
func (a account) mayPurge(e entry) bool {
	return a.privileged(e.CreatedBy)
}

// MayWriteRoom tells whether the account signed in as login may change the
// board in a collaboration room. A room that is not listed is treated as a
// board outside any folder. It serves the collaboration server and the
// Firestore-compatible API, which see a room, not a request to the list.
func MayWriteRoom(ctx context.Context, store core.CanvasStore, login, room string) bool {
	a := accountFor(login)
	if a.open {
		return true
	}
	if ensureLoaded(ctx, store) != nil {
		// Without the list the board's folder is unknown. The list stays in
		// memory once read, so this happens only until the first read succeeds.
		return false
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	rec := registry.records[room]
	return rec == nil || a.mayEditLocked(rec.entry)
}

// MayWriteRequest is MayWriteRoom for an HTTP request.
func MayWriteRequest(r *http.Request, store core.CanvasStore, room string) bool {
	if signInOff() {
		return true
	}
	return MayWriteRoom(r.Context(), store, accountOf(r).login, room)
}

func forbid(w http.ResponseWriter, message string) {
	http.Error(w, message, http.StatusForbidden)
}
