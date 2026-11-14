package boards

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"excalidraw-complete/core"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/render"
	"github.com/sirupsen/logrus"
)

// Folders group boards in the list. There is one level of them: a board is in
// one folder or in none. A folder holds its name and who may change its boards
// (see permissions.go); which boards are in it is recorded on the boards.

// foldersOwner is the storage namespace folders live under.
const foldersOwner = "folders"

var folderIDPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)

type folderEntry struct {
	CreatedBy string `json:"createdBy"`
	// Access is what everybody but the folder's author and the administrators
	// may do with its boards: accessEdit, or else look only.
	Access string `json:"access,omitempty"`
}

func (e folderEntry) access() string {
	if e.Access == accessEdit {
		return accessEdit
	}
	return accessView
}

type folder struct {
	folderEntry
	name      string
	createdAt time.Time
}

func (f *folder) canvas(id string) *core.Canvas {
	data, _ := json.Marshal(f.folderEntry)
	return &core.Canvas{ID: id, UserID: foldersOwner, Name: f.name, Data: data, CreatedAt: f.createdAt}
}

// Folder is a folder as the list sees it.
type Folder struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
	// Access is "view" or "edit": what everybody but the author and the
	// administrators may do with the folder's boards.
	Access string `json:"access"`
	// CanEdit tells the reader whether they may change the folder's boards.
	CanEdit bool `json:"canEdit"`
	// CanManage tells the reader whether they may rename or delete the folder
	// and change its access.
	CanManage bool `json:"canManage"`
}

func toFolder(id string, f *folder) Folder {
	return Folder{ID: id, Name: f.name, CreatedBy: f.CreatedBy, CreatedAt: f.createdAt, Access: f.access()}
}

func (a account) folderView(id string, f *folder) Folder {
	out := toFolder(id, f)
	out.CanEdit = a.mayEditFolder(f)
	out.CanManage = a.mayManageFolder(f)
	return out
}

func readFolders(ctx context.Context, store core.CanvasStore) (map[string]*folder, error) {
	canvases, err := readAll(ctx, store, foldersOwner)
	if err != nil {
		return nil, err
	}
	folders := make(map[string]*folder, len(canvases))
	for id, canvas := range canvases {
		var e folderEntry
		_ = json.Unmarshal(canvas.Data, &e)
		folders[id] = &folder{folderEntry: e, name: canvas.Name, createdAt: canvas.CreatedAt}
	}
	return folders, nil
}

// folderName validates a folder name: folders are told apart by name, so one
// is required.
func folderName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", fmt.Errorf("a folder needs a name")
	}
	if len([]rune(name)) > maxNameLength {
		name = string([]rune(name)[:maxNameLength])
	}
	return name, nil
}

// Folders returns every folder, by name.
func Folders(ctx context.Context, store core.CanvasStore) ([]Folder, error) {
	if err := ensureLoaded(ctx, store); err != nil {
		return nil, err
	}
	registry.mu.RLock()
	folders := make([]Folder, 0, len(registry.folders))
	for id, f := range registry.folders {
		folders = append(folders, toFolder(id, f))
	}
	registry.mu.RUnlock()
	sort.Slice(folders, func(i, j int) bool {
		return strings.ToLower(folders[i].Name) < strings.ToLower(folders[j].Name)
	})
	return folders, nil
}

// CreateFolder adds a folder. Others may only look at its boards until its
// author lets them edit.
func CreateFolder(ctx context.Context, store core.CanvasStore, name, author string) (Folder, error) {
	name, err := folderName(name)
	if err != nil {
		return Folder{}, err
	}
	if err := ensureLoaded(ctx, store); err != nil {
		return Folder{}, err
	}
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return Folder{}, err
	}
	id := hex.EncodeToString(raw)
	f := &folder{folderEntry: folderEntry{CreatedBy: author}, name: name, createdAt: time.Now()}
	if err := store.Save(ctx, f.canvas(id)); err != nil {
		return Folder{}, err
	}
	registry.mu.Lock()
	registry.folders[id] = f
	registry.mu.Unlock()
	return toFolder(id, f), nil
}

// MayEditFolder tells whether the account signed in as login may change the
// boards in a folder.
func MayEditFolder(ctx context.Context, store core.CanvasStore, login, folderID string) bool {
	if ensureLoaded(ctx, store) != nil {
		return false
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	return accountFor(login).mayEditLocked(folderID)
}

// FindFolder returns a folder by its id or, ignoring case, its name.
func FindFolder(ctx context.Context, store core.CanvasStore, ref string) (Folder, bool) {
	folders, err := Folders(ctx, store)
	if err != nil {
		return Folder{}, false
	}
	ref = strings.TrimSpace(ref)
	for _, f := range folders {
		if f.ID == ref || strings.EqualFold(f.Name, ref) {
			return f, true
		}
	}
	return Folder{}, false
}

// MoveBoard puts a board in a folder, or in none when folderID is empty.
func MoveBoard(ctx context.Context, store core.CanvasStore, id, folderID string) error {
	if err := ensureLoaded(ctx, store); err != nil {
		return err
	}
	registry.mu.RLock()
	existing := registry.records[id]
	_, folderExists := registry.folders[folderID]
	registry.mu.RUnlock()
	if existing == nil {
		return errNoBoard
	}
	if folderID != "" && !folderExists {
		return errNoFolder
	}
	if existing.Folder == folderID {
		return nil
	}

	next := *existing
	next.Folder = folderID
	if err := store.Save(ctx, next.canvas(id)); err != nil {
		return err
	}
	registry.mu.Lock()
	registry.records[id] = &next
	registry.mu.Unlock()
	return nil
}

var (
	errNoBoard  = fmt.Errorf("no such board")
	errNoFolder = fmt.Errorf("no such folder")
)

// HandleFolders lists the folders, with what the reader may do in each.
func HandleFolders(store core.CanvasStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		folders, err := Folders(r.Context(), store)
		if err != nil {
			unavailable(w, err)
			return
		}
		a := accountOf(r)
		registry.mu.RLock()
		for i := range folders {
			if f := registry.folders[folders[i].ID]; f != nil {
				folders[i] = a.folderView(folders[i].ID, f)
			}
		}
		registry.mu.RUnlock()
		render.JSON(w, r, folders)
	}
}

// managedFolder reads the folder a request names and checks that the request
// may manage it. On failure it has answered the request and returns nil.
func managedFolder(w http.ResponseWriter, r *http.Request, store core.CanvasStore) (string, *folder) {
	id := chi.URLParam(r, "id")
	if !folderIDPattern.MatchString(id) {
		http.Error(w, "invalid folder id", http.StatusBadRequest)
		return "", nil
	}
	if err := ensureLoaded(r.Context(), store); err != nil {
		unavailable(w, err)
		return "", nil
	}
	registry.mu.RLock()
	existing := registry.folders[id]
	registry.mu.RUnlock()
	if existing == nil {
		http.Error(w, "no such folder", http.StatusNotFound)
		return "", nil
	}
	if !accountOf(r).mayManageFolder(existing) {
		forbid(w, "only the folder's author or an organisation admin can change the folder")
		return "", nil
	}
	return id, existing
}

// saveFolder writes a changed folder and puts it in the list.
func saveFolder(ctx context.Context, store core.CanvasStore, id string, f *folder) error {
	if err := store.Save(ctx, f.canvas(id)); err != nil {
		return err
	}
	registry.mu.Lock()
	registry.folders[id] = f
	registry.mu.Unlock()
	return nil
}

type folderBody struct {
	Name string `json:"name"`
}

// HandleCreateFolder adds a folder.
func HandleCreateFolder(store core.CanvasStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body folderBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if _, err := folderName(body.Name); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f, err := CreateFolder(r.Context(), store, body.Name, author(r))
		if err != nil {
			logrus.WithError(err).Error("failed to create folder")
			http.Error(w, "failed to create folder", http.StatusInternalServerError)
			return
		}
		render.Status(r, http.StatusCreated)
		render.JSON(w, r, f)
	}
}

// HandleRenameFolder renames a folder.
func HandleRenameFolder(store core.CanvasStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body folderBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		name, err := folderName(body.Name)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id, existing := managedFolder(w, r, store)
		if existing == nil {
			return
		}

		next := *existing
		next.name = name
		if err := saveFolder(r.Context(), store, id, &next); err != nil {
			http.Error(w, "failed to rename folder", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// HandleFolderAccess sets what everybody but the folder's author and the
// administrators may do with its boards: {"access": "view"} or "edit".
func HandleFolderAccess(store core.CanvasStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Access string `json:"access"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		if body.Access != accessView && body.Access != accessEdit {
			http.Error(w, `access must be "view" or "edit"`, http.StatusBadRequest)
			return
		}
		id, existing := managedFolder(w, r, store)
		if existing == nil {
			return
		}

		next := *existing
		next.Access = body.Access
		if err := saveFolder(r.Context(), store, id, &next); err != nil {
			http.Error(w, "failed to change the folder's access", http.StatusInternalServerError)
			return
		}
		logrus.WithFields(logrus.Fields{"folder": id, "access": body.Access, "by": author(r)}).Info("folder access changed")
		render.JSON(w, r, accountOf(r).folderView(id, &next))
	}
}

// HandleDeleteFolder removes a folder. Its boards are not deleted: they move
// out of it, to the top of the list.
func HandleDeleteFolder(store core.CanvasStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		// Only those who manage the folder: its boards would come out of it
		// open to everybody.
		id, existing := managedFolder(w, r, store)
		if existing == nil {
			return
		}
		registry.mu.RLock()
		var inside []string
		for boardID, rec := range registry.records {
			if rec.Folder == id {
				inside = append(inside, boardID)
			}
		}
		registry.mu.RUnlock()

		// Boards first: if moving one fails, the folder is still there and
		// deleting it can be retried.
		for _, boardID := range inside {
			if err := MoveBoard(ctx, store, boardID, ""); err != nil && err != errNoBoard {
				logrus.WithError(err).WithField("board", boardID).Error("failed to move board out of folder")
				http.Error(w, "failed to move the folder's boards out", http.StatusInternalServerError)
				return
			}
		}
		if err := store.Delete(ctx, foldersOwner, id); err != nil {
			http.Error(w, "failed to delete folder", http.StatusInternalServerError)
			return
		}
		registry.mu.Lock()
		delete(registry.folders, id)
		registry.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}
}

// HandleMoveBoard puts a board in a folder: {"folder": "<id>"}, or "" for none.
// It takes the right to change boards both where the board is and where it
// goes.
func HandleMoveBoard(store core.CanvasStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Folder string `json:"folder"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid body", http.StatusBadRequest)
			return
		}
		id := chi.URLParam(r, "id")
		if err := ensureLoaded(r.Context(), store); err != nil {
			unavailable(w, err)
			return
		}
		a := accountOf(r)
		registry.mu.RLock()
		rec := registry.records[id]
		allowed := rec == nil || (a.mayEditLocked(rec.Folder) && a.mayEditLocked(body.Folder))
		registry.mu.RUnlock()
		if !allowed {
			forbid(w, "this folder is view-only for you")
			return
		}
		switch err := MoveBoard(r.Context(), store, id, body.Folder); err {
		case nil:
			w.WriteHeader(http.StatusNoContent)
		case errNoBoard:
			http.Error(w, err.Error(), http.StatusNotFound)
		case errNoFolder:
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			logrus.WithError(err).Error("failed to move board")
			http.Error(w, "failed to move board", http.StatusInternalServerError)
		}
	}
}
