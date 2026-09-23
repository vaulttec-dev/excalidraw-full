package main

import (
	"embed"
	_ "embed"
	"excalidraw-complete/handlers/api/boards"
	"excalidraw-complete/handlers/api/documents"
	"excalidraw-complete/handlers/api/firebase"
	"excalidraw-complete/handlers/api/me"
	"excalidraw-complete/handlers/api/scene"
	"excalidraw-complete/handlers/auth"
	"excalidraw-complete/handlers/mcpserver"
	"excalidraw-complete/handlers/oauth"
	authMiddleware "excalidraw-complete/middleware"
	"excalidraw-complete/stores"
	"flag"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/cors"
	"github.com/joho/godotenv"
	"github.com/sirupsen/logrus"
	"github.com/zishang520/engine.io/v2/types"
	"github.com/zishang520/engine.io/v2/utils"
	socketio "github.com/zishang520/socket.io/v2/socket"
)

//go:embed all:frontend
var assets embed.FS

// autoRoomScript puts every session into a collaboration room, which is what
// makes drawings land in the object store at all: the upstream editor has no
// accounts and keeps a plain canvas in the browser's localStorage, syncing to
// the backend only while a room is open.
//
// It runs before the editor's own bundle, so by the time that reads the hash the
// room is already there and no reload is needed. The key is generated in the
// browser. Every room opened here is registered in the shared board list (see
// handlers/api/boards), which is what lets the team find boards without passing
// links around. The last room is remembered so that reopening the instance
// returns to the same board; "/?new" starts another one.
const autoRoomScript = `<script>
(function () {
  var STORAGE_KEY = "excalidraw-self-host-room";
  var ROOM = /^#room=([0-9a-f]{20}),([A-Za-z0-9_-]{22})$/;

  // Any other hash is a shared drawing (#json=...): leave it alone.
  if (location.hash.length > 1 && !ROOM.test(location.hash)) return;

  var toHex = function (bytes) {
    return Array.prototype.map
      .call(bytes, function (b) { return ("0" + b.toString(16)).slice(-2); })
      .join("");
  };

  // The room key is a JWK "k" value: base64url of the raw 128 bit AES key.
  var toBase64Url = function (bytes) {
    var binary = String.fromCharCode.apply(null, bytes);
    return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  };

  if (!ROOM.test(location.hash)) {
    var saved = null;
    if (!/[?&]new(=|&|$)/.test(location.search)) {
      try { saved = localStorage.getItem(STORAGE_KEY); } catch (e) {}
    }
    location.hash = saved && ROOM.test(saved)
      ? saved
      : "#room=" + toHex(crypto.getRandomValues(new Uint8Array(10))) + "," +
        toBase64Url(crypto.getRandomValues(new Uint8Array(16)));
  }

  var room = ROOM.exec(location.hash);
  try { localStorage.setItem(STORAGE_KEY, location.hash); } catch (e) {}

  // Registering is idempotent: a board already listed keeps its name and author.
  fetch("/api/boards/" + room[1], {
    method: "PUT",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ key: room[2] }),
  }).then(function (response) {
    // 410: the board was deleted. Leave it rather than write it back into
    // storage; it can be restored from the trash.
    if (response.status === 410) {
      try { localStorage.removeItem(STORAGE_KEY); } catch (e) {}
      alert("Цю дошку видалено. Її можна відновити з кошика в панелі «Дошки».");
      location.replace("/?new");
    }
  }).catch(function () {});
})();
</script>`

func handleUI() http.HandlerFunc {
	sub, err := fs.Sub(assets, "frontend")
	if err != nil {
		panic(err)
	}

	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		// If the path is empty, it means it's the root, so serve index.html
		if path == "/" || path == "" {
			path = "/index.html"
		}

		// The editor ships a service worker that answers every navigation from
		// its cache, which would swallow the redirect to the login and leave the
		// app running against endpoints that all answer 401; a browser still
		// running the previous frontend's worker bounced between two sign-ins.
		// This worker replaces either, removes itself and its caches, and
		// reloads the open tabs onto the current frontend. The cost is the
		// offline mode, which an instance behind a login cannot offer anyway.
		if path == "/sw.js" || path == "/service-worker.js" {
			w.Header().Set("Content-Type", "application/javascript")
			w.Header().Set("Cache-Control", "no-store")
			_, _ = w.Write([]byte(serviceWorkerKillSwitch))
			return
		}

		// Check if the file exists in the embedded filesystem.
		f, err := sub.Open(strings.TrimPrefix(path, "/"))
		if err != nil {
			// If the file does not exist, and it's not a request for a static asset (like .js, .css),
			// then it's likely a client-side route. In that case, we should serve the index.html
			// and let the client-side router handle it.
			if os.IsNotExist(err) && !strings.Contains(path, ".") {
				path = "/index.html"
				f, err = sub.Open("index.html")
			} else {
				// It's a genuine 404 for a missing asset.
				http.NotFound(w, r)
				return
			}
		}

		if err != nil {
			// If we still have an error, something is wrong.
			http.Error(w, "File not found", http.StatusNotFound)
			return
		}
		defer f.Close()

		// The editor talks to Firestore; pointing it at this host makes it use
		// the Firestore shim in handlers/api/firebase instead.
		backendHost := os.Getenv("EXCALIDRAW_BACKEND_HOST")
		if backendHost == "" {
			backendHost = r.Host
		}

		// The rewrite below runs over a 2 MB bundle; do it once per file and
		// host rather than on every request.
		cacheKey := backendHost + path
		if cached, ok := servedFiles.Load(cacheKey); ok {
			serveFile(w, path, cached.(servedFile))
			return
		}

		fileContent, err := io.ReadAll(f)
		if err != nil {
			http.Error(w, "Error reading file", http.StatusInternalServerError)
			return
		}

		modifiedContent := strings.ReplaceAll(string(fileContent), "firestore.googleapis.com", backendHost)
		modifiedContent = strings.ReplaceAll(modifiedContent, "ssl=!0", "ssl=0")
		modifiedContent = strings.ReplaceAll(modifiedContent, "ssl:!0", "ssl:0")

		if strings.HasSuffix(path, ".html") {
			modifiedContent = strings.Replace(modifiedContent, "<head>", "<head>"+autoRoomScript, 1)
		}

		// Set the correct Content-Type based on the file extension
		contentType := http.DetectContentType([]byte(modifiedContent))
		switch {
		case strings.HasSuffix(path, ".js"):
			contentType = "application/javascript"
		case strings.HasSuffix(path, ".html"):
			contentType = "text/html"
		case strings.HasSuffix(path, ".css"):
			contentType = "text/css"
		case strings.HasSuffix(path, ".wasm"):
			contentType = "application/wasm"
		case strings.HasSuffix(path, ".webmanifest"):
			contentType = "application/manifest+json"
		case strings.HasSuffix(path, ".json"):
			contentType = "application/json"
		case strings.HasSuffix(path, ".svg"):
			contentType = "image/svg+xml"
		case strings.HasSuffix(path, ".png"):
			contentType = "image/png"
		case strings.HasSuffix(path, ".woff2"):
			contentType = "font/woff2"
		}

		file := servedFile{body: []byte(modifiedContent), contentType: contentType}
		servedFiles.Store(cacheKey, file)
		serveFile(w, path, file)
	}
}

// serviceWorkerKillSwitch reloads the open tabs only when there were caches to
// clear, i.e. when it replaced a worker that served pages from cache. The
// current frontend registers /sw.js on every load too; reloading
// unconditionally would put it in a reload loop.
const serviceWorkerKillSwitch = `self.addEventListener("install", () => self.skipWaiting());
self.addEventListener("activate", (event) => {
  event.waitUntil((async () => {
    await self.registration.unregister();
    const keys = await caches.keys();
    await Promise.all(keys.map((key) => caches.delete(key)));
    if (keys.length === 0) return;
    const windows = await self.clients.matchAll({ type: "window" });
    windows.forEach((client) => client.navigate(client.url));
  })());
});
`

// servedFile is a frontend file after the host rewrite, ready to send.
type servedFile struct {
	body        []byte
	contentType string
}

var servedFiles sync.Map

func serveFile(w http.ResponseWriter, path string, file servedFile) {
	w.Header().Set("Content-Type", file.contentType)
	w.Header().Set("Cache-Control", cacheControl(path))
	_, _ = w.Write(file.body)
}

// cacheControl lets browsers keep the frontend. Without it every page load —
// and switching boards used to be one — fetched the 3 MB of scripts again.
// Everything is private: the instance is behind a login.
func cacheControl(path string) string {
	switch {
	case strings.HasPrefix(path, "/assets/"):
		// Build output carries a content hash in its name, so a changed file
		// always arrives under a new name.
		return "private, max-age=31536000, immutable"
	case strings.HasSuffix(path, ".html"):
		// The page names the current bundles, so it is always revalidated.
		return "no-cache"
	default:
		return "private, max-age=86400"
	}
}

func setupRouter(store stores.Store) *chi.Mux {
	r := chi.NewRouter()
	r.Use(middleware.Logger)

	r.Use(cors.Handler(cors.Options{
		AllowedOrigins:   []string{"https://*", "http://*"},
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "Content-Length", "X-CSRF-Token", "Token", "session", "Origin", "Host", "Connection", "Accept-Encoding", "Accept-Language", "X-Requested-With"},
		AllowCredentials: true,
		MaxAge:           300, // Maximum value not ignored by any of major browsers
	}))

	// Everything below is gated behind a GitHub login once OAuth is configured:
	// the upstream editor has no accounts of its own, so without this the whole
	// instance — rooms included — would be open to anyone holding the URL.
	r.Use(authMiddleware.RequireSession)

	r.Route("/v1/projects/{project_id}/databases/{database_id}", func(r chi.Router) {
		r.Post("/documents:commit", firebase.HandleBatchCommit(store))
		r.Post("/documents:batchGet", firebase.HandleBatchGet(store))
	})

	r.Get(authMiddleware.HealthPath, boards.HandleHealth(store))

	// OAuth for MCP clients (see handlers/oauth). Some clients look the
	// metadata up with the resource path appended, so both forms are served.
	r.Get("/.well-known/oauth-protected-resource", oauth.HandleProtectedResource)
	r.Get("/.well-known/oauth-protected-resource/mcp", oauth.HandleProtectedResource)
	r.Get("/.well-known/oauth-authorization-server", oauth.HandleAuthorizationServer)
	r.Get("/.well-known/oauth-authorization-server/mcp", oauth.HandleAuthorizationServer)
	r.Post("/oauth/register", oauth.HandleRegister)
	r.Get("/oauth/authorize", oauth.HandleAuthorize)
	r.Post("/oauth/authorize", oauth.HandleAuthorize)
	r.Post("/oauth/token", oauth.HandleToken)
	r.Get("/api/me", me.HandleMe)

	// The shared board list. The editor shows it in its sidebar; the page is the
	// same list outside the editor.
	r.Get("/boards", boards.HandlePage)
	r.Route("/api/boards", func(r chi.Router) {
		r.Get("/", boards.HandleList(store))
		r.Put("/{id}", boards.HandlePut(store))
		r.Delete("/{id}", boards.HandleDelete(store))
		r.Get("/{id}/versions", boards.HandleVersions(store))
		r.Post("/{id}/versions/{version}/restore", boards.HandleRestoreVersion(store))
		r.Get("/trash", boards.HandleTrash(store))
		r.Post("/trash/{id}/restore", boards.HandleRestoreFromTrash(store))
	})

	// The editor's "Share → link" (#json=): an encrypted snapshot, not a room.
	r.Route("/api/v2", func(r chi.Router) {
		r.Post("/post/", documents.HandleCreate(store))
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", documents.HandleGet(store))
		})
	})

	r.Route("/auth", func(r chi.Router) {
		r.Get("/login", auth.HandleLogin)
		r.Get("/callback", auth.HandleCallback)
		r.Get("/logout", auth.HandleLogout)
		r.Get("/signed-out", auth.HandleSignedOut)
		r.Get("/denied", auth.HandleDenied)
	})

	return r
}

func setupSocketIO() *socketio.Server {
	opts := socketio.DefaultServerOptions()
	opts.SetMaxHttpBufferSize(5000000)
	opts.SetPath("/socket.io")
	opts.SetAllowEIO3(true)
	opts.SetCors(&types.Cors{
		Origin:      "*",
		Credentials: true,
	})
	ioo := socketio.NewServer(nil, opts)

	ioo.On("connection", func(clients ...any) {
		socket := clients[0].(*socketio.Socket)
		me := socket.Id()
		myRoom := socketio.Room(me)
		ioo.To(myRoom).Emit("init-room")
		utils.Log().Println("init room ", myRoom)
		socket.On("join-room", func(datas ...any) {
			room := socketio.Room(datas[0].(string))
			utils.Log().Printf("Socket %v has joined %v\n", me, room)
			socket.Join(room)
			ioo.In(room).FetchSockets()(func(usersInRoom []*socketio.RemoteSocket, _ error) {
				if len(usersInRoom) <= 1 {
					ioo.To(myRoom).Emit("first-in-room")
				} else {
					utils.Log().Printf("emit new user %v in room %v\n", me, room)
					socket.Broadcast().To(room).Emit("new-user", me)
				}

				// Inform all clients by new users.
				newRoomUsers := []socketio.SocketId{}
				for _, user := range usersInRoom {
					newRoomUsers = append(newRoomUsers, user.Id())
				}
				utils.Log().Println(" room ", room, " has users ", newRoomUsers)
				ioo.In(room).Emit(
					"room-user-change",
					newRoomUsers,
				)

			})
		})
		socket.On("server-broadcast", func(datas ...any) {
			roomID := datas[0].(string)
			utils.Log().Printf(" user %v sends update to room %v\n", me, roomID)
			socket.Broadcast().To(socketio.Room(roomID)).Emit("client-broadcast", datas[1], datas[2])
		})
		socket.On("server-volatile-broadcast", func(datas ...any) {
			roomID := datas[0].(string)
			utils.Log().Printf(" user %v sends volatile update to room %v\n", me, roomID)
			socket.Volatile().Broadcast().To(socketio.Room(roomID)).Emit("client-broadcast", datas[1], datas[2])
		})

		// Follow mode, as in excalidraw-room: followers of a socket share a room
		// named after it, and the followed client is told who is in it — that
		// is what makes it start sending its viewport.
		socket.On("user-follow", func(datas ...any) {
			payload, _ := datas[0].(map[string]any)
			target, _ := payload["userToFollow"].(map[string]any)
			targetID, _ := target["socketId"].(string)
			if targetID == "" {
				return
			}
			followRoom := socketio.Room("follow@" + targetID)
			switch payload["action"] {
			case "FOLLOW":
				socket.Join(followRoom)
			case "UNFOLLOW":
				socket.Leave(followRoom)
			default:
				return
			}
			announceFollowers(ioo, followRoom, socketio.SocketId(targetID))
		})
		socket.On("disconnecting", func(datas ...any) {
			for _, currentRoom := range socket.Rooms().Keys() {
				ioo.In(currentRoom).FetchSockets()(func(usersInRoom []*socketio.RemoteSocket, _ error) {
					otherClients := []socketio.SocketId{}
					for _, userInRoom := range usersInRoom {
						if userInRoom.Id() != me {
							otherClients = append(otherClients, userInRoom.Id())
						}
					}

					// A follower leaving: the followed client may stop sending
					// its viewport once nobody follows it.
					if followed, ok := strings.CutPrefix(string(currentRoom), "follow@"); ok {
						ioo.To(socketio.Room(followed)).Emit("user-follow-room-change", otherClients)
						return
					}

					if len(otherClients) > 0 {
						utils.Log().Printf("leaving user, room %v has users  %v\n", currentRoom, otherClients)
						ioo.In(currentRoom).Emit("room-user-change", otherClients)
					}
				})
			}
		})
		socket.On("disconnect", func(datas ...any) {
			socket.RemoveAllListeners("")
			socket.Disconnect(true)
		})
	})
	return ioo
}

// announceFollowers tells a followed client who follows it now.
func announceFollowers(ioo *socketio.Server, followRoom socketio.Room, followed socketio.SocketId) {
	ioo.In(followRoom).FetchSockets()(func(followers []*socketio.RemoteSocket, _ error) {
		ids := []socketio.SocketId{}
		for _, follower := range followers {
			ids = append(ids, follower.Id())
		}
		ioo.To(socketio.Room(followed)).Emit("user-follow-room-change", ids)
	})
}

func waitForShutdown(ioo *socketio.Server) {
	exit := make(chan struct{})
	SignalC := make(chan os.Signal, 1)

	signal.Notify(SignalC, os.Interrupt, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		for s := range SignalC {
			switch s {
			case os.Interrupt, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT:
				close(exit)
				return
			}
		}
	}()

	<-exit
	logrus.Info("shutting down")
	ioo.Close(nil)
	os.Exit(0)
}

func main() {
	// Load .env file
	if err := godotenv.Load(); err != nil {
		logrus.Info("No .env file found")
	}

	listenAddress := flag.String("listen", ":3002", "The address to listen on.")
	logLevel := flag.String("loglevel", "info", "The log level (debug, info, warn, error).")
	flag.Parse()

	level, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		logrus.Fatalf("Invalid log level: %v", err)
	}
	logrus.SetLevel(level)
	logrus.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})

	auth.InitAuth()
	store := stores.GetStore()
	boards.Preload(store)

	r := setupRouter(store)

	ioo := setupSocketIO()
	r.Mount("/socket.io/", ioo.ServeHandler(nil))

	// MCP for Claude. Its changes reach people with a board open through the
	// collaboration server, relayed like a browser's own updates.
	scene.SetBroadcaster(func(room string, ciphertext, iv []byte) {
		ioo.To(socketio.Room(room)).Emit("client-broadcast", ciphertext, iv)
	})
	r.Handle("/mcp", mcpserver.NewHandler(store))
	r.NotFound(handleUI())

	logrus.WithField("addr", *listenAddress).Info("starting server")
	go func() {
		if err := http.ListenAndServe(*listenAddress, r); err != nil {
			logrus.WithField("event", "start server").Fatal(err)
		}
	}()

	logrus.Debug("Server is running in the background")
	waitForShutdown(ioo)
}
