package firebase

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"excalidraw-complete/core"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/render"
	"github.com/sirupsen/logrus"
)

type (
	BatchGetRequest struct {
		Documents []string `json:"documents"`
	}
	BatchGetEmptyResponse struct {
		Missing  string `json:"missing"`
		ReadTime string `json:"readTime"`
	}

	FoundInfoResponse struct {
		Name       string      `json:"name"`
		Fields     interface{} `json:"fields"`
		CreateTime string      `json:"createTime"`
		UpdateTime string      `json:"updateTime"`
	}
	BatchGetExistsResponse struct {
		Found    FoundInfoResponse `json:"found"`
		ReadTime string            `json:"readTime"`
	}

	UpdateRequest struct {
		Name   string      `json:"name"`
		Fields interface{} `json:"fields"`
	}
	WriteRequest struct {
		Update UpdateRequest `json:"update"`
	}
	BatchCommitRequest struct {
		Writes []WriteRequest `json:"writes"`
	}

	WriteResult struct {
		UpdateTime string `json:"updateTime"`
	}
	BatchCommitResponse struct {
		WriteResults []WriteResult `json:"writeResults"`
		CommitTime   string        `json:"commitTime"`
	}
)

// roomOwner is the storage namespace for live collaboration rooms. Rooms are
// not owned by a single account, so they live next to the per-user canvases
// under their own prefix.
const roomOwner = "rooms"

// RoomOwner is the namespace rooms are stored under, for callers that list them.
const RoomOwner = roomOwner

// DocumentPath is the Firestore document path the editor uses for a room. The
// project id is the one baked into the frontend build.
func DocumentPath(room string) string {
	return "projects/excalidraw-room-persistence/databases/(default)/documents/scenes/" + room
}

// StoreScene writes a room's encrypted scene in the shape the editor reads back.
func StoreScene(ctx context.Context, store core.CanvasStore, room string, fields interface{}) error {
	return saveRoomCtx(ctx, store, DocumentPath(room), fields)
}

// DeleteScene removes a room's stored scene.
func DeleteScene(ctx context.Context, store core.CanvasStore, room string) error {
	documentPath := DocumentPath(room)
	rooms.mu.Lock()
	delete(rooms.fields, documentPath)
	delete(rooms.edited, documentPath)
	rooms.mu.Unlock()
	return store.Delete(ctx, roomOwner, roomID(documentPath))
}

// StorageID is the id a room's scene is stored under.
func StorageID(room string) string {
	return roomID(DocumentPath(room))
}

// EditedAt reports when a room was last saved, as far as this process knows.
func EditedAt(room string) (time.Time, bool) {
	rooms.mu.RLock()
	defer rooms.mu.RUnlock()
	at, ok := rooms.edited[DocumentPath(room)]
	return at, ok
}

// SeedEditedAt records a room's last save as read from storage at startup. A
// save made since then is newer and is kept.
func SeedEditedAt(room string, at time.Time) {
	rooms.mu.Lock()
	defer rooms.mu.Unlock()
	documentPath := DocumentPath(room)
	if current, ok := rooms.edited[documentPath]; !ok || at.After(current) {
		rooms.edited[documentPath] = at
	}
}

// rooms caches every scene this process has read or written.
//
// The object store sits an ocean away from the server, so each round trip costs
// a few hundred milliseconds, and the editor reads a room every time a board is
// opened. The instance runs as a single process, so what it last wrote is the
// current state and reads can be served from memory; writes still go to the
// store before they are acknowledged. Without a store the cache is the storage.
var rooms = struct {
	mu     sync.RWMutex
	fields map[string]interface{}
	edited map[string]time.Time
}{
	fields: make(map[string]interface{}),
	edited: make(map[string]time.Time),
}

// roomID derives a flat, filesystem-safe id from the Firestore document path
// the frontend sends (".../documents/scenes/<room>"), which contains slashes
// and parentheses that the storage layer rejects.
func roomID(documentPath string) string {
	sum := sha256.Sum256([]byte(documentPath))
	return hex.EncodeToString(sum[:])
}

// LoadScene reads a room's stored scene fields.
func LoadScene(ctx context.Context, store core.CanvasStore, room string) (interface{}, bool) {
	return loadRoomCtx(ctx, store, DocumentPath(room))
}

// EncryptElements encrypts a scene's elements the way the editor does — AES-GCM
// with the room's 128 bit key and a fresh 12 byte IV — and returns them as the
// fields a room is stored with.
func EncryptElements(roomKey string, elements []byte, sceneVersion int) (map[string]any, error) {
	ciphertext, iv, err := Seal(roomKey, elements)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"sceneVersion": map[string]string{"integerValue": strconv.Itoa(sceneVersion)},
		"iv":           map[string]string{"bytesValue": base64.StdEncoding.EncodeToString(iv)},
		"ciphertext":   map[string]string{"bytesValue": base64.StdEncoding.EncodeToString(ciphertext)},
	}, nil
}

// DecryptElements reverses EncryptElements.
func DecryptElements(roomKey string, fields interface{}) ([]byte, error) {
	var stored struct {
		IV struct {
			BytesValue string `json:"bytesValue"`
		} `json:"iv"`
		Ciphertext struct {
			BytesValue string `json:"bytesValue"`
		} `json:"ciphertext"`
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &stored); err != nil {
		return nil, err
	}
	iv, err := base64.StdEncoding.DecodeString(stored.IV.BytesValue)
	if err != nil {
		return nil, err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(stored.Ciphertext.BytesValue)
	if err != nil {
		return nil, err
	}
	gcm, err := roomCipher(roomKey)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, iv, ciphertext, nil)
}

// Seal encrypts data with a room's key under a fresh IV, as the editor does for
// both stored scenes and live updates.
func Seal(roomKey string, data []byte) (ciphertext, iv []byte, err error) {
	gcm, err := roomCipher(roomKey)
	if err != nil {
		return nil, nil, err
	}
	iv = make([]byte, gcm.NonceSize())
	if _, err := rand.Read(iv); err != nil {
		return nil, nil, err
	}
	return gcm.Seal(nil, iv, data, nil), iv, nil
}

// roomCipher turns a room key — the JWK "k" value, base64url of a raw 128 bit
// AES key — into AES-GCM.
func roomCipher(roomKey string) (cipher.AEAD, error) {
	raw, err := base64.RawURLEncoding.DecodeString(roomKey)
	if err != nil {
		return nil, fmt.Errorf("invalid room key: %w", err)
	}
	block, err := aes.NewCipher(raw)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func loadRoom(r *http.Request, store core.CanvasStore, documentPath string) (interface{}, bool) {
	return loadRoomCtx(r.Context(), store, documentPath)
}

func loadRoomCtx(ctx context.Context, store core.CanvasStore, documentPath string) (interface{}, bool) {
	rooms.mu.RLock()
	fields, ok := rooms.fields[documentPath]
	rooms.mu.RUnlock()
	if ok || store == nil {
		return fields, ok
	}

	canvas, err := store.Get(ctx, roomOwner, roomID(documentPath))
	if err != nil || canvas == nil {
		return nil, false
	}

	if err := json.Unmarshal(canvas.Data, &fields); err != nil {
		logrus.WithError(err).WithField("room", documentPath).Warn("failed to decode stored room")
		return nil, false
	}

	rooms.mu.Lock()
	// A save may have landed while the store was being read; it is newer.
	if _, saved := rooms.fields[documentPath]; !saved {
		rooms.fields[documentPath] = fields
	}
	fields = rooms.fields[documentPath]
	rooms.mu.Unlock()
	return fields, true
}

func saveRoom(r *http.Request, store core.CanvasStore, documentPath string, fields interface{}) error {
	return saveRoomCtx(r.Context(), store, documentPath, fields)
}

func saveRoomCtx(ctx context.Context, store core.CanvasStore, documentPath string, fields interface{}) error {
	now := time.Now()

	if store != nil {
		data, err := json.Marshal(fields)
		if err != nil {
			return err
		}

		// A creation time given up front spares the store reading the object
		// back just to preserve it; nothing uses a room's creation time.
		if err := store.Save(ctx, &core.Canvas{
			ID:        roomID(documentPath),
			UserID:    roomOwner,
			Name:      documentPath,
			Data:      data,
			CreatedAt: now,
		}); err != nil {
			return err
		}
	}

	rooms.mu.Lock()
	rooms.fields[documentPath] = fields
	rooms.edited[documentPath] = now
	rooms.mu.Unlock()
	return nil
}

func (body *BatchGetRequest) Bind(r *http.Request) (err error) {
	return nil
}
func (body *BatchCommitRequest) Bind(r *http.Request) (err error) {
	return nil
}

func HandleBatchCommit(store core.CanvasStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projectId := chi.URLParam(r, "project_id")
		databaseId := chi.URLParam(r, "database_id")
		_ = projectId
		_ = databaseId

		data := &BatchCommitRequest{}
		// Seems like requests is text/plain but content is json ...
		if err := render.DecodeJSON(r.Body, data); err != nil {
			fmt.Println(err)
			render.Status(r, http.StatusBadRequest)
			return
		}

		if len(data.Writes) == 0 {
			render.Status(r, http.StatusBadRequest)
			render.JSON(w, r, map[string]string{"error": "no writes in request"})
			return
		}

		update := data.Writes[0].Update
		if err := saveRoom(r, store, update.Name, update.Fields); err != nil {
			logrus.WithError(err).WithField("room", update.Name).Error("failed to persist room")
			render.Status(r, http.StatusInternalServerError)
			render.JSON(w, r, map[string]string{"error": "failed to persist room"})
			return
		}

		render.JSON(w, r, BatchCommitResponse{
			CommitTime: time.Now().Format(time.RFC3339),
			WriteResults: []WriteResult{
				{UpdateTime: time.Now().Format(time.RFC3339)},
			},
		})
		render.Status(r, http.StatusOK)
	}
}

func HandleBatchGet(store core.CanvasStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		projectId := chi.URLParam(r, "project_id")
		databaseId := chi.URLParam(r, "database_id")
		_ = projectId
		_ = databaseId

		data := &BatchGetRequest{}

		// Seems like requests is text/plain but content is json ...
		if err := render.DecodeJSON(r.Body, data); err != nil {
			fmt.Println(err)
			render.Status(r, http.StatusBadRequest)
			return
		}

		if len(data.Documents) == 0 {
			render.Status(r, http.StatusBadRequest)
			render.JSON(w, r, map[string]string{"error": "no documents in request"})
			return
		}

		key := data.Documents[0]
		fields, ok := loadRoom(r, store, key)
		if !ok {
			render.JSON(w, r, []BatchGetEmptyResponse{{
				Missing:  key,
				ReadTime: time.Now().Format(time.RFC3339),
			}})
			render.Status(r, http.StatusOK)
			return
		}

		render.JSON(w, r, []BatchGetExistsResponse{{
			Found: FoundInfoResponse{
				Name:       key,
				Fields:     fields,
				CreateTime: time.Now().Format(time.RFC3339),
				UpdateTime: time.Now().Format(time.RFC3339),
			},
			ReadTime: time.Now().Format(time.RFC3339),
		}})
		render.Status(r, http.StatusOK)
	}
}
