package boards

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"excalidraw-complete/stores/memory"

	"github.com/go-chi/chi/v5"
)

func TestFolders(t *testing.T) {
	ctx := context.Background()
	store := memory.NewStore()
	registry.loaded = false

	const board = "0123456789abcdef0123"
	if err := Create(ctx, store, board, "AAAAAAAAAAAAAAAAAAAAAA", "Plan", "alice"); err != nil {
		t.Fatal(err)
	}
	f, err := CreateFolder(ctx, store, "  Roadmap ", "alice")
	if err != nil || f.Name != "Roadmap" || !folderIDPattern.MatchString(f.ID) {
		t.Fatalf("CreateFolder = %+v, %v", f, err)
	}
	if _, err := CreateFolder(ctx, store, "   ", "alice"); err == nil {
		t.Fatal("folder without a name accepted")
	}

	if err := MoveBoard(ctx, store, board, "ffffffffffffffff"); err != errNoFolder {
		t.Fatalf("move into a missing folder: %v", err)
	}
	if err := MoveBoard(ctx, store, board, f.ID); err != nil {
		t.Fatal(err)
	}
	// Read back from storage, not memory: the folder is recorded on the board.
	registry.loaded = false
	if b, _ := Find(ctx, store, board); b.Folder != f.ID {
		t.Fatalf("board is in %q, want %q", b.Folder, f.ID)
	}
	if found, ok := FindFolder(ctx, store, "roadmap"); !ok || found.ID != f.ID {
		t.Fatal("folder not found by name")
	}

	r := chi.NewRouter()
	r.Put("/api/folders/{id}", HandleRenameFolder(store))
	r.Delete("/api/folders/{id}", HandleDeleteFolder(store))

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/api/folders/"+f.ID, strings.NewReader(`{"name":"Q4"}`)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("rename: %d", rec.Code)
	}
	if found, _ := FindFolder(ctx, store, f.ID); found.Name != "Q4" {
		t.Fatalf("renamed folder is %q", found.Name)
	}

	// Deleting a folder keeps its boards, outside any folder.
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/folders/"+f.ID, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rec.Code)
	}
	b, ok := Find(ctx, store, board)
	if !ok || b.Folder != "" {
		t.Fatalf("board after deleting its folder: %+v, %v", b, ok)
	}
	registry.loaded = false
	if folders, _ := Folders(ctx, store); len(folders) != 0 {
		t.Fatalf("%d folders left", len(folders))
	}
}
