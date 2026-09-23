# Excalidraw для команди

Офіційний редактор [Excalidraw](https://github.com/excalidraw/excalidraw) і власний Go-бекенд до нього:
спільні дошки, які зберігаються в S3-сумісному сховищі (у нас — Cloudflare R2), вхід через GitHub,
історія версій і кошик, а також MCP-сервер, через який Claude малює на дошках.

## Як це влаштовано

| Частина | Де | Що робить |
|---|---|---|
| Редактор | сабмодуль `excalidraw` → форк [`vaulttec-dev/excalidraw`](https://github.com/vaulttec-dev/excalidraw) | апстрім + панель «Дошки», меню акаунта, аватарки GitHub (`excalidraw-app/selfhost/`) |
| Кімнати | `handlers/api/firebase` | REST-шим Firestore: редактор зберігає кімнату в «Firestore», а насправді — у сховищі. `main.go` при роздачі бандла підміняє `firestore.googleapis.com` на хост інстансу |
| Live-співпраця | `main.go` (socket.io) | протокол `excalidraw-room`, разом із режимом «стежити» |
| Дошки | `handlers/api/boards` | спільний список: кожна дошка — кімната з ключем і назвою; версії (`handlers/api/history`) і кошик |
| Вхід | `middleware/session.go`, `handlers/auth` | увесь інстанс за GitHub OAuth; доступ — логіни або організації |
| MCP | `handlers/mcpserver`, `handlers/oauth` | `/mcp` (streamable HTTP) з OAuth-входом через GitHub: `list_boards`, `create_board`, `read_board`, `update_board` |
| «Поділитися → посилання» | `handlers/api/documents` | зашифровані знімки `#json=` |

Кожна відкрита вкладка одразу потрапляє в кімнату (скрипт `autoRoomScript` у `main.go`), тому все
намальоване опиняється у сховищі, а не лише в `localStorage` браузера. `/?new` відкриває нову дошку.

Сервер тримає ключі кімнат, отже може читати дошки. Інакше колега не відкрив би дошку, яку створив хтось інший.

Реєстр дошок і кімнати кешуються в пам'яті процесу (write-through). **Запускайте лише одну репліку.**

## Змінні оточення

| Змінна | Значення |
|---|---|
| `GITHUB_CLIENT_ID`, `GITHUB_CLIENT_SECRET` | GitHub OAuth App. Без них вхід вимкнено й інстанс відкритий (лише для локального запуску) |
| `GITHUB_REDIRECT_URL` | `https://<хост>/auth/callback` |
| `JWT_SECRET` | ключ підпису сесій, OAuth-токенів MCP і client id |
| `ALLOWED_GITHUB_ORGS` | організації, активні учасники яких мають доступ (через кому) |
| `ALLOWED_GITHUB_LOGINS` | окремі логіни поза організаціями; `*` — будь-хто. Обидві змінні порожні — не пускати нікого |
| `STORAGE_TYPE` | `s3` (прод), `sqlite`, `filesystem` або порожньо (пам'ять) |
| `S3_BUCKET_NAME`, `AWS_REGION`, `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_ENDPOINT_URL` | для `s3`; для R2 — `AWS_REGION=auto`, `AWS_ENDPOINT_URL=https://<account>.r2.cloudflarestorage.com` |
| `DATA_SOURCE_NAME` / `LOCAL_STORAGE_PATH` | для `sqlite` / `filesystem` |

Розкладка сховища: `rooms/` — сцени кімнат, `boards/` — реєстр, `history/<room>/` — версії, `trash/` — видалені дошки,
у корені бакета за id — посилання «Поділитися».

## Збірка й деплой

```sh
git submodule update --init
docker build -f excalidraw-complete.Dockerfile -t ghcr.io/vaulttec-dev/excalidraw-full:$(git rev-parse --short HEAD) .
docker push ghcr.io/vaulttec-dev/excalidraw-full:$(git rev-parse --short HEAD)
```

Далі в Coolify змінити тег образу в compose сервісу й перезапустити. Змінні задаються в Coolify
(Environment Variables → Developer view), у compose лише посилання `${…}`. Контейнер має healthcheck на `/healthz`.

Локально без сховища й входу: `docker run -p 3002:3002 <образ>` і відкрити http://localhost:3002.

## Оновлення редактора з апстріму

1. На GitHub у `vaulttec-dev/excalidraw` натиснути **Sync fork**.
2. У сабмодулі: `git pull --rebase` (власні коміти лягають поверх апстріму), `git push --force-with-lease`.
3. У цьому репозиторії закомітити новий коміт сабмодуля, зібрати й задеплоїти образ.

Власні зміни у форку тримаються в `excalidraw-app/selfhost/` і кількох точкових правках, щоб rebase був безконфліктним.

## MCP

Підключення (Claude Code):

```sh
claude mcp add --transport http --scope user excalidraw https://<хост>/mcp
```

Далі `/mcp` → excalidraw → Authenticate: відкриється вхід через GitHub і сторінка згоди. Для команди конфігурація
поширюється через `eloicompany/claude-config` (`global/mcp.json`).

## Тести

`go test ./...` ганяє CI (`.github/workflows/ci.yml`) разом із `gofmt` і `go vet`.
