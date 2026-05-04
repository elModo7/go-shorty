<img src="https://github.com/user-attachments/assets/8bb4edb8-a291-456c-8ef1-bc9406f082e4" alt="go-shorty-mascot" width="200" height="200">

# Go-shorty

##### A lightweight URL shortener with custom aliases, redirect tracking, local persistence, minimal web UI, and a couple of operational extras like health reporting and Docker support.

## What it does

- Creates short links from a JSON API or from the web UI
- Supports custom aliases
- Handles collisions for both generated codes and user-provided aliases
- Persists data in SQLite so links survive restarts
- Tracks redirect count per short link
- Exposes a delete endpoint
- Ships with a simple frontend for creating links and checking high-level usage
- Includes a `/health` endpoint with process/runtime info
- Includes a Dockerfile for running the app in a container
- Rate limiting
- Expiration dates for short links
- QR generation
- Dedicated admin page for browsing, editing, searching, filtering, sorting, and deleting links

![go-shorty web UI](https://github.com/user-attachments/assets/19b4457e-4450-4aa3-8781-e6f34796a018)

## Running locally

```bash
go run .
```

By default the app starts on `http://localhost:8080`.

You can also override the main runtime settings with environment variables:

```bash
BASE_URL=http://localhost:8080
LISTEN_ADDR=:8080
DB_PATH=shorty.db
ADMIN_USERNAME=admin
ADMIN_PASSWORD=admin
```

## Admin panel

The app includes a dedicated admin panel at:

`GET /admin`

It is protected with HTTP Basic Auth.

By default, the credentials are:

- Username: `admin`
- Password: `admin`

For anything beyond local testing, override them with:

```bash
ADMIN_USERNAME=your-user
ADMIN_PASSWORD=your-strong-password
```

The admin page currently supports:

- paginated browsing
- search by alias or destination URL
- filter by active or expired links
- sorting by clicks, alias, or creation date
- copy short URL
- open/regenerate QR
- edit destination URL and expiration preset
- delete links

## Docker

Build the image:

```bash
docker build -t go-shorty .
```

Run it:

```bash
docker run --rm -p 8080:8080 -v shorty-data:/data go-shorty
```

The container stores the SQLite database at `/data/shorty.db`.

## API

### Create short link

`POST /shorten`

```json
{
  "url": "https://example.com/some-page",
  "alias": "example-page"
}
```

`alias` is optional. If it is not provided, the server generates a short code automatically.

Example response:

```json
{
  "code": "example-page",
  "short_url": "http://localhost:8080/example-page",
  "url": "https://example.com/some-page",
  "created_at": "2026-04-22T11:00:00Z",
  "click_count": 0
}
```

### List links

`GET /links`

Returns all stored links as JSON.

### Delete link

`DELETE /links/{code}`

Deletes a short link by code.

### Redirect

`GET /{code}`

Redirects to the original URL and increments the click counter.

### Health

`GET /health`

Returns basic service status plus process/runtime metrics such as CPU, memory, goroutines, GC count, and app version.

### Admin API

The admin page uses these protected endpoints:

- `GET /admin/api/links`
- `PUT /admin/api/links/{code}`
- `DELETE /admin/api/links/{code}`

## Project notes

A few implementation choices were intentional:

- The app keeps an in-memory map protected by a mutex for simple reads/writes during runtime
- SQLite is used as the persistence layer so the service still survives restarts
- The frontend stays intentionally small and dependency-free
- The health endpoint is useful for local monitoring without bringing in a full metrics stack

## Repo layout

```text
.
├── templates/
│   └── index.html
│   └── admin.html
├── Dockerfile
├── go.mod
├── main.go
└── README.md
```

## UI Credits

- **Particles author:** [@sebagarcia](https://www.sebagarcia.es/)
- **Inspired by:** *Particleground* by [Jonathan Nicol](https://github.com/jnicol)

## License

MIT
