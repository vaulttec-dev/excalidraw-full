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
	"encoding/json"
	"excalidraw-complete/core"
	"excalidraw-complete/handlers/api/firebase"
	"excalidraw-complete/handlers/api/history"
	"excalidraw-complete/handlers/api/scene"
	"excalidraw-complete/handlers/auth"
	"excalidraw-complete/middleware"
	"fmt"
	"net/http"
	"regexp"
	"sort"
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
	// Folder is the id of the folder the board is in; empty for none.
	Folder string `json:"folder,omitempty"`
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
	Folder    string    `json:"folder,omitempty"`
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
	trash   map[string]*trashed
	folders map[string]*folder
}{}

// trashOwner is where deleted boards go. A deleted board keeps its key and
// name there, and its last content as a version (see handlers/api/history), so
// it can be brought back.
const trashOwner = "trash"

type trashEntry struct {
	entry
	DeletedBy string `json:"deletedBy"`
}

type trashed struct {
	trashEntry
	name      string
	createdAt time.Time
	deletedAt time.Time
}

// Preload reads the registry in the background, so the first request after a
// restart does not wait for it.
func Preload(store core.CanvasStore) {
	go func() {
		ctx := context.Background()
		if err := ensureLoaded(ctx, store); err != nil {
			logrus.WithError(err).Warn("failed to preload the board list; will retry on first request")
			return
		}
		baselineVersions(ctx, store)
	}()
}

// baselineVersions gives every board that has no version yet a first one, so a
// board nobody has edited since versions were introduced is covered too.
func baselineVersions(ctx context.Context, store core.CanvasStore) {
	registry.mu.RLock()
	ids := make([]string, 0, len(registry.records))
	for id := range registry.records {
		ids = append(ids, id)
	}
	registry.mu.RUnlock()

	taken := 0
	for _, id := range ids {
		versions, err := history.List(ctx, store, id)
		if err != nil || len(versions) > 0 {
			continue
		}
		if fields, ok := firebase.LoadScene(ctx, store, id); ok {
			if err := history.Snapshot(ctx, store, id, fields); err == nil {
				taken++
			}
		}
	}
	if taken > 0 {
		logrus.WithField("boards", taken).Info("Recorded first versions of boards")
	}
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
	trash, err := readTrash(ctx, store)
	if err != nil {
		return err
	}
	folders, err := readFolders(ctx, store)
	if err != nil {
		return err
	}
	seedEditTimes(ctx, store, records)

	registry.records = records
	registry.trash = trash
	registry.folders = folders
	registry.loaded = true
	logrus.WithField("boards", len(records)).Info("Board list loaded")
	return nil
}

// HandleHealth reports whether the instance can serve boards: it answers once
// the board list has been read from storage, which also proves the storage is
// reachable with the configured credentials. Until then it answers 503, so a
// container that cannot reach its bucket shows up as unhealthy rather than as
// running with an empty board list.
func HandleHealth(store core.CanvasStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := ensureLoaded(ctx, store); err != nil {
			http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok"))
	}
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

// readAll reads every canvas under a namespace, a few at a time.
func readAll(ctx context.Context, store core.CanvasStore, namespace string) (map[string]*core.Canvas, error) {
	ids, err := listIDs(ctx, store, namespace)
	if err != nil {
		return nil, err
	}

	canvases := make(map[string]*core.Canvas, len(ids))
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

			canvas, err := store.Get(ctx, namespace, id)
			if err != nil || canvas == nil {
				logrus.WithError(err).WithFields(logrus.Fields{"namespace": namespace, "id": id}).Warn("failed to read entry")
				return
			}
			mu.Lock()
			canvases[id] = canvas
			mu.Unlock()
		}(id)
	}
	wg.Wait()
	return canvases, nil
}

func readRegistry(ctx context.Context, store core.CanvasStore) (map[string]*record, error) {
	canvases, err := readAll(ctx, store, owner)
	if err != nil {
		return nil, err
	}
	records := make(map[string]*record, len(canvases))
	for id, canvas := range canvases {
		var e entry
		if json.Unmarshal(canvas.Data, &e) != nil || e.Key == "" {
			continue
		}
		records[id] = &record{entry: e, name: canvas.Name, createdAt: canvas.CreatedAt}
	}
	return records, nil
}

func readTrash(ctx context.Context, store core.CanvasStore) (map[string]*trashed, error) {
	canvases, err := readAll(ctx, store, trashOwner)
	if err != nil {
		return nil, err
	}
	items := make(map[string]*trashed, len(canvases))
	for id, canvas := range canvases {
		var t trashEntry
		if json.Unmarshal(canvas.Data, &t) != nil || t.Key == "" {
			continue
		}
		items[id] = &trashed{trashEntry: t, name: canvas.Name, createdAt: canvas.CreatedAt, deletedAt: canvas.UpdatedAt}
	}
	return items, nil
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

func toBoard(id string, rec *record) Board {
	board := Board{
		ID:        id,
		Key:       rec.Key,
		Name:      rec.name,
		CreatedBy: rec.CreatedBy,
		Folder:    rec.Folder,
		CreatedAt: rec.createdAt,
		EditedAt:  rec.createdAt,
	}
	if at, ok := firebase.EditedAt(id); ok && at.After(board.EditedAt) {
		board.EditedAt = at
	}
	return board
}

// Snapshot returns every board, most recently edited first.
func Snapshot(ctx context.Context, store core.CanvasStore) ([]Board, error) {
	if err := ensureLoaded(ctx, store); err != nil {
		return nil, err
	}

	registry.mu.RLock()
	boards := make([]Board, 0, len(registry.records))
	for id, rec := range registry.records {
		boards = append(boards, toBoard(id, rec))
	}
	registry.mu.RUnlock()

	sort.Slice(boards, func(i, j int) bool { return boards[i].EditedAt.After(boards[j].EditedAt) })
	return boards, nil
}

// Find returns a listed board by its room id.
func Find(ctx context.Context, store core.CanvasStore, id string) (Board, bool) {
	if ensureLoaded(ctx, store) != nil {
		return Board{}, false
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	rec, ok := registry.records[id]
	if !ok {
		return Board{}, false
	}
	return toBoard(id, rec), true
}

// Create lists a new board.
func Create(ctx context.Context, store core.CanvasStore, id, key, name, author string) error {
	if !roomIDPattern.MatchString(id) || !roomKeyPattern.MatchString(key) {
		return fmt.Errorf("invalid room id or key")
	}
	if err := ensureLoaded(ctx, store); err != nil {
		return err
	}
	rec := &record{
		entry:     entry{Key: key, CreatedBy: author},
		name:      strings.TrimSpace(name),
		createdAt: time.Now(),
	}
	if err := store.Save(ctx, rec.canvas(id)); err != nil {
		return err
	}
	registry.mu.Lock()
	registry.records[id] = rec
	registry.mu.Unlock()
	return nil
}

// HandleList returns every board, most recently edited first.
func HandleList(store core.CanvasStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		boards, err := Snapshot(r.Context(), store)
		if err != nil {
			unavailable(w, err)
			return
		}
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
		_, inTrash := registry.trash[id]
		registry.mu.RUnlock()

		// A tab that still has a deleted board open would otherwise list it
		// again, empty and untitled, the next time it loads. Deleted boards
		// come back only through the trash.
		if existing == nil && inTrash {
			http.Error(w, "board is in the trash", http.StatusGone)
			return
		}

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

// HandleDelete moves a board to the trash. Its last content is kept as a
// version first; if that cannot be written, the board is not deleted.
func HandleDelete(store core.CanvasStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := chi.URLParam(r, "id")
		if !roomIDPattern.MatchString(id) {
			http.Error(w, "invalid room id", http.StatusBadRequest)
			return
		}
		if err := ensureLoaded(ctx, store); err != nil {
			unavailable(w, err)
			return
		}
		registry.mu.RLock()
		rec := registry.records[id]
		registry.mu.RUnlock()
		if rec == nil {
			http.Error(w, "no such board", http.StatusNotFound)
			return
		}

		if fields, ok := firebase.LoadScene(ctx, store, id); ok {
			if err := history.Snapshot(ctx, store, id, fields); err != nil {
				logrus.WithError(err).WithField("board", id).Error("failed to keep the board before deleting it")
				http.Error(w, "failed to keep the board's content; nothing was deleted", http.StatusInternalServerError)
				return
			}
		}

		item := &trashed{
			trashEntry: trashEntry{entry: rec.entry, DeletedBy: author(r)},
			name:       rec.name,
			createdAt:  rec.createdAt,
			deletedAt:  time.Now(),
		}
		data, _ := json.Marshal(item.trashEntry)
		if err := store.Save(ctx, &core.Canvas{ID: id, UserID: trashOwner, Name: rec.name, Data: data, CreatedAt: rec.createdAt}); err != nil {
			http.Error(w, "failed to move the board to the trash", http.StatusInternalServerError)
			return
		}
		if err := firebase.DeleteScene(ctx, store, id); err != nil {
			logrus.WithError(err).WithField("board", id).Warn("failed to delete board scene")
		}
		if err := store.Delete(ctx, owner, id); err != nil {
			http.Error(w, "failed to delete board", http.StatusInternalServerError)
			return
		}

		registry.mu.Lock()
		delete(registry.records, id)
		registry.trash[id] = item
		registry.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}
}

// TrashedBoard is a deleted board as the trash lists it.
type TrashedBoard struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedBy string    `json:"createdBy"`
	DeletedBy string    `json:"deletedBy"`
	DeletedAt time.Time `json:"deletedAt"`
}

// HandleTrash lists deleted boards, most recently deleted first.
func HandleTrash(store core.CanvasStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := ensureLoaded(r.Context(), store); err != nil {
			unavailable(w, err)
			return
		}
		registry.mu.RLock()
		items := make([]TrashedBoard, 0, len(registry.trash))
		for id, t := range registry.trash {
			items = append(items, TrashedBoard{ID: id, Name: t.name, CreatedBy: t.CreatedBy, DeletedBy: t.DeletedBy, DeletedAt: t.deletedAt})
		}
		registry.mu.RUnlock()
		sort.Slice(items, func(i, j int) bool { return items[i].DeletedAt.After(items[j].DeletedAt) })
		render.JSON(w, r, items)
	}
}

// HandleRestoreFromTrash brings a deleted board back with its last content.
func HandleRestoreFromTrash(store core.CanvasStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := chi.URLParam(r, "id")
		if err := ensureLoaded(ctx, store); err != nil {
			unavailable(w, err)
			return
		}
		registry.mu.RLock()
		item := registry.trash[id]
		registry.mu.RUnlock()
		if item == nil {
			http.Error(w, "no such board in the trash", http.StatusNotFound)
			return
		}

		if fields, ok := history.Latest(ctx, store, id); ok {
			if err := firebase.StoreScene(ctx, store, id, fields); err != nil {
				http.Error(w, "failed to restore the board's content", http.StatusInternalServerError)
				return
			}
		}
		rec := &record{entry: item.entry, name: item.name, createdAt: item.createdAt}
		registry.mu.RLock()
		if _, ok := registry.folders[rec.Folder]; !ok {
			// Its folder was deleted meanwhile; it comes back outside any.
			rec.Folder = ""
		}
		registry.mu.RUnlock()
		if err := store.Save(ctx, rec.canvas(id)); err != nil {
			http.Error(w, "failed to restore the board", http.StatusInternalServerError)
			return
		}
		if err := store.Delete(ctx, trashOwner, id); err != nil {
			logrus.WithError(err).WithField("board", id).Warn("failed to clear the trash entry")
		}

		registry.mu.Lock()
		registry.records[id] = rec
		delete(registry.trash, id)
		registry.mu.Unlock()
		render.JSON(w, r, toBoard(id, rec))
	}
}

// HandlePurge deletes a board from the trash for good: its versions, its scene
// and the trash entry. Only a board already in the trash can be purged, so a
// board always passes through the trash first.
func HandlePurge(store core.CanvasStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := chi.URLParam(r, "id")
		if err := ensureLoaded(ctx, store); err != nil {
			unavailable(w, err)
			return
		}
		registry.mu.RLock()
		item := registry.trash[id]
		registry.mu.RUnlock()
		if item == nil {
			http.Error(w, "no such board in the trash", http.StatusNotFound)
			return
		}

		// Versions go first: if that fails half way, the entry stays in the
		// trash and purging can be retried.
		if err := history.Purge(ctx, store, id); err != nil {
			logrus.WithError(err).WithField("board", id).Error("failed to delete board versions")
			http.Error(w, "failed to delete the board's versions", http.StatusInternalServerError)
			return
		}
		if err := firebase.DeleteScene(ctx, store, id); err != nil {
			logrus.WithError(err).WithField("board", id).Warn("failed to delete board scene")
		}
		if err := store.Delete(ctx, trashOwner, id); err != nil {
			http.Error(w, "failed to delete the trash entry", http.StatusInternalServerError)
			return
		}

		registry.mu.Lock()
		delete(registry.trash, id)
		registry.mu.Unlock()
		logrus.WithFields(logrus.Fields{"board": id, "name": item.name, "by": author(r)}).Info("board deleted for good")
		w.WriteHeader(http.StatusNoContent)
	}
}

// HandleVersions lists a board's stored versions, newest first.
func HandleVersions(store core.CanvasStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if _, ok := Find(r.Context(), store, id); !ok {
			http.Error(w, "no such board", http.StatusNotFound)
			return
		}
		versions, err := history.List(r.Context(), store, id)
		if err != nil {
			http.Error(w, "failed to list versions", http.StatusInternalServerError)
			return
		}
		render.JSON(w, r, versions)
	}
}

// HandleRestoreVersion makes a stored version the board's content again. The
// content it replaces is kept as a version first, so a restore can be undone,
// and the change reaches open tabs live, with versions they will accept.
func HandleRestoreVersion(store core.CanvasStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := chi.URLParam(r, "id")
		board, ok := Find(ctx, store, id)
		if !ok {
			http.Error(w, "no such board", http.StatusNotFound)
			return
		}
		fields, err := history.Load(ctx, store, id, chi.URLParam(r, "version"))
		if err != nil {
			http.Error(w, "no such version", http.StatusNotFound)
			return
		}
		target, err := scene.Decode(board.Key, fields)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		current, err := scene.Load(ctx, store, id, board.Key)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if currentFields, ok := firebase.LoadScene(ctx, store, id); ok {
			if err := history.Snapshot(ctx, store, id, currentFields); err != nil {
				http.Error(w, "failed to keep the current version; nothing was restored", http.StatusInternalServerError)
				return
			}
		}

		next, changed := scene.ReplaceWith(current, scene.Visible(target))
		if err := scene.Save(ctx, store, id, board.Key, next); err != nil {
			http.Error(w, "failed to restore the version", http.StatusInternalServerError)
			return
		}
		scene.Announce(id, board.Key, changed)
		w.WriteHeader(http.StatusNoContent)
	}
}
