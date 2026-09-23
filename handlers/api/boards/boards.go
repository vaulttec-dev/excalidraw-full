// Package boards keeps the list of boards the whole team can see.
//
// The upstream editor has no accounts and no board list: a board is a
// collaboration room, reachable only through its link. Without a registry a
// teammate could open a board only if somebody had passed the link around.
//
// Each entry holds the room's key next to its name, so the list can hand out
// working links. That means the server can read the boards it lists — the price
// of having a list at all, since any member must be able to open a board
// somebody else created, and the key has to live somewhere they can all reach.
package boards

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"excalidraw-complete/core"
	"excalidraw-complete/handlers/api/firebase"
	"excalidraw-complete/handlers/auth"
	"excalidraw-complete/middleware"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/render"
	"github.com/sirupsen/logrus"
)

// owner is the storage namespace the registry lives under.
const owner = "boards"

const maxNameLength = 120

// automatedAuthor names boards created without a browser session, which in
// practice means the MCP server.
const automatedAuthor = "Claude"

var (
	roomIDPattern  = regexp.MustCompile(`^[0-9a-f]{20}$`)
	roomKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)
)

// entry is what the registry stores for a board besides its name.
type entry struct {
	Key       string `json:"key"`
	CreatedBy string `json:"createdBy"`
	// Source marks a board carried over from the previous editor, so importing
	// twice does not produce duplicates.
	Source string `json:"source,omitempty"`
}

// Board is a registry entry as the list page sees it.
type Board struct {
	ID        string    `json:"id"`
	Key       string    `json:"key"`
	Name      string    `json:"name"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
	EditedAt  time.Time `json:"editedAt"`
}

func author(r *http.Request) string {
	if claims, ok := r.Context().Value(middleware.ClaimsContextKey).(*auth.AppClaims); ok && claims != nil {
		if claims.Login != "" {
			return claims.Login
		}
		return claims.Name
	}
	return automatedAuthor
}

func readEntry(canvas *core.Canvas) (entry, bool) {
	var e entry
	if canvas == nil || json.Unmarshal(canvas.Data, &e) != nil || e.Key == "" {
		return entry{}, false
	}
	return e, true
}

func load(ctx context.Context, store core.CanvasStore) ([]Board, map[string]entry, error) {
	items, err := store.List(ctx, owner)
	if err != nil {
		return nil, nil, err
	}

	// A room is rewritten on every save, so its stored timestamp is the time of
	// the last edit. One listing covers all of them.
	edited := make(map[string]time.Time)
	if rooms, err := store.List(ctx, firebase.RoomOwner); err == nil {
		for _, room := range rooms {
			edited[room.Name] = room.UpdatedAt
		}
	}

	boards := make([]Board, 0, len(items))
	entries := make(map[string]entry, len(items))
	for _, item := range items {
		// Listings leave the payload out, and the payload is where the key is.
		full, err := store.Get(ctx, owner, item.ID)
		if err != nil {
			continue
		}
		e, ok := readEntry(full)
		if !ok {
			continue
		}
		entries[item.ID] = e

		board := Board{
			ID:        item.ID,
			Key:       e.Key,
			Name:      full.Name,
			CreatedBy: e.CreatedBy,
			CreatedAt: full.CreatedAt,
			EditedAt:  full.CreatedAt,
		}
		if at, ok := edited[firebase.DocumentPath(item.ID)]; ok {
			board.EditedAt = at
		}
		boards = append(boards, board)
	}

	sort.Slice(boards, func(i, j int) bool { return boards[i].EditedAt.After(boards[j].EditedAt) })
	return boards, entries, nil
}

// HandleList returns every board, most recently edited first.
func HandleList(store core.CanvasStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		boards, _, err := load(r.Context(), store)
		if err != nil {
			logrus.WithError(err).Error("failed to list boards")
			http.Error(w, "failed to list boards", http.StatusInternalServerError)
			return
		}
		render.JSON(w, r, boards)
	}
}

// HandlePut registers a board, or renames one that is already listed. The
// editor calls it on every load, so registering is idempotent and a plain
// re-registration changes nothing.
func HandlePut(store core.CanvasStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		var body struct {
			Key  string  `json:"key"`
			Name *string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if !roomIDPattern.MatchString(id) || !roomKeyPattern.MatchString(body.Key) {
			http.Error(w, "invalid room id or key", http.StatusBadRequest)
			return
		}

		var name string
		if body.Name != nil {
			name = strings.TrimSpace(*body.Name)
			if len([]rune(name)) > maxNameLength {
				name = string([]rune(name)[:maxNameLength])
			}
		}

		existing, err := store.Get(r.Context(), owner, id)
		if e, ok := readEntry(existing); err == nil && ok {
			// The key is fixed at registration. Accepting another one would let
			// anybody who knows a room id point the list at a room they control.
			if e.Key != body.Key {
				http.Error(w, "board is registered with another key", http.StatusConflict)
				return
			}
			if body.Name == nil || name == existing.Name {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			existing.Name = name
			// The owner is not part of a canvas's stored JSON, so a canvas read
			// back has none; without it the save would land outside the registry.
			existing.UserID = owner
			if err := store.Save(r.Context(), existing); err != nil {
				http.Error(w, "failed to rename board", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}

		data, _ := json.Marshal(entry{Key: body.Key, CreatedBy: author(r)})
		if err := store.Save(r.Context(), &core.Canvas{ID: id, UserID: owner, Name: name, Data: data}); err != nil {
			logrus.WithError(err).WithField("board", id).Error("failed to register board")
			http.Error(w, "failed to register board", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}
}

// HandleDelete removes a board: its entry and the scene stored for its room.
func HandleDelete(store core.CanvasStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if !roomIDPattern.MatchString(id) {
			http.Error(w, "invalid room id", http.StatusBadRequest)
			return
		}
		if err := firebase.DeleteScene(r.Context(), store, id); err != nil {
			logrus.WithError(err).WithField("board", id).Warn("failed to delete board scene")
		}
		if err := store.Delete(r.Context(), owner, id); err != nil {
			http.Error(w, "failed to delete board", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// legacyCanvas is the part of a canvas saved by the previous, multi-canvas
// editor that a room needs.
type legacyCanvas struct {
	Elements json.RawMessage `json:"elements"`
	Files    map[string]any  `json:"files"`
}

type importResult struct {
	Imported    int      `json:"imported"`
	Skipped     int      `json:"skipped"`
	WithImages  []string `json:"withImages,omitempty"`
	FailedNames []string `json:"failed,omitempty"`
}

// HandleImport carries the canvases the previous editor saved for an account
// (owner "github:<id>") over into rooms and lists them. The editor that wrote
// them is gone and nothing reads that prefix any more, so without this they
// would sit in storage unreachable.
func HandleImport(store core.CanvasStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		source := r.URL.Query().Get("owner")
		if !strings.HasPrefix(source, "github:") && !strings.HasPrefix(source, "oidc:") {
			http.Error(w, "owner must be a previous account prefix, e.g. github:<id>", http.StatusBadRequest)
			return
		}

		legacy, err := store.List(r.Context(), source)
		if err != nil {
			http.Error(w, "failed to list previous canvases", http.StatusInternalServerError)
			return
		}
		_, entries, err := load(r.Context(), store)
		if err != nil {
			http.Error(w, "failed to list boards", http.StatusInternalServerError)
			return
		}
		imported := make(map[string]bool, len(entries))
		for _, e := range entries {
			if e.Source != "" {
				imported[e.Source] = true
			}
		}

		var result importResult
		for _, item := range legacy {
			marker := source + "/" + item.ID
			if imported[marker] {
				result.Skipped++
				continue
			}

			full, err := store.Get(r.Context(), source, item.ID)
			if err != nil {
				result.FailedNames = append(result.FailedNames, item.Name)
				continue
			}
			var canvas legacyCanvas
			if err := json.Unmarshal(full.Data, &canvas); err != nil || len(canvas.Elements) == 0 {
				result.FailedNames = append(result.FailedNames, item.Name)
				continue
			}

			room, key, err := newRoom(r.Context(), store, canvas.Elements)
			if err != nil {
				logrus.WithError(err).WithField("canvas", item.ID).Error("failed to import canvas")
				result.FailedNames = append(result.FailedNames, item.Name)
				continue
			}

			name := strings.TrimSpace(full.Name)
			if name == "" || name == item.ID {
				name = "Стара дошка " + full.CreatedAt.Format("2006-01-02")
			}
			data, _ := json.Marshal(entry{Key: key, CreatedBy: author(r), Source: marker})
			if err := store.Save(r.Context(), &core.Canvas{
				ID: room, UserID: owner, Name: name, Data: data, CreatedAt: full.CreatedAt,
			}); err != nil {
				result.FailedNames = append(result.FailedNames, item.Name)
				continue
			}

			result.Imported++
			// Images of a room live in a storage this instance does not provide,
			// so they cannot come along; the caller is told which boards had any.
			if len(canvas.Files) > 0 {
				result.WithImages = append(result.WithImages, name)
			}
		}

		render.JSON(w, r, result)
	}
}

// newRoom stores elements as a new room, encrypted exactly the way the editor
// does it: AES-GCM with a 128 bit key and a 12 byte IV over the elements' JSON.
func newRoom(ctx context.Context, store core.CanvasStore, elements json.RawMessage) (string, string, error) {
	idBytes := make([]byte, 10)
	rawKey := make([]byte, 16)
	iv := make([]byte, 12)
	for _, b := range [][]byte{idBytes, rawKey, iv} {
		if _, err := rand.Read(b); err != nil {
			return "", "", err
		}
	}

	block, err := aes.NewCipher(rawKey)
	if err != nil {
		return "", "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", "", err
	}
	ciphertext := gcm.Seal(nil, iv, elements, nil)

	// The editor compares scene versions to decide which copy is newer.
	var versions []struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(elements, &versions); err != nil {
		return "", "", fmt.Errorf("elements are not a list: %w", err)
	}
	sceneVersion := 0
	for _, v := range versions {
		sceneVersion += v.Version
	}

	room := hex.EncodeToString(idBytes)
	fields := map[string]any{
		"sceneVersion": map[string]string{"integerValue": strconv.Itoa(sceneVersion)},
		"iv":           map[string]string{"bytesValue": base64.StdEncoding.EncodeToString(iv)},
		"ciphertext":   map[string]string{"bytesValue": base64.StdEncoding.EncodeToString(ciphertext)},
	}
	if err := firebase.StoreScene(ctx, store, room, fields); err != nil {
		return "", "", err
	}
	return room, base64.RawURLEncoding.EncodeToString(rawKey), nil
}
