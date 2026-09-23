package core

import (
	"context"
	"time"
)

type (
	// Canvas represents the metadata and content of a user-saved drawing.
	Canvas struct {
		ID        string    `json:"id"`
		UserID    string    `json:"-"` // Not exposed in JSON responses, used internally.
		Name      string    `json:"name"`
		Thumbnail string    `json:"thumbnail,omitempty"`
		Data      []byte    `json:"data,omitempty"` // The full canvas data, not included in list views.
		CreatedAt time.Time `json:"createdAt"`
		UpdatedAt time.Time `json:"updatedAt"`
	}

	// CanvasMeta is what a store can tell about a canvas without reading it.
	CanvasMeta struct {
		ID        string
		UpdatedAt time.Time
	}

	// CanvasMetaLister is implemented by stores that can list canvases without
	// reading each one. CanvasStore.List reads every object in full, which on a
	// remote object store costs a round trip per canvas.
	CanvasMetaLister interface {
		ListMeta(ctx context.Context, userID string) ([]CanvasMeta, error)
	}

	// CanvasStore defines the persistence layer for user-owned canvases.
	// All operations are scoped to a specific user.
	CanvasStore interface {
		// List returns metadata for all canvases owned by a user.
		// The returned Canvas objects should not contain the `Data` field to keep the response light.
		List(ctx context.Context, userID string) ([]*Canvas, error)

		// Get returns a single canvas by its ID, ensuring it belongs to the user.
		Get(ctx context.Context, userID, id string) (*Canvas, error)

		// Save creates or updates a canvas for a user.
		Save(ctx context.Context, canvas *Canvas) error

		// Delete removes a canvas, ensuring it belongs to the user.
		Delete(ctx context.Context, userID, id string) error
	}
)
