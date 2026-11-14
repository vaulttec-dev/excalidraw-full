package boards

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"excalidraw-complete/handlers/auth"
	"excalidraw-complete/middleware"
	"excalidraw-complete/stores/memory"

	"github.com/go-chi/chi/v5"
)

func as(r *http.Request, login string) *http.Request {
	claims := &auth.AppClaims{Login: login}
	return r.WithContext(context.WithValue(r.Context(), middleware.ClaimsContextKey, claims))
}

func TestOnlyAuthorOrAdminDeletes(t *testing.T) {
	t.Setenv("GITHUB_CLIENT_ID", "client")
	previous := isOrgAdmin
	isOrgAdmin = func(login string) bool { return login == "root" }
	t.Cleanup(func() { isOrgAdmin = previous })

	ctx := context.Background()
	store := memory.NewStore()
	registry.loaded = false
	const (
		mine   = "0123456789abcdef0123"
		theirs = "0123456789abcdef0124"
		key    = "AAAAAAAAAAAAAAAAAAAAAA"
	)
	for id, author := range map[string]string{mine: "Alice", theirs: "bob"} {
		if err := Create(ctx, store, id, key, "Plan", author); err != nil {
			t.Fatal(err)
		}
	}

	r := chi.NewRouter()
	r.Get("/api/boards", HandleList(store))
	r.Delete("/api/boards/{id}", HandleDelete(store))
	r.Get("/api/boards/trash", HandleTrash(store))
	r.Delete("/api/boards/trash/{id}", HandlePurge(store))
	call := func(method, path, login string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, as(httptest.NewRequest(method, path, nil), login))
		return rec
	}

	var listed []Board
	_ = json.NewDecoder(call(http.MethodGet, "/api/boards", "alice").Body).Decode(&listed)
	// The memory store is shared between tests, so the list holds their boards
	// too.
	for _, b := range listed {
		if b.ID != mine && b.ID != theirs {
			continue
		}
		if want := b.ID == mine; b.CanDelete != want {
			t.Fatalf("alice canDelete %s = %v, want %v", b.ID, b.CanDelete, want)
		}
	}

	if rec := call(http.MethodDelete, "/api/boards/"+theirs, "alice"); rec.Code != http.StatusForbidden {
		t.Fatalf("someone else's board: %d, want 403", rec.Code)
	}
	if _, ok := Find(ctx, store, theirs); !ok {
		t.Fatal("a refused delete removed the board")
	}
	// Logins are compared without case, as GitHub does.
	if rec := call(http.MethodDelete, "/api/boards/"+mine, "alice"); rec.Code != http.StatusNoContent {
		t.Fatalf("own board: %d, want 204", rec.Code)
	}
	if rec := call(http.MethodDelete, "/api/boards/"+theirs, "root"); rec.Code != http.StatusNoContent {
		t.Fatalf("admin: %d, want 204", rec.Code)
	}

	var trash []TrashedBoard
	_ = json.NewDecoder(call(http.MethodGet, "/api/boards/trash", "alice").Body).Decode(&trash)
	for _, item := range trash {
		if item.ID != mine && item.ID != theirs {
			continue
		}
		if want := item.ID == mine; item.CanPurge != want {
			t.Fatalf("alice canPurge %s = %v, want %v", item.ID, item.CanPurge, want)
		}
	}
	if rec := call(http.MethodDelete, "/api/boards/trash/"+theirs, "alice"); rec.Code != http.StatusForbidden {
		t.Fatalf("purging someone else's board: %d, want 403", rec.Code)
	}
	if rec := call(http.MethodDelete, "/api/boards/trash/"+mine, "alice"); rec.Code != http.StatusNoContent {
		t.Fatalf("purging own board: %d, want 204", rec.Code)
	}
}
