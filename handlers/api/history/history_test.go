package history

import (
	"context"
	"testing"
	"time"

	"excalidraw-complete/stores/memory"
)

func TestSnapshotListPurge(t *testing.T) {
	ctx := context.Background()
	store := memory.NewStore()
	const room = "0123456789abcdef0123"

	for i := 0; i < 2; i++ {
		if err := Snapshot(ctx, store, room, map[string]any{"n": i}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // versions are named by the millisecond
	}
	if err := Snapshot(ctx, store, "another-room", map[string]any{}); err != nil {
		t.Fatal(err)
	}

	versions, err := List(ctx, store, room)
	if err != nil || len(versions) != 2 {
		t.Fatalf("List = %v, %v; want 2 versions", versions, err)
	}
	latest, ok := Latest(ctx, store, room)
	if !ok || latest.(map[string]any)["n"] != 1.0 {
		t.Fatalf("Latest = %v, want the newest version", latest)
	}

	if err := Purge(ctx, store, room); err != nil {
		t.Fatal(err)
	}
	if versions, _ := List(ctx, store, room); len(versions) != 0 {
		t.Fatalf("%d versions left after purge", len(versions))
	}
	if others, _ := List(ctx, store, "another-room"); len(others) != 1 {
		t.Fatal("purge touched another room")
	}
}
