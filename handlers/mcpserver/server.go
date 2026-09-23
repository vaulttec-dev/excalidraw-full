// Package mcpserver serves the MCP tools for drawing on the team's boards,
// from the instance itself: Claude Code or claude.ai connects to /mcp, signs
// in through the instance's OAuth (handlers/oauth), and needs nothing installed.
//
// Being inside the instance, it can do what a separate client could not: it
// knows every listed board's key, and it sends its changes into the room the
// same way a collaborator's browser does, so people with the board open see
// them appear live.
package mcpserver

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"excalidraw-complete/core"
	"excalidraw-complete/handlers/api/boards"
	"excalidraw-complete/handlers/api/firebase"
	"excalidraw-complete/handlers/auth"
	"excalidraw-complete/middleware"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sirupsen/logrus"
)

// Broadcaster sends an encrypted update to everyone in a room, as the
// collaboration server relays a browser's changes ("client-broadcast").
type Broadcaster func(room string, ciphertext, iv []byte)

const instructions = `Tools for the team's Excalidraw whiteboard. Every board is shared: whatever is drawn here appears for everyone who has the board open, live, and stays in the board list under the given name.

Elements use a compact notation — only "type", "x" and "y" are required:
- type: rectangle | ellipse | diamond | text | arrow | line
- x, y: top-left corner in canvas pixels; y grows downwards. Leave ~40px between shapes.
- width, height: size of shapes; arrows and lines use them as the vector to the end point, or give "points" as [[0,0],[dx,dy],...] relative to x,y.
- label: {"text": "..."} on a shape or arrow puts centred text inside it — prefer this to separate text elements.
- text: the content of a text element; fontSize defaults to 20.
- strokeColor, backgroundColor (e.g. "#a5d8ff"), fillStyle ("solid", "hachure"), strokeStyle ("solid", "dashed"), roundness ({"type": 3} rounds corners).
- id: optional; give one when an arrow's startBinding/endBinding ({"elementId": "..."}) should point at it.

A board is addressed by its link, its id, or its name as shown by list_boards.`

type listInput struct{}

type createInput struct {
	Name     string    `json:"name" jsonschema:"board title shown in the team's board list"`
	Elements []Element `json:"elements" jsonschema:"elements in the compact notation described in the server instructions"`
}

type readInput struct {
	Board string `json:"board" jsonschema:"board link, id or name"`
}

type updateInput struct {
	Board    string    `json:"board" jsonschema:"board link, id or name"`
	Elements []Element `json:"elements" jsonschema:"elements in the compact notation; an element with the id of one already on the board replaces it"`
	Mode     string    `json:"mode,omitempty" jsonschema:"append (default) adds to the board; replace clears everything else first"`
}

var roomLink = regexp.MustCompile(`#room=([0-9a-f]{20}),([A-Za-z0-9_-]{22})`)
var roomID = regexp.MustCompile(`^[0-9a-f]{20}$`)

type tools struct {
	store     core.CanvasStore
	broadcast Broadcaster
	baseURL   string
	author    string
}

func textResult(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}}}
}

func (t *tools) link(id, key string) string {
	return fmt.Sprintf("%s/#room=%s,%s", t.baseURL, id, key)
}

// resolve finds a board by link, id or name.
func (t *tools) resolve(ctx context.Context, ref string) (id, key, name string, err error) {
	ref = strings.TrimSpace(ref)
	if m := roomLink.FindStringSubmatch(ref); m != nil {
		board, _ := boards.Find(ctx, t.store, m[1])
		return m[1], m[2], board.Name, nil
	}
	if roomID.MatchString(ref) {
		if board, ok := boards.Find(ctx, t.store, ref); ok {
			return board.ID, board.Key, board.Name, nil
		}
		return "", "", "", fmt.Errorf("no board with id %s in the list; use its link", ref)
	}

	list, err := boards.Snapshot(ctx, t.store)
	if err != nil {
		return "", "", "", err
	}
	var matches []boards.Board
	for _, b := range list {
		if strings.EqualFold(b.Name, ref) {
			return b.ID, b.Key, b.Name, nil
		}
		if ref != "" && strings.Contains(strings.ToLower(b.Name), strings.ToLower(ref)) {
			matches = append(matches, b)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].ID, matches[0].Key, matches[0].Name, nil
	case 0:
		return "", "", "", fmt.Errorf("no board named %q; call list_boards", ref)
	default:
		names := make([]string, len(matches))
		for i, b := range matches {
			names[i] = fmt.Sprintf("%q", b.Name)
		}
		return "", "", "", fmt.Errorf("%q matches several boards: %s", ref, strings.Join(names, ", "))
	}
}

func (t *tools) load(ctx context.Context, id, key string) ([]Element, error) {
	fields, ok := firebase.LoadScene(ctx, t.store, id)
	if !ok {
		return nil, nil
	}
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

func (t *tools) save(ctx context.Context, id, key string, elements []Element) error {
	plain, err := json.Marshal(elements)
	if err != nil {
		return err
	}
	fields, err := firebase.EncryptElements(key, plain, sceneVersion(elements))
	if err != nil {
		return err
	}
	return firebase.StoreScene(ctx, t.store, id, fields)
}

// announce sends changed elements to everyone with the board open. The editor
// reconciles them by version, as it does a collaborator's update.
func (t *tools) announce(id, key string, changed []Element) {
	if t.broadcast == nil || len(changed) == 0 {
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
	t.broadcast(id, ciphertext, iv)
}

func visible(elements []Element) []Element {
	out := make([]Element, 0, len(elements))
	for _, el := range elements {
		if deleted, _ := el["isDeleted"].(bool); !deleted {
			out = append(out, el)
		}
	}
	return out
}

func (t *tools) listBoards(ctx context.Context, _ *mcp.CallToolRequest, _ listInput) (*mcp.CallToolResult, any, error) {
	list, err := boards.Snapshot(ctx, t.store)
	if err != nil {
		return nil, nil, err
	}
	if len(list) == 0 {
		return textResult("No boards yet."), nil, nil
	}
	var b strings.Builder
	for _, board := range list {
		name := board.Name
		if name == "" {
			name = "(untitled)"
		}
		fmt.Fprintf(&b, "%s — by %s, edited %s\n%s\n\n", name, board.CreatedBy,
			board.EditedAt.Format(time.RFC3339), t.link(board.ID, board.Key))
	}
	return textResult("%s", strings.TrimSpace(b.String())), nil, nil
}

func (t *tools) createBoard(ctx context.Context, _ *mcp.CallToolRequest, in createInput) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.Name) == "" {
		return nil, nil, fmt.Errorf("name is required")
	}
	elements, err := buildScene(in.Elements, "")
	if err != nil {
		return nil, nil, err
	}

	idBytes := make([]byte, 10)
	keyBytes := make([]byte, 16)
	_, _ = rand.Read(idBytes)
	_, _ = rand.Read(keyBytes)
	id := hex.EncodeToString(idBytes)
	// The room key is a JWK "k" value: base64url of the raw 128 bit AES key.
	key := base64.RawURLEncoding.EncodeToString(keyBytes)

	if err := t.save(ctx, id, key, elements); err != nil {
		return nil, nil, err
	}
	if err := boards.Create(ctx, t.store, id, key, in.Name, t.author); err != nil {
		return nil, nil, err
	}
	return textResult("Board %q created with %d elements.\n%s", in.Name, len(elements), t.link(id, key)), nil, nil
}

func (t *tools) readBoard(ctx context.Context, _ *mcp.CallToolRequest, in readInput) (*mcp.CallToolResult, any, error) {
	id, key, name, err := t.resolve(ctx, in.Board)
	if err != nil {
		return nil, nil, err
	}
	elements, err := t.load(ctx, id, key)
	if err != nil {
		return nil, nil, err
	}
	shown := visible(elements)
	if len(shown) == 0 {
		return textResult("Board %q is empty.", name), nil, nil
	}
	data, _ := json.Marshal(shown)
	return textResult("Board %q, %d elements:\n%s", name, len(shown), data), nil, nil
}

func (t *tools) updateBoard(ctx context.Context, _ *mcp.CallToolRequest, in updateInput) (*mcp.CallToolResult, any, error) {
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	if mode == "" {
		mode = "append"
	}
	if mode != "append" && mode != "replace" {
		return nil, nil, fmt.Errorf("mode must be append or replace")
	}
	id, key, name, err := t.resolve(ctx, in.Board)
	if err != nil {
		return nil, nil, err
	}
	current, err := t.load(ctx, id, key)
	if err != nil {
		return nil, nil, err
	}
	added, err := buildScene(in.Elements, maxIndex(current))
	if err != nil {
		return nil, nil, err
	}

	byID := make(map[string]Element, len(current))
	for _, el := range current {
		byID[text(el["id"])] = el
	}
	replaced := make(map[string]bool)
	// An element given with the id of one already on the board supersedes it;
	// its version has to be higher for the editor to take it.
	for _, el := range added {
		if old, ok := byID[text(el["id"])]; ok {
			el["version"] = int(number(old["version"], 1)) + 1
			el["index"] = old["index"]
			replaced[text(el["id"])] = true
		}
	}

	var next, changed []Element
	for _, el := range current {
		if replaced[text(el["id"])] {
			continue
		}
		if deleted, _ := el["isDeleted"].(bool); mode == "replace" && !deleted {
			// Deleting is itself a change the editor has to win against the
			// copy in each open tab, hence a new version.
			el["isDeleted"] = true
			el["version"] = int(number(el["version"], 1)) + 1
			el["versionNonce"] = randomInt()
			el["updated"] = time.Now().UnixMilli()
			changed = append(changed, el)
		}
		next = append(next, el)
	}
	next = append(next, added...)
	changed = append(changed, added...)

	if err := t.save(ctx, id, key, next); err != nil {
		return nil, nil, err
	}
	t.announce(id, key, changed)
	return textResult("Board %q updated (%s): %d elements on it now.\n%s", name, mode, len(visible(next)), t.link(id, key)), nil, nil
}

// NewHandler serves the MCP endpoint. It is stateless, so a restart drops no
// session, and each request gets tools bound to its caller and origin.
func NewHandler(store core.CanvasStore, broadcast Broadcaster) http.Handler {
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		author := "Claude"
		if claims, ok := r.Context().Value(middleware.ClaimsContextKey).(*auth.AppClaims); ok && claims != nil && claims.Login != "" {
			author = claims.Login
		}
		t := &tools{store: store, broadcast: broadcast, baseURL: middleware.BaseURL(r), author: author}

		server := mcp.NewServer(&mcp.Implementation{Name: "excalidraw-team", Title: "Excalidraw команди", Version: "1.0.0"},
			&mcp.ServerOptions{Instructions: instructions})
		mcp.AddTool(server, &mcp.Tool{Name: "list_boards",
			Description: "List the team's boards, most recently edited first, with their links."}, t.listBoards)
		mcp.AddTool(server, &mcp.Tool{Name: "create_board",
			Description: "Draw a new board and add it to the team's board list under the given name. Returns its link."}, t.createBoard)
		mcp.AddTool(server, &mcp.Tool{Name: "read_board",
			Description: "Read the elements of a board, to inspect it or to edit it with update_board."}, t.readBoard)
		mcp.AddTool(server, &mcp.Tool{Name: "update_board",
			Description: "Add elements to a board, change elements on it (give their ids), or redraw it entirely with mode=replace. People with the board open see the change live."}, t.updateBoard)
		return server
	}, &mcp.StreamableHTTPOptions{Stateless: true})
}
