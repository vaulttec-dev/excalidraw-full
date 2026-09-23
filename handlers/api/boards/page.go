package boards

import "net/http"

// HandlePage serves the shared board list. It is a page of its own rather than
// a panel inside the editor because the editor is upstream's build, left
// untouched; the editor links here through the button the served index adds.
func HandlePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(page))
}

const page = `<!doctype html>
<html lang="uk">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Дошки</title>
<style>
  :root {
    --bg: #f8f9fa; --surface: #fff; --text: #1b1b1f; --muted: #6b6b76;
    --border: #e4e4eb; --accent: #6965db; --accent-text: #fff; --danger: #d63c3c;
    --hover: #f1f0ff;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #121214; --surface: #1c1c21; --text: #ececf1; --muted: #9a9aa6;
      --border: #2c2c34; --accent: #a8a5ff; --accent-text: #121214; --danger: #ff7b7b;
      --hover: #25243a;
    }
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; background: var(--bg); color: var(--text);
    font: 15px/1.45 system-ui, -apple-system, "Segoe UI", Roboto, sans-serif;
  }
  main { max-width: 760px; margin: 0 auto; padding: 32px 16px 64px; }
  header { display: flex; align-items: center; gap: 12px; margin-bottom: 20px; }
  h1 { font-size: 24px; margin: 0; flex: 1; }
  button, .button {
    font: inherit; border-radius: 8px; border: 1px solid var(--border);
    background: var(--surface); color: var(--text); padding: 7px 14px; cursor: pointer;
    text-decoration: none; display: inline-flex; align-items: center; gap: 6px;
  }
  button:hover, .button:hover { background: var(--hover); }
  .primary { background: var(--accent); border-color: var(--accent); color: var(--accent-text); }
  .primary:hover { background: var(--accent); filter: brightness(1.08); }
  .create { display: none; gap: 8px; margin-bottom: 16px; }
  .create.open { display: flex; }
  input[type=text], input[type=search] {
    font: inherit; flex: 1; min-width: 0; padding: 8px 12px; border-radius: 8px;
    border: 1px solid var(--border); background: var(--surface); color: var(--text);
  }
  input:focus { outline: 2px solid var(--accent); outline-offset: -1px; }
  .search { width: 100%; margin-bottom: 12px; }
  ul { list-style: none; margin: 0; padding: 0; border: 1px solid var(--border);
       border-radius: 12px; background: var(--surface); overflow: hidden; }
  li { display: flex; align-items: center; gap: 12px; padding: 12px 16px;
       border-top: 1px solid var(--border); }
  li:first-child { border-top: 0; }
  li:hover { background: var(--hover); }
  .info { flex: 1; min-width: 0; }
  .name { color: var(--text); text-decoration: none; font-weight: 600; display: block;
          overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .name:hover { color: var(--accent); }
  .untitled { color: var(--muted); font-weight: 500; font-style: italic; }
  .meta { color: var(--muted); font-size: 13px; margin-top: 2px; }
  .actions { display: flex; gap: 4px; }
  .actions button { padding: 5px 9px; font-size: 13px; }
  .danger { color: var(--danger); }
  .empty, .error { padding: 40px 16px; text-align: center; color: var(--muted); }
  .error { color: var(--danger); }
  @media (max-width: 520px) {
    li { flex-wrap: wrap; }
    .actions { width: 100%; justify-content: flex-end; }
  }
</style>
</head>
<body>
<main>
  <header>
    <h1>Дошки</h1>
    <button class="primary" id="new">+ Нова дошка</button>
  </header>

  <form class="create" id="create">
    <input type="text" id="create-name" placeholder="Назва дошки" maxlength="120" autocomplete="off">
    <button class="primary" type="submit">Створити</button>
    <button type="button" id="create-cancel">Скасувати</button>
  </form>

  <input type="search" class="search" id="search" placeholder="Пошук за назвою або автором">
  <div id="list"><div class="empty">Завантаження…</div></div>
</main>

<script>
(function () {
  var STORAGE_KEY = "excalidraw-self-host-room";
  var listEl = document.getElementById("list");
  var searchEl = document.getElementById("search");
  var boards = [];

  var relative = new Intl.RelativeTimeFormat("uk", { numeric: "auto" });
  var ago = function (iso) {
    var seconds = (new Date(iso).getTime() - Date.now()) / 1000;
    var units = [["year", 31536000], ["month", 2592000], ["week", 604800],
                 ["day", 86400], ["hour", 3600], ["minute", 60]];
    for (var i = 0; i < units.length; i++) {
      if (Math.abs(seconds) >= units[i][1]) {
        return relative.format(Math.round(seconds / units[i][1]), units[i][0]);
      }
    }
    return "щойно";
  };

  var link = function (board) { return "/#room=" + board.id + "," + board.key; };

  var toHex = function (bytes) {
    return Array.prototype.map.call(bytes, function (b) { return ("0" + b.toString(16)).slice(-2); }).join("");
  };
  var toBase64Url = function (bytes) {
    return btoa(String.fromCharCode.apply(null, bytes)).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  };

  var request = function (method, path, body) {
    return fetch(path, {
      method: method,
      credentials: "same-origin",
      headers: body ? { "Content-Type": "application/json" } : {},
      body: body ? JSON.stringify(body) : undefined,
    }).then(function (response) {
      if (!response.ok) throw new Error(response.status + " " + response.statusText);
      return response;
    });
  };

  var render = function () {
    var query = searchEl.value.trim().toLowerCase();
    var shown = boards.filter(function (b) {
      return !query || (b.name || "").toLowerCase().indexOf(query) !== -1 ||
        (b.createdBy || "").toLowerCase().indexOf(query) !== -1;
    });

    listEl.textContent = "";
    if (!shown.length) {
      var empty = document.createElement("div");
      empty.className = "empty";
      empty.textContent = boards.length ? "Нічого не знайдено." : "Дошок ще немає — створіть першу.";
      listEl.appendChild(empty);
      return;
    }

    var ul = document.createElement("ul");
    shown.forEach(function (board) {
      var li = document.createElement("li");

      var info = document.createElement("div");
      info.className = "info";
      var a = document.createElement("a");
      a.className = "name" + (board.name ? "" : " untitled");
      a.href = link(board);
      a.textContent = board.name || "Без назви";
      a.addEventListener("click", function () {
        try { localStorage.setItem(STORAGE_KEY, "#room=" + board.id + "," + board.key); } catch (e) {}
      });
      var meta = document.createElement("div");
      meta.className = "meta";
      meta.textContent = "змінено " + ago(board.editedAt) + " · створив(ла) " + (board.createdBy || "—");
      info.appendChild(a);
      info.appendChild(meta);

      var actions = document.createElement("div");
      actions.className = "actions";

      var copy = document.createElement("button");
      copy.textContent = "Посилання";
      copy.title = "Скопіювати посилання на дошку";
      copy.addEventListener("click", function () {
        navigator.clipboard.writeText(location.origin + link(board)).then(function () {
          copy.textContent = "Скопійовано";
          setTimeout(function () { copy.textContent = "Посилання"; }, 1500);
        });
      });

      var rename = document.createElement("button");
      rename.textContent = "Перейменувати";
      rename.addEventListener("click", function () { startRename(li, board); });

      var remove = document.createElement("button");
      remove.className = "danger";
      remove.textContent = "Видалити";
      remove.addEventListener("click", function () {
        var label = board.name || "Без назви";
        if (!confirm("Видалити дошку «" + label + "» разом із вмістом для всієї команди?")) return;
        request("DELETE", "/api/boards/" + board.id).then(load, fail);
      });

      actions.appendChild(copy);
      actions.appendChild(rename);
      actions.appendChild(remove);
      li.appendChild(info);
      li.appendChild(actions);
      ul.appendChild(li);
    });
    listEl.appendChild(ul);
  };

  var startRename = function (li, board) {
    var form = document.createElement("form");
    form.style.display = "flex";
    form.style.gap = "8px";
    form.style.flex = "1";
    var input = document.createElement("input");
    input.type = "text";
    input.maxLength = 120;
    input.value = board.name || "";
    var save = document.createElement("button");
    save.className = "primary";
    save.type = "submit";
    save.textContent = "Зберегти";
    var cancel = document.createElement("button");
    cancel.type = "button";
    cancel.textContent = "Скасувати";
    cancel.addEventListener("click", render);
    form.appendChild(input);
    form.appendChild(save);
    form.appendChild(cancel);
    form.addEventListener("submit", function (event) {
      event.preventDefault();
      request("PUT", "/api/boards/" + board.id, { key: board.key, name: input.value }).then(load, fail);
    });
    li.textContent = "";
    li.appendChild(form);
    input.focus();
    input.select();
  };

  var fail = function (error) {
    alert("Не вдалося: " + error.message);
  };

  var load = function () {
    return request("GET", "/api/boards")
      .then(function (response) { return response.json(); })
      .then(function (data) { boards = data || []; render(); })
      .catch(function (error) {
        listEl.textContent = "";
        var el = document.createElement("div");
        el.className = "error";
        el.textContent = "Не вдалося завантажити список: " + error.message;
        listEl.appendChild(el);
      });
  };

  var createForm = document.getElementById("create");
  var createName = document.getElementById("create-name");
  document.getElementById("new").addEventListener("click", function () {
    createForm.classList.add("open");
    createName.focus();
  });
  document.getElementById("create-cancel").addEventListener("click", function () {
    createForm.classList.remove("open");
  });
  createForm.addEventListener("submit", function (event) {
    event.preventDefault();
    var id = toHex(crypto.getRandomValues(new Uint8Array(10)));
    var key = toBase64Url(crypto.getRandomValues(new Uint8Array(16)));
    request("PUT", "/api/boards/" + id, { key: key, name: createName.value }).then(function () {
      var room = "#room=" + id + "," + key;
      try { localStorage.setItem(STORAGE_KEY, room); } catch (e) {}
      location.href = "/" + room;
    }, fail);
  });

  searchEl.addEventListener("input", render);
  load();
})();
</script>
</body>
</html>
`
