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
	"sync"
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

// loadConcurrency bounds the parallel reads when the registry is loaded.
const loadConcurrency = 8

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

// record is a registry entry as held in memory.
type record struct {
	entry
	name      string
	createdAt time.Time
}

func (r *record) canvas(id string) *core.Canvas {
	data, _ := json.Marshal(r.entry)
	// The creation time is passed along so the store does not read the object
	// back to preserve it.
	return &core.Canvas{ID: id, UserID: owner, Name: r.name, Data: data, CreatedAt: r.createdAt}
}

// Board is a registry entry as the list sees it.
type Board struct {
	ID        string    `json:"id"`
	Key       string    `json:"key"`
	Name      string    `json:"name"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
	EditedAt  time.Time `json:"editedAt"`
}

// registry holds the whole list in memory. Reading it from the object store
// took a round trip per board, and the store is far enough away that the list
// took well over a second with a single board in it. The instance runs as a
// single process, so memory holds the current list; every change is written to
// the store before it is acknowledged, and the list is read back from the store
// once, at startup.
var registry = struct {
	mu      sync.RWMutex
	loaded  bool
	records map[string]*record
}{}

// Preload reads the registry in the background, so the first request after a
// restart does not wait for it.
func Preload(store core.CanvasStore) {
	go func() {
		if err := ensureLoaded(context.Background(), store); err != nil {
			logrus.WithError(err).Warn("failed to preload the board list; will retry on first request")
		}
	}()
}

func ensureLoaded(ctx context.Context, store core.CanvasStore) error {
	registry.mu.RLock()
	loaded := registry.loaded
	registry.mu.RUnlock()
	if loaded {
		return nil
	}

	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.loaded {
		return nil
	}

	records, err := readRegistry(ctx, store)
	if err != nil {
		return err
	}
	seedEditTimes(ctx, store, records)

	registry.records = records
	registry.loaded = true
	logrus.WithField("boards", len(records)).Info("Board list loaded")
	return nil
}

func listIDs(ctx context.Context, store core.CanvasStore, userID string) ([]string, error) {
	if lister, ok := store.(core.CanvasMetaLister); ok {
		metas, err := lister.ListMeta(ctx, userID)
		if err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(metas))
		for _, meta := range metas {
			ids = append(ids, meta.ID)
		}
		return ids, nil
	}

	canvases, err := store.List(ctx, userID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(canvases))
	for _, canvas := range canvases {
		ids = append(ids, canvas.ID)
	}
	return ids, nil
}

func readRegistry(ctx context.Context, store core.CanvasStore) (map[string]*record, error) {
	ids, err := listIDs(ctx, store, owner)
	if err != nil {
		return nil, err
	}

	records := make(map[string]*record, len(ids))
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		sem = make(chan struct{}, loadConcurrency)
	)
	for _, id := range ids {
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()

			canvas, err := store.Get(ctx, owner, id)
			if err != nil || canvas == nil {
				logrus.WithError(err).WithField("board", id).Warn("failed to read board entry")
				return
			}
			var e entry
			if json.Unmarshal(canvas.Data, &e) != nil || e.Key == "" {
				return
			}

			mu.Lock()
			records[id] = &record{entry: e, name: canvas.Name, createdAt: canvas.CreatedAt}
			mu.Unlock()
		}(id)
	}
	wg.Wait()
	return records, nil
}

// seedEditTimes learns when each board was last saved from the store's object
// timestamps, in one listing. Saves made after startup are tracked as they
// happen.
func seedEditTimes(ctx context.Context, store core.CanvasStore, records map[string]*record) {
	lister, ok := store.(core.CanvasMetaLister)
	if !ok {
		return
	}
	metas, err := lister.ListMeta(ctx, firebase.RoomOwner)
	if err != nil {
		logrus.WithError(err).Warn("failed to read when boards were last edited")
		return
	}
	edited := make(map[string]time.Time, len(metas))
	for _, meta := range metas {
		edited[meta.ID] = meta.UpdatedAt
	}
	for id := range records {
		if at, ok := edited[firebase.StorageID(id)]; ok {
			firebase.SeedEditedAt(id, at)
		}
	}
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

func unavailable(w http.ResponseWriter, err error) {
	logrus.WithError(err).Error("board list unavailable")
	http.Error(w, "board list unavailable", http.StatusServiceUnavailable)
}

// HandleList returns every board, most recently edited first.
func HandleList(store core.CanvasStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := ensureLoaded(r.Context(), store); err != nil {
			unavailable(w, err)
			return
		}

		registry.mu.RLock()
		boards := make([]Board, 0, len(registry.records))
		for id, rec := range registry.records {
			board := Board{
				ID:        id,
				Key:       rec.Key,
				Name:      rec.name,
				CreatedBy: rec.CreatedBy,
				CreatedAt: rec.createdAt,
				EditedAt:  rec.createdAt,
			}
			if at, ok := firebase.EditedAt(id); ok && at.After(board.EditedAt) {
				board.EditedAt = at
			}
			boards = append(boards, board)
		}
		registry.mu.RUnlock()

		sort.Slice(boards, func(i, j int) bool { return boards[i].EditedAt.After(boards[j].EditedAt) })
		render.JSON(w, r, boards)
	}
}

// HandlePut registers a board, or renames one that is already listed. The
// editor calls it on every load, so registering is idempotent and a plain
// re-registration changes nothing — and, served from memory, costs nothing.
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
		if err := ensureLoaded(r.Context(), store); err != nil {
			unavailable(w, err)
			return
		}

		var name string
		if body.Name != nil {
			name = strings.TrimSpace(*body.Name)
			if len([]rune(name)) > maxNameLength {
				name = string([]rune(name)[:maxNameLength])
			}
		}

		registry.mu.RLock()
		existing := registry.records[id]
		registry.mu.RUnlock()

		var next record
		status := http.StatusCreated
		if existing != nil {
			// The key is fixed at registration. Accepting another one would let
			// anybody who knows a room id point the list at a room they control.
			if existing.Key != body.Key {
				http.Error(w, "board is registered with another key", http.StatusConflict)
				return
			}
			if body.Name == nil || name == existing.name {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next = *existing
			next.name = name
			status = http.StatusNoContent
		} else {
			next = record{
				entry:     entry{Key: body.Key, CreatedBy: author(r)},
				name:      name,
				createdAt: time.Now(),
			}
		}

		if err := store.Save(r.Context(), next.canvas(id)); err != nil {
			logrus.WithError(err).WithField("board", id).Error("failed to save board")
			http.Error(w, "failed to save board", http.StatusInternalServerError)
			return
		}

		registry.mu.Lock()
		registry.records[id] = &next
		registry.mu.Unlock()
		w.WriteHeader(status)
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
		if err := ensureLoaded(r.Context(), store); err != nil {
			unavailable(w, err)
			return
		}
		if err := firebase.DeleteScene(r.Context(), store, id); err != nil {
			logrus.WithError(err).WithField("board", id).Warn("failed to delete board scene")
		}
		if err := store.Delete(r.Context(), owner, id); err != nil {
			http.Error(w, "failed to delete board", http.StatusInternalServerError)
			return
		}

		registry.mu.Lock()
		delete(registry.records, id)
		registry.mu.Unlock()
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
		if err := ensureLoaded(r.Context(), store); err != nil {
			unavailable(w, err)
			return
		}

		legacy, err := store.List(r.Context(), source)
		if err != nil {
			http.Error(w, "failed to list previous canvases", http.StatusInternalServerError)
			return
		}

		imported := make(map[string]bool)
		registry.mu.RLock()
		for _, rec := range registry.records {
			if rec.Source != "" {
				imported[rec.Source] = true
			}
		}
		registry.mu.RUnlock()

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
			rec := &record{
				entry:     entry{Key: key, CreatedBy: author(r), Source: marker},
				name:      name,
				createdAt: full.CreatedAt,
			}
			if rec.createdAt.IsZero() {
				rec.createdAt = time.Now()
			}
			if err := store.Save(r.Context(), rec.canvas(room)); err != nil {
				result.FailedNames = append(result.FailedNames, item.Name)
				continue
			}
			registry.mu.Lock()
			registry.records[room] = rec
			registry.mu.Unlock()

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
