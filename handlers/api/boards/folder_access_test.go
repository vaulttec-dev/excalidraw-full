package boards

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"excalidraw-complete/handlers/api/firebase"
	"excalidraw-complete/stores/memory"

	"github.com/go-chi/chi/v5"
)

// alice made a folder, bob a board in it and one outside any; carol is anybody
// else and root an organisation admin.
func TestFolderAccess(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_ID", "client")
	previous := isOrgAdmin
	isOrgAdmin = func(login string) bool { return login == "root" }
	t.Cleanup(func() { isOrgAdmin = previous })

	ctx := context.Background()
	store := memory.NewStore()
	registry.loaded = false
	const (
		inside = "0123456789abcdef0001"
		loose  = "0123456789abcdef0002"
		key    = "AAAAAAAAAAAAAAAAAAAAAA"
	)
	for _, id := range []string{inside, loose} {
		if err := Create(ctx, store, id, key, "Plan", "bob"); err != nil {
			t.Fatal(err)
		}
	}
	// The memory store is shared between tests; leave it as found.
	var folderID string
	t.Cleanup(func() {
		_ = store.Delete(ctx, foldersOwner, folderID)
		for _, id := range []string{inside, loose} {
			_ = store.Delete(ctx, owner, id)
			_ = store.Delete(ctx, trashOwner, id)
		}
		registry.loaded = false
	})
	f, err := CreateFolder(ctx, store, "Access", "alice")
	if err != nil {
		t.Fatal(err)
	}
	folderID = f.ID
	if f.Access != accessView {
		t.Fatalf("a new folder's access is %q, want view", f.Access)
	}
	if err := MoveBoard(ctx, store, inside, f.ID); err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	r.Get("/api/boards", HandleList(store))
	r.Get("/api/boards/trash", HandleTrash(store))
	r.Get("/api/boards/{id}", HandleGet(store))
	r.Put("/api/boards/{id}", HandlePut(store))
	r.Put("/api/boards/{id}/folder", HandleMoveBoard(store))
	r.Put("/api/boards/{id}/access", HandleBoardAccess(store))
	r.Delete("/api/boards/{id}", HandleDelete(store))
	r.Post("/api/boards/{id}/versions/{version}/restore", HandleRestoreVersion(store))
	r.Post("/api/boards/trash/{id}/restore", HandleRestoreFromTrash(store))
	r.Get("/api/folders", HandleFolders(store))
	r.Put("/api/folders/{id}", HandleRenameFolder(store))
	r.Put("/api/folders/{id}/access", HandleFolderAccess(store))
	r.Delete("/api/folders/{id}", HandleDeleteFolder(store))
	r.Post("/documents:commit", firebase.HandleBatchCommit(store))
	firebase.SetWriteGuard(func(r *http.Request, room string) bool { return MayWriteRequest(r, store, room) })
	t.Cleanup(func() { firebase.SetWriteGuard(nil) })

	call := func(method, path, login, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, as(httptest.NewRequest(method, path, strings.NewReader(body)), login))
		return rec
	}
	expect := func(what string, rec *httptest.ResponseRecorder, code int) {
		t.Helper()
		if rec.Code != code {
			t.Fatalf("%s: %d %s, want %d", what, rec.Code, strings.TrimSpace(rec.Body.String()), code)
		}
	}
	board := func(login, id string) Board {
		t.Helper()
		var b Board
		_ = json.NewDecoder(call(http.MethodGet, "/api/boards/"+id, login, "").Body).Decode(&b)
		return b
	}
	folderAs := func(login string) Folder {
		t.Helper()
		var folders []Folder
		_ = json.NewDecoder(call(http.MethodGet, "/api/folders", login, "").Body).Decode(&folders)
		// The memory store is shared between tests, so other folders show too.
		for _, got := range folders {
			if got.ID == f.ID {
				return got
			}
		}
		t.Fatal("the folder is not listed")
		return Folder{}
	}

	// What the list says.
	for _, c := range []struct {
		login              string
		canEdit, canDelete bool
	}{{"carol", false, false}, {"bob", false, false}, {"Alice", true, true}, {"root", true, true}} {
		if b := board(c.login, inside); b.CanEdit != c.canEdit || b.CanDelete != c.canDelete {
			t.Fatalf("%s on a board in a view-only folder: canEdit %v canDelete %v", c.login, b.CanEdit, b.CanDelete)
		}
	}
	// A board is view-only for others too until its author opens it.
	if b := board("carol", loose); b.CanEdit || b.CanDelete || b.CanManage || b.Access != accessView {
		t.Fatalf("carol on bob's board outside folders: %+v", b)
	}
	if b := board("bob", loose); !b.CanEdit || !b.CanDelete || !b.CanManage {
		t.Fatalf("bob on his board outside folders: %+v", b)
	}
	if got := folderAs("carol"); got.Access != accessView || got.CanEdit || got.CanManage {
		t.Fatalf("carol sees the folder as %+v", got)
	}
	if got := folderAs("root"); !got.CanEdit || !got.CanManage {
		t.Fatalf("an admin sees the folder as %+v", got)
	}

	// Looking only.
	expect("rename by carol", call(http.MethodPut, "/api/boards/"+inside, "carol", `{"key":"`+key+`","name":"X"}`), http.StatusForbidden)
	expect("re-registering without a name", call(http.MethodPut, "/api/boards/"+inside, "carol", `{"key":"`+key+`"}`), http.StatusNoContent)
	expect("moving out by carol", call(http.MethodPut, "/api/boards/"+inside+"/folder", "carol", `{"folder":""}`), http.StatusForbidden)
	expect("moving in by carol", call(http.MethodPut, "/api/boards/"+loose+"/folder", "carol", `{"folder":"`+f.ID+`"}`), http.StatusForbidden)
	// A board outside folders moves only by its author or an admin, even into
	// a folder the mover may change: otherwise carol could lock bob out of it.
	own, err := CreateFolder(ctx, store, "Carol's", "carol")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, foldersOwner, own.ID) })
	if b := board("carol", loose); b.CanMove {
		t.Fatal("carol may move bob's board outside folders")
	}
	if b := board("bob", loose); !b.CanMove {
		t.Fatal("bob may not move his own board")
	}
	expect("someone else's loose board into one's own folder", call(http.MethodPut, "/api/boards/"+loose+"/folder", "carol", `{"folder":"`+own.ID+`"}`), http.StatusForbidden)
	expect("own loose board into a view-only folder", call(http.MethodPut, "/api/boards/"+loose+"/folder", "bob", `{"folder":"`+own.ID+`"}`), http.StatusForbidden)
	expect("admin moving a loose board", call(http.MethodPut, "/api/boards/"+loose+"/folder", "root", `{"folder":"`+own.ID+`"}`), http.StatusNoContent)
	expect("folder author moving it back out", call(http.MethodPut, "/api/boards/"+loose+"/folder", "carol", `{"folder":""}`), http.StatusNoContent)
	expect("delete by the board's author", call(http.MethodDelete, "/api/boards/"+inside, "bob", ""), http.StatusForbidden)
	expect("version restore by carol", call(http.MethodPost, "/api/boards/"+inside+"/versions/1/restore", "carol", ""), http.StatusForbidden)
	expect("folder rename by carol", call(http.MethodPut, "/api/folders/"+f.ID, "carol", `{"name":"Mine"}`), http.StatusForbidden)
	expect("folder access by carol", call(http.MethodPut, "/api/folders/"+f.ID+"/access", "carol", `{"access":"edit"}`), http.StatusForbidden)
	expect("folder delete by carol", call(http.MethodDelete, "/api/folders/"+f.ID, "carol", ""), http.StatusForbidden)
	commit := `{"writes":[{"update":{"name":"` + firebase.DocumentPath(inside) + `","fields":{}}}]}`
	expect("saving the scene as carol", call(http.MethodPost, "/documents:commit", "carol", commit), http.StatusForbidden)
	expect("saving the scene as alice", call(http.MethodPost, "/documents:commit", "alice", commit), http.StatusOK)
	if MayWriteRoom(ctx, store, "carol", inside) || !MayWriteRoom(ctx, store, "alice", inside) {
		t.Fatal("MayWriteRoom disagrees with the folder")
	}
	if MayWriteRoom(ctx, store, "carol", loose) || !MayWriteRoom(ctx, store, "bob", loose) {
		t.Fatal("MayWriteRoom disagrees with the board")
	}
	if !MayWriteRoom(ctx, store, "carol", "ffffffffffffffffffff") {
		t.Fatal("a room that is not listed takes changes from anybody")
	}

	// The board's author opens it: outside folders that is enough; in a
	// view-only folder it is not.
	expect("board access by carol", call(http.MethodPut, "/api/boards/"+loose+"/access", "carol", `{"access":"edit"}`), http.StatusForbidden)
	expect("bad board access", call(http.MethodPut, "/api/boards/"+loose+"/access", "bob", `{"access":"all"}`), http.StatusBadRequest)
	expect("board access by bob", call(http.MethodPut, "/api/boards/"+loose+"/access", "bob", `{"access":"edit"}`), http.StatusOK)
	if b := board("carol", loose); !b.CanEdit || !b.CanDelete || b.CanMove || b.CanManage {
		t.Fatalf("carol on bob's opened board: %+v", b)
	}
	expect("board access by bob, in a folder", call(http.MethodPut, "/api/boards/"+inside+"/access", "bob", `{"access":"edit"}`), http.StatusOK)
	if b := board("carol", inside); b.CanEdit || b.Access != accessEdit {
		t.Fatalf("carol on an opened board in a view-only folder: %+v", b)
	}
	if !MayEditFolder(ctx, store, "root", f.ID) || MayEditFolder(ctx, store, "carol", f.ID) {
		t.Fatal("MayEditFolder disagrees with the folder")
	}

	// The author opens the folder for everybody.
	expect("bad access", call(http.MethodPut, "/api/folders/"+f.ID+"/access", "alice", `{"access":"all"}`), http.StatusBadRequest)
	expect("folder access by alice", call(http.MethodPut, "/api/folders/"+f.ID+"/access", "alice", `{"access":"edit"}`), http.StatusOK)
	if b := board("carol", inside); !b.CanEdit || !b.CanDelete {
		t.Fatalf("carol on an editable folder: %+v", b)
	}
	if got := folderAs("carol"); got.Access != accessEdit || !got.CanEdit || got.CanManage {
		t.Fatalf("carol sees the opened folder as %+v", got)
	}
	expect("rename by carol, editable", call(http.MethodPut, "/api/boards/"+inside, "carol", `{"key":"`+key+`","name":"Y"}`), http.StatusNoContent)
	expect("folder rename by carol, editable", call(http.MethodPut, "/api/folders/"+f.ID, "carol", `{"name":"Mine"}`), http.StatusForbidden)
	expect("delete by carol, editable", call(http.MethodDelete, "/api/boards/"+inside, "carol", ""), http.StatusNoContent)

	// Read back from storage: the access is kept.
	registry.loaded = false
	if got := folderAs("carol"); got.Access != accessEdit {
		t.Fatalf("access after a reload is %q", got.Access)
	}

	// Back to looking only: what was deleted from the folder goes back to it,
	// so restoring it takes the right to change the folder.
	expect("folder access by root", call(http.MethodPut, "/api/folders/"+f.ID+"/access", "root", `{"access":"view"}`), http.StatusOK)
	var trash []TrashedBoard
	_ = json.NewDecoder(call(http.MethodGet, "/api/boards/trash", "carol", "").Body).Decode(&trash)
	if len(trash) != 1 || trash[0].CanRestore || trash[0].CanPurge {
		t.Fatalf("carol's trash: %+v", trash)
	}
	expect("restore by carol", call(http.MethodPost, "/api/boards/trash/"+inside+"/restore", "carol", ""), http.StatusForbidden)
	expect("restore by alice", call(http.MethodPost, "/api/boards/trash/"+inside+"/restore", "alice", ""), http.StatusOK)
	if b := board("carol", inside); b.Folder != f.ID || b.CanEdit {
		t.Fatalf("restored board: %+v", b)
	}

	// Only those who manage it delete the folder; its boards come out of it.
	expect("folder delete by alice", call(http.MethodDelete, "/api/folders/"+f.ID, "alice", ""), http.StatusNoContent)
	if b := board("carol", inside); b.Folder != "" || !b.CanEdit {
		t.Fatalf("board of a deleted folder: %+v", b)
	}
}

func TestOpenInstanceAllowsEverything(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_ID", "")
	ctx := context.Background()
	store := memory.NewStore()
	registry.loaded = false
	const id = "0123456789abcdef0003"
	if err := Create(ctx, store, id, "AAAAAAAAAAAAAAAAAAAAAA", "Plan", "bob"); err != nil {
		t.Fatal(err)
	}
	f, err := CreateFolder(ctx, store, "Open instance", "alice")
	if err != nil {
		t.Fatal(err)
	}
	// The memory store is shared between tests; leave it as found.
	t.Cleanup(func() {
		_ = store.Delete(ctx, owner, id)
		_ = store.Delete(ctx, foldersOwner, f.ID)
		registry.loaded = false
	})
	if err := MoveBoard(ctx, store, id, f.ID); err != nil {
		t.Fatal(err)
	}
	if !MayWriteRoom(ctx, store, "", id) || !MayEditFolder(ctx, store, "", f.ID) {
		t.Fatal("an instance with sign-in off refused a change")
	}
}
