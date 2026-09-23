// Package history keeps past versions of boards.
//
// R2 has no object versioning, and a room is rewritten in place on every save,
// so without this a bad edit, a full redraw or an emptied bucket is final. A
// room's stored scene is copied to history/<room>/<unix ms> at most once per
// interval while it is being edited, and always right before anything that
// throws content away — a redraw, a restore, deleting the board — so that
// step can be undone too.
//
// Versions are the stored scenes as they are, still encrypted with the room's
// key; restoring one decrypts it with the key the board list holds.
package history

import (
	"context"
	"encoding/json"
	"excalidraw-complete/core"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
)

// interval is the least time between two automatic versions of a room.
const interval = 10 * time.Minute

func owner(room string) string { return "history/" + room }

var lastSnapshot sync.Map // room → time.Time

// Version is one stored version of a board.
type Version struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
}

// MaybeSnapshot records the scene just saved as a version, unless one was
// taken within the interval. It runs in the background: the save that
// triggered it has already been acknowledged.
func MaybeSnapshot(store core.CanvasStore, room string, fields interface{}) {
	now := time.Now()
	if last, ok := lastSnapshot.Load(room); ok && now.Sub(last.(time.Time)) < interval {
		return
	}
	lastSnapshot.Store(room, now)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := Snapshot(ctx, store, room, fields); err != nil {
			lastSnapshot.Delete(room)
			logrus.WithError(err).WithField("room", room).Warn("failed to record board version")
		}
	}()
}

// Snapshot records a version now.
func Snapshot(ctx context.Context, store core.CanvasStore, room string, fields interface{}) error {
	data, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	now := time.Now()
	lastSnapshot.Store(room, now)
	return store.Save(ctx, &core.Canvas{
		ID:        strconv.FormatInt(now.UnixMilli(), 10),
		UserID:    owner(room),
		Name:      room,
		Data:      data,
		CreatedAt: now,
	})
}

// List returns a room's versions, newest first.
func List(ctx context.Context, store core.CanvasStore, room string) ([]Version, error) {
	var ids []string
	if lister, ok := store.(core.CanvasMetaLister); ok {
		metas, err := lister.ListMeta(ctx, owner(room))
		if err != nil {
			return nil, err
		}
		for _, m := range metas {
			ids = append(ids, m.ID)
		}
	} else {
		canvases, err := store.List(ctx, owner(room))
		if err != nil {
			return nil, err
		}
		for _, c := range canvases {
			ids = append(ids, c.ID)
		}
	}

	versions := make([]Version, 0, len(ids))
	for _, id := range ids {
		ms, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			continue
		}
		versions = append(versions, Version{ID: id, At: time.UnixMilli(ms).UTC()})
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].At.After(versions[j].At) })
	return versions, nil
}

// Load returns a stored version's scene fields.
func Load(ctx context.Context, store core.CanvasStore, room, id string) (interface{}, error) {
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return nil, fmt.Errorf("invalid version id")
	}
	canvas, err := store.Get(ctx, owner(room), id)
	if err != nil {
		return nil, err
	}
	var fields interface{}
	if err := json.Unmarshal(canvas.Data, &fields); err != nil {
		return nil, err
	}
	return fields, nil
}

// Latest returns the newest version's scene fields, if there is one.
func Latest(ctx context.Context, store core.CanvasStore, room string) (interface{}, bool) {
	versions, err := List(ctx, store, room)
	if err != nil || len(versions) == 0 {
		return nil, false
	}
	fields, err := Load(ctx, store, room, versions[0].ID)
	return fields, err == nil
}
