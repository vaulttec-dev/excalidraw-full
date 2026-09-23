// Package scene reads and writes a board's elements on the server side, and
// sends changes to everyone who has the board open — what the MCP tools and
// restoring a version both need.
package scene

import (
	"context"
	"encoding/json"
	"excalidraw-complete/core"
	"excalidraw-complete/handlers/api/firebase"
	"fmt"
	"math/rand"
	"time"

	"github.com/sirupsen/logrus"
)

// Element is an Excalidraw element as the editor stores it.
type Element = map[string]any

// Broadcaster sends an encrypted update to everyone in a room, as the
// collaboration server relays a browser's changes ("client-broadcast").
type Broadcaster func(room string, ciphertext, iv []byte)

var broadcast Broadcaster

// SetBroadcaster wires in the collaboration server, which exists only once the
// router is built.
func SetBroadcaster(b Broadcaster) { broadcast = b }

// Decode decrypts stored scene fields into elements.
func Decode(key string, fields interface{}) ([]Element, error) {
	plain, err := firebase.DecryptElements(key, fields)
	if err != nil {
		return nil, fmt.Errorf("cannot read the board: wrong key or damaged scene")
	}
	var elements []Element
	if err := json.Unmarshal(plain, &elements); err != nil {
		return nil, err
	}
	return elements, nil
}

// Load returns a room's current elements; nil when nothing is stored yet.
func Load(ctx context.Context, store core.CanvasStore, room, key string) ([]Element, error) {
	fields, ok := firebase.LoadScene(ctx, store, room)
	if !ok {
		return nil, nil
	}
	return Decode(key, fields)
}

// Save stores a room's elements.
func Save(ctx context.Context, store core.CanvasStore, room, key string, elements []Element) error {
	plain, err := json.Marshal(elements)
	if err != nil {
		return err
	}
	fields, err := firebase.EncryptElements(key, plain, Version(elements))
	if err != nil {
		return err
	}
	return firebase.StoreScene(ctx, store, room, fields)
}

// Announce sends changed elements to everyone with the board open. The editor
// reconciles them by version, as it does a collaborator's update.
func Announce(room, key string, changed []Element) {
	if broadcast == nil || len(changed) == 0 {
		return
	}
	payload, err := json.Marshal(map[string]any{
		"type":    "SCENE_UPDATE",
		"payload": map[string]any{"elements": changed},
	})
	if err != nil {
		return
	}
	ciphertext, iv, err := firebase.Seal(key, payload)
	if err != nil {
		logrus.WithError(err).Warn("failed to encrypt live update")
		return
	}
	broadcast(room, ciphertext, iv)
}

// Version is the scene version the editor compares: the sum of its elements'.
func Version(elements []Element) int {
	total := 0
	for _, el := range elements {
		total += intOf(el["version"], 1)
	}
	return total
}

func intOf(v any, fallback int) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	}
	return fallback
}

func idOf(el Element) string {
	s, _ := el["id"].(string)
	return s
}

// Visible leaves out deleted elements.
func Visible(elements []Element) []Element {
	out := make([]Element, 0, len(elements))
	for _, el := range elements {
		if deleted, _ := el["isDeleted"].(bool); !deleted {
			out = append(out, el)
		}
	}
	return out
}

// ReplaceWith makes target the board's content: elements of current missing
// from target are deleted, the rest take target's state. Every change carries a
// version above the one in current, which is what makes an open tab accept it
// instead of keeping — and saving back — its own copy. It returns the next
// stored scene and the elements that changed.
func ReplaceWith(current, target []Element) (next, changed []Element) {
	byID := make(map[string]Element, len(current))
	for _, el := range current {
		byID[idOf(el)] = el
	}
	inTarget := make(map[string]bool, len(target))
	for _, el := range target {
		inTarget[idOf(el)] = true
		if old, ok := byID[idOf(el)]; ok {
			el["version"] = intOf(old["version"], 1) + 1
			el["versionNonce"] = rand.Intn(1 << 31)
			el["updated"] = time.Now().UnixMilli()
		}
	}

	for _, el := range current {
		if inTarget[idOf(el)] {
			continue
		}
		if deleted, _ := el["isDeleted"].(bool); !deleted {
			el["isDeleted"] = true
			el["version"] = intOf(el["version"], 1) + 1
			el["versionNonce"] = rand.Intn(1 << 31)
			el["updated"] = time.Now().UnixMilli()
			changed = append(changed, el)
		}
		next = append(next, el)
	}
	next = append(next, target...)
	changed = append(changed, target...)
	return next, changed
}
