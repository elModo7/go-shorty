package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/process"
	"github.com/skip2/go-qrcode"
	"golang.org/x/time/rate"
	_ "modernc.org/sqlite"
)

const (
	version           = "1.2.1"
	defaultBaseURL    = "http://localhost:8080"
	defaultListenAddr = ":8080"
	defaultDBPath     = "shorty.db"
	codeLength        = 6
	maxCodeAttempts   = 10

	defaultRateLimitRPM = 60
	defaultRateBurst    = 12

	defaultAdminUsername = "admin"
	defaultAdminPassword = "admin"

	defaultAdminPageSize   = 10
	maxAdminPageSize       = 100
	defaultCleanupInterval = 5 * time.Minute
)

var (
	errCodeExists  = errors.New("code already exists")
	errCodeMiss    = errors.New("code not found")
	errCodeExpired = errors.New("code has expired")

	aliasPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{3,32}$`)
)

//go:embed templates/*.html
var templateFS embed.FS

type Link struct {
	Code       string     `json:"code"`
	URL        string     `json:"url"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	ClickCount int        `json:"click_count"`
}

func (l *Link) Expired(now time.Time) bool {
	return l.ExpiresAt != nil && !l.ExpiresAt.After(now.UTC())
}

type paginatedLinks struct {
	Links      []*Link `json:"links"`
	Page       int     `json:"page"`
	PageSize   int     `json:"page_size"`
	Total      int     `json:"total"`
	TotalPages int     `json:"total_pages"`
	Query      string  `json:"query"`
	Status     string  `json:"status"`
	Sort       string  `json:"sort"`
}

type listOptions struct {
	Page     int
	PageSize int
	Query    string
	Status   string
	Sort     string
}

type Store struct {
	mu    sync.RWMutex
	db    *sql.DB
	links map[string]*Link
}

func NewStore(ctx context.Context, dbPath string) (*Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}

	store := &Store{
		db:    db,
		links: make(map[string]*Link),
	}

	if err := store.init(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}

	return store, nil
}

func (s *Store) init(ctx context.Context) error {
	schema := `
CREATE TABLE IF NOT EXISTS links (
	code TEXT PRIMARY KEY,
	url TEXT NOT NULL,
	created_at TEXT NOT NULL,
	expires_at TEXT,
	click_count INTEGER NOT NULL DEFAULT 0
);`

	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}

	if err := s.ensureColumn(ctx, "links", "expires_at", "ALTER TABLE links ADD COLUMN expires_at TEXT"); err != nil {
		return err
	}

	rows, err := s.db.QueryContext(ctx, `
SELECT code, url, created_at, expires_at, click_count
FROM links
ORDER BY created_at DESC`)
	if err != nil {
		return fmt.Errorf("load links: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var link Link
		var createdAt string
		var expiresAt sql.NullString

		if err := rows.Scan(&link.Code, &link.URL, &createdAt, &expiresAt, &link.ClickCount); err != nil {
			return fmt.Errorf("scan link: %w", err)
		}

		link.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return fmt.Errorf("parse created_at for %s: %w", link.Code, err)
		}

		if expiresAt.Valid && strings.TrimSpace(expiresAt.String) != "" {
			parsed, err := time.Parse(time.RFC3339Nano, expiresAt.String)
			if err != nil {
				return fmt.Errorf("parse expires_at for %s: %w", link.Code, err)
			}
			link.ExpiresAt = &parsed
		}

		copied := link
		s.links[link.Code] = &copied
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate links: %w", err)
	}

	return nil
}

func (s *Store) ensureColumn(ctx context.Context, table, column, statement string) error {
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("inspect schema for %s: %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			cid        int
			name       string
			columnType string
			notNull    int
			defaultVal sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultVal, &primaryKey); err != nil {
			return fmt.Errorf("scan schema info: %w", err)
		}
		if name == column {
			return nil
		}
	}

	if _, err := s.db.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("migrate add %s.%s: %w", table, column, err)
	}

	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Save(ctx context.Context, link *Link) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.links[link.Code]; exists {
		return errCodeExists
	}

	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO links (code, url, created_at, expires_at, click_count) VALUES (?, ?, ?, ?, ?)`,
		link.Code,
		link.URL,
		link.CreatedAt.UTC().Format(time.RFC3339Nano),
		nullableTimeString(link.ExpiresAt),
		link.ClickCount,
	)
	if err != nil {
		if isUniqueConstraintError(err) {
			return errCodeExists
		}
		return fmt.Errorf("insert link: %w", err)
	}

	copied := *link
	s.links[link.Code] = &copied
	return nil
}

func (s *Store) Get(code string) (*Link, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	link, ok := s.links[code]
	if !ok {
		return nil, false
	}

	copied := *link
	return &copied, true
}

func (s *Store) Update(ctx context.Context, code, destination string, expiresAt *time.Time) (*Link, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	link, exists := s.links[code]
	if !exists {
		return nil, errCodeMiss
	}

	_, err := s.db.ExecContext(
		ctx,
		`UPDATE links SET url = ?, expires_at = ? WHERE code = ?`,
		destination,
		nullableTimeString(expiresAt),
		code,
	)
	if err != nil {
		return nil, fmt.Errorf("update link: %w", err)
	}

	link.URL = destination
	link.ExpiresAt = expiresAt

	copied := *link
	return &copied, nil
}

func (s *Store) Delete(ctx context.Context, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.links[code]; !exists {
		return errCodeMiss
	}

	result, err := s.db.ExecContext(ctx, `DELETE FROM links WHERE code = ?`, code)
	if err != nil {
		return fmt.Errorf("delete link: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check deleted rows: %w", err)
	}
	if affected == 0 {
		return errCodeMiss
	}

	delete(s.links, code)
	return nil
}

func (s *Store) DeleteExpired(ctx context.Context, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	nowUTC := now.UTC().Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `DELETE FROM links WHERE expires_at IS NOT NULL AND expires_at <= ?`, nowUTC)
	if err != nil {
		return 0, fmt.Errorf("delete expired links: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count deleted expired links: %w", err)
	}

	if affected == 0 {
		return 0, nil
	}

	for code, link := range s.links {
		if link.Expired(now) {
			delete(s.links, code)
		}
	}

	return int(affected), nil
}

func (s *Store) IncrementClick(ctx context.Context, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	link, ok := s.links[code]
	if !ok {
		return errCodeMiss
	}

	link.ClickCount++
	if _, err := s.db.ExecContext(ctx, `UPDATE links SET click_count = ? WHERE code = ?`, link.ClickCount, code); err != nil {
		link.ClickCount--
		return fmt.Errorf("update click count: %w", err)
	}

	return nil
}

func (s *Store) Exists(code string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, exists := s.links[code]
	return exists
}

func (s *Store) List() []*Link {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]*Link, 0, len(s.links))
	for _, link := range s.links {
		copied := *link
		out = append(out, &copied)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})

	return out
}

func (s *Store) ListPage(opts listOptions) paginatedLinks {
	all := s.List()
	filtered := filterLinks(all, opts)
	sortLinks(filtered, opts.Sort)

	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = defaultAdminPageSize
	}
	if pageSize > maxAdminPageSize {
		pageSize = maxAdminPageSize
	}

	page := opts.Page
	if page <= 0 {
		page = 1
	}

	total := len(filtered)
	totalPages := total / pageSize
	if total%pageSize != 0 {
		totalPages++
	}
	if totalPages == 0 {
		totalPages = 1
	}
	if page > totalPages {
		page = totalPages
	}

	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}

	return paginatedLinks{
		Links:      filtered[start:end],
		Page:       page,
		PageSize:   pageSize,
		Total:      total,
		TotalPages: totalPages,
		Query:      opts.Query,
		Status:     opts.Status,
		Sort:       opts.Sort,
	}
}

func filterLinks(links []*Link, opts listOptions) []*Link {
	out := make([]*Link, 0, len(links))
	query := strings.ToLower(strings.TrimSpace(opts.Query))
	now := time.Now()

	for _, link := range links {
		if query != "" {
			if !strings.Contains(strings.ToLower(link.Code), query) && !strings.Contains(strings.ToLower(link.URL), query) {
				continue
			}
		}

		switch strings.ToLower(strings.TrimSpace(opts.Status)) {
		case "", "all":
		case "active":
			if link.Expired(now) {
				continue
			}
		case "expired":
			if !link.Expired(now) {
				continue
			}
		}

		out = append(out, link)
	}

	return out
}

func sortLinks(links []*Link, sortKey string) {
	switch strings.ToLower(strings.TrimSpace(sortKey)) {
	case "clicks_asc":
		sort.Slice(links, func(i, j int) bool {
			if links[i].ClickCount == links[j].ClickCount {
				return links[i].CreatedAt.After(links[j].CreatedAt)
			}
			return links[i].ClickCount < links[j].ClickCount
		})
	case "created_asc":
		sort.Slice(links, func(i, j int) bool {
			return links[i].CreatedAt.Before(links[j].CreatedAt)
		})
	case "created_desc":
		sort.Slice(links, func(i, j int) bool {
			return links[i].CreatedAt.After(links[j].CreatedAt)
		})
	case "code_asc":
		sort.Slice(links, func(i, j int) bool {
			return strings.ToLower(links[i].Code) < strings.ToLower(links[j].Code)
		})
	case "code_desc":
		sort.Slice(links, func(i, j int) bool {
			return strings.ToLower(links[i].Code) > strings.ToLower(links[j].Code)
		})
	case "clicks_desc":
		fallthrough
	default:
		sort.Slice(links, func(i, j int) bool {
			if links[i].ClickCount == links[j].ClickCount {
				return links[i].CreatedAt.After(links[j].CreatedAt)
			}
			return links[i].ClickCount > links[j].ClickCount
		})
	}
}

type App struct {
	store           *Store
	baseURL         string
	uiTmpl          *template.Template
	adminTmpl       *template.Template
	proc            *process.Process
	adminUsername   string
	adminPassword   string
	cleanupInterval time.Duration
}

type shortenRequest struct {
	URL       string `json:"url"`
	Alias     string `json:"alias"`
	ExpiresAt string `json:"expires_at"`
	ExpiresIn string `json:"expires_in"`
}

type adminUpdateRequest struct {
	URL       string `json:"url"`
	ExpiresIn string `json:"expires_in"`
}

type shortenResponse struct {
	Code       string     `json:"code"`
	ShortURL   string     `json:"short_url"`
	URL        string     `json:"url"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	ClickCount int        `json:"click_count"`
	QRURL      string     `json:"qr_url"`
}

type pageData struct {
	BaseURL string
}

type adminPageData struct {
	BaseURL     string
	Version     string
	DefaultSize int
	AdminUser   string
}

type healthResponse struct {
	Status  string        `json:"status"`
	Message string        `json:"message"`
	Process processHealth `json:"process"`
	Runtime runtimeHealth `json:"runtime"`
	Time    string        `json:"time"`
	Version string        `json:"version"`
}

type processHealth struct {
	PID             int32   `json:"pid"`
	CPUPercent      float64 `json:"cpu_percent"`
	CPUTotalSeconds float64 `json:"cpu_total_seconds"`
	MemoryRSSBytes  uint64  `json:"memory_rss_bytes"`
	MemoryVMSBytes  uint64  `json:"memory_vms_bytes"`
}

type runtimeHealth struct {
	Goroutines     int    `json:"goroutines"`
	HeapAllocBytes uint64 `json:"heap_alloc_bytes"`
	SysBytes       uint64 `json:"sys_bytes"`
	NumGC          uint32 `json:"num_gc"`
}

type rateLimiters struct {
	mu       sync.Mutex
	visitors map[string]*visitor
	limit    rate.Limit
	burst    int
}

type visitor struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func main() {
	ctx := context.Background()

	baseURL := readEnv("BASE_URL", defaultBaseURL)
	listenAddr := readEnv("LISTEN_ADDR", defaultListenAddr)
	dbPath := readEnv("DB_PATH", defaultDBPath)
	adminUsername := readEnv("ADMIN_USERNAME", defaultAdminUsername)
	adminPassword := readEnv("ADMIN_PASSWORD", defaultAdminPassword)
	cleanupInterval := defaultCleanupInterval

	store, err := NewStore(ctx, dbPath)
	if err != nil {
		log.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			log.Printf("close store: %v", err)
		}
	}()

	uiTmpl, err := template.ParseFS(templateFS, "templates/index.html")
	if err != nil {
		log.Fatalf("parse frontend template: %v", err)
	}

	adminTmpl, err := template.ParseFS(templateFS, "templates/admin.html")
	if err != nil {
		log.Fatalf("parse admin template: %v", err)
	}

	proc, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		log.Fatalf("create process monitor: %v", err)
	}

	app := &App{
		store:           store,
		baseURL:         strings.TrimRight(baseURL, "/"),
		uiTmpl:          uiTmpl,
		adminTmpl:       adminTmpl,
		proc:            proc,
		adminUsername:   adminUsername,
		adminPassword:   adminPassword,
		cleanupInterval: cleanupInterval,
	}

	app.startExpiredCleanup(context.Background())

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", app.handleHome)
	mux.HandleFunc("GET /health", app.handleHealth)
	mux.HandleFunc("POST /shorten", app.handleShorten)
	mux.HandleFunc("GET /links", app.handleListLinks)
	mux.HandleFunc("DELETE /links/{code}", app.handleDeleteLink)
	mux.HandleFunc("GET /qr/{code}", app.handleQRCode)

	mux.Handle("GET /admin", app.adminAuth(http.HandlerFunc(app.handleAdminPage)))
	mux.Handle("GET /admin/api/links", app.adminAuth(http.HandlerFunc(app.handleAdminListLinks)))
	mux.Handle("DELETE /admin/api/links/{code}", app.adminAuth(http.HandlerFunc(app.handleAdminDeleteLink)))
	mux.Handle("PUT /admin/api/links/{code}", app.adminAuth(http.HandlerFunc(app.handleAdminUpdateLink)))

	mux.HandleFunc("GET /{code}", app.handleRedirect)

	limiters := newRateLimiters(defaultRateLimitRPM, defaultRateBurst)

	log.Printf("listening on %s with database %s", listenAddr, dbPath)
	if adminUsername == defaultAdminUsername && adminPassword == defaultAdminPassword {
		log.Printf("admin auth is using default credentials; set ADMIN_USERNAME and ADMIN_PASSWORD for anything beyond local use")
	}
	log.Fatal(http.ListenAndServe(listenAddr, loggingMiddleware(limiters.middleware(mux))))
}

func (a *App) startExpiredCleanup(ctx context.Context) {
	ticker := time.NewTicker(a.cleanupInterval)

	go func() {
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				deleted, err := a.store.DeleteExpired(context.Background(), time.Now())
				if err != nil {
					log.Printf("cleanup expired links: %v", err)
					continue
				}
				if deleted > 0 {
					log.Printf("cleanup expired links: deleted %d expired link(s)", deleted)
				}
			}
		}
	}()
}

func (a *App) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		a.handleRedirect(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.uiTmpl.Execute(w, pageData{BaseURL: a.baseURL}); err != nil {
		http.Error(w, "failed to render page", http.StatusInternalServerError)
		log.Printf("render home: %v", err)
	}
}

func (a *App) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.adminTmpl.Execute(w, adminPageData{
		BaseURL:     a.baseURL,
		Version:     version,
		DefaultSize: defaultAdminPageSize,
		AdminUser:   a.adminUsername,
	}); err != nil {
		http.Error(w, "failed to render admin page", http.StatusInternalServerError)
		log.Printf("render admin page: %v", err)
	}
}

func (a *App) handleAdminListLinks(w http.ResponseWriter, r *http.Request) {
	opts := listOptions{
		Page:     parsePositiveInt(r.URL.Query().Get("page"), 1),
		PageSize: parsePositiveInt(r.URL.Query().Get("page_size"), defaultAdminPageSize),
		Query:    strings.TrimSpace(r.URL.Query().Get("q")),
		Status:   strings.TrimSpace(r.URL.Query().Get("status")),
		Sort:     strings.TrimSpace(r.URL.Query().Get("sort")),
	}

	writeJSON(w, http.StatusOK, a.store.ListPage(opts))
}

func (a *App) handleAdminDeleteLink(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.PathValue("code"))
	if code == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing code",
		})
		return
	}

	if err := a.store.Delete(r.Context(), code); err != nil {
		status := http.StatusInternalServerError
		message := "failed to delete short URL"
		if errors.Is(err, errCodeMiss) {
			status = http.StatusNotFound
			message = "short URL not found"
		}
		writeJSON(w, status, map[string]string{
			"error": message,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status": "deleted",
		"code":   code,
	})
}

func (a *App) handleAdminUpdateLink(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.PathValue("code"))
	if code == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing code",
		})
		return
	}

	var req adminUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid JSON body",
		})
		return
	}

	req.URL = strings.TrimSpace(req.URL)
	req.ExpiresIn = strings.TrimSpace(req.ExpiresIn)

	if req.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "url is required",
		})
		return
	}

	if !isValidURL(req.URL) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid URL; must include http:// or https://",
		})
		return
	}

	expiresAt, err := parseExpiry("", req.ExpiresIn)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": err.Error(),
		})
		return
	}

	link, err := a.store.Update(r.Context(), code, req.URL, expiresAt)
	if err != nil {
		status := http.StatusInternalServerError
		message := "failed to update short URL"
		if errors.Is(err, errCodeMiss) {
			status = http.StatusNotFound
			message = "short URL not found"
		}
		writeJSON(w, status, map[string]string{
			"error": message,
		})
		return
	}

	writeJSON(w, http.StatusOK, shortenResponse{
		Code:       link.Code,
		ShortURL:   fmt.Sprintf("%s/%s", a.baseURL, link.Code),
		URL:        link.URL,
		CreatedAt:  link.CreatedAt,
		ExpiresAt:  link.ExpiresAt,
		ClickCount: link.ClickCount,
		QRURL:      fmt.Sprintf("%s/qr/%s", a.baseURL, link.Code),
	})
}

func (a *App) adminAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		if !ok || username != a.adminUsername || password != a.adminPassword {
			w.Header().Set("WWW-Authenticate", `Basic realm="go-shorty admin", charset="UTF-8"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "admin authentication required",
			})
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	snapshot, err := a.healthSnapshot(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"status":  "error",
			"message": err.Error(),
		})
		return
	}

	writeJSON(w, http.StatusOK, snapshot)
}

func (a *App) handleShorten(w http.ResponseWriter, r *http.Request) {
	var req shortenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid JSON body",
		})
		return
	}

	req.URL = strings.TrimSpace(req.URL)
	req.Alias = strings.TrimSpace(req.Alias)
	req.ExpiresAt = strings.TrimSpace(req.ExpiresAt)
	req.ExpiresIn = strings.TrimSpace(req.ExpiresIn)

	if req.URL == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "url is required",
		})
		return
	}

	if !isValidURL(req.URL) {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "invalid URL; must include http:// or https://",
		})
		return
	}

	expiresAt, err := parseExpiry(req.ExpiresAt, req.ExpiresIn)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": err.Error(),
		})
		return
	}

	code, status, err := a.resolveCode(req.Alias)
	if err != nil {
		writeJSON(w, status, map[string]string{
			"error": err.Error(),
		})
		return
	}

	link := &Link{
		Code:      code,
		URL:       req.URL,
		CreatedAt: time.Now().UTC(),
		ExpiresAt: expiresAt,
	}

	if err := a.store.Save(r.Context(), link); err != nil {
		status := http.StatusInternalServerError
		message := "failed to save shortened URL"
		if errors.Is(err, errCodeExists) {
			status = http.StatusConflict
			message = "short code already exists"
		}
		writeJSON(w, status, map[string]string{
			"error": message,
		})
		return
	}

	resp := shortenResponse{
		Code:       link.Code,
		ShortURL:   fmt.Sprintf("%s/%s", a.baseURL, link.Code),
		URL:        link.URL,
		CreatedAt:  link.CreatedAt,
		ExpiresAt:  link.ExpiresAt,
		ClickCount: link.ClickCount,
		QRURL:      fmt.Sprintf("%s/qr/%s", a.baseURL, link.Code),
	}

	writeJSON(w, http.StatusCreated, resp)
}

func (a *App) handleListLinks(w http.ResponseWriter, r *http.Request) {
	links := a.store.List()
	writeJSON(w, http.StatusOK, links)
}

func (a *App) handleDeleteLink(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.PathValue("code"))
	if code == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing code",
		})
		return
	}

	if err := a.store.Delete(r.Context(), code); err != nil {
		status := http.StatusInternalServerError
		message := "failed to delete short URL"
		if errors.Is(err, errCodeMiss) {
			status = http.StatusNotFound
			message = "short URL not found"
		}
		writeJSON(w, status, map[string]string{
			"error": message,
		})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{
		"status": "deleted",
		"code":   code,
	})
}

func (a *App) handleQRCode(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.PathValue("code"))
	if code == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "missing code",
		})
		return
	}

	link, ok := a.store.Get(code)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "short URL not found",
		})
		return
	}

	if link.Expired(time.Now()) {
		writeJSON(w, http.StatusGone, map[string]string{
			"error": "short URL has expired",
		})
		return
	}

	png, err := qrcode.Encode(fmt.Sprintf("%s/%s", a.baseURL, link.Code), qrcode.Medium, 256)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "failed to generate qr code",
		})
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(png)
}

func (a *App) handleRedirect(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimSpace(r.PathValue("code"))
	if code == "" || code == "favicon.ico" {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "short URL not found",
		})
		return
	}

	link, ok := a.store.Get(code)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "short URL not found",
		})
		return
	}

	if link.Expired(time.Now()) {
		writeJSON(w, http.StatusGone, map[string]string{
			"error": "short URL has expired",
		})
		return
	}

	if err := a.store.IncrementClick(r.Context(), code); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "failed to update click count",
		})
		return
	}

	http.Redirect(w, r, link.URL, http.StatusFound)
}

func (a *App) resolveCode(alias string) (string, int, error) {
	if alias != "" {
		if !isValidAlias(alias) {
			return "", http.StatusBadRequest, errors.New("alias must be 3-32 chars and use letters, numbers, hyphens, or underscores")
		}
		if a.store.Exists(alias) {
			return "", http.StatusConflict, errors.New("requested alias already exists")
		}
		return alias, http.StatusCreated, nil
	}

	for range maxCodeAttempts {
		code, err := generateCode(codeLength)
		if err != nil {
			return "", http.StatusInternalServerError, errors.New("failed to generate short code")
		}
		if !a.store.Exists(code) {
			return code, http.StatusCreated, nil
		}
	}

	return "", http.StatusInternalServerError, errors.New("failed to generate a unique short code after multiple attempts")
}

func (a *App) healthSnapshot(ctx context.Context) (*healthResponse, error) {
	memInfo, err := a.proc.MemoryInfoWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("read process memory info: %w", err)
	}

	cpuPercent, err := a.proc.CPUPercentWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("read process cpu percent: %w", err)
	}

	cpuTimes, err := a.proc.TimesWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("read process cpu times: %w", err)
	}

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	return &healthResponse{
		Status:  "ok",
		Message: "go-shorty is running",
		Version: version,
		Process: processHealth{
			PID:             a.proc.Pid,
			CPUPercent:      cpuPercent,
			CPUTotalSeconds: cpuTimes.Total(),
			MemoryRSSBytes:  memInfo.RSS,
			MemoryVMSBytes:  memInfo.VMS,
		},
		Runtime: runtimeHealth{
			Goroutines:     runtime.NumGoroutine(),
			HeapAllocBytes: memStats.HeapAlloc,
			SysBytes:       memStats.Sys,
			NumGC:          memStats.NumGC,
		},
		Time: time.Now().UTC().Format(time.RFC3339),
	}, nil
}

func isValidURL(raw string) bool {
	parsed, err := url.ParseRequestURI(raw)
	if err != nil {
		return false
	}
	return parsed.Scheme == "http" || parsed.Scheme == "https"
}

func isValidAlias(alias string) bool {
	return aliasPattern.MatchString(alias)
}

func parseExpiry(rawAt, rawIn string) (*time.Time, error) {
	if rawAt != "" && rawIn != "" {
		return nil, errors.New("provide either expires_at or expires_in, not both")
	}

	if rawIn != "" {
		duration, err := parseExpiryDuration(rawIn)
		if err != nil {
			return nil, err
		}
		expires := time.Now().UTC().Add(duration)
		return &expires, nil
	}

	if rawAt == "" {
		return nil, nil
	}

	layouts := []string{
		time.RFC3339,
		time.RFC3339Nano,
		"2006-01-02T15:04",
	}

	for _, layout := range layouts {
		if parsed, err := time.Parse(layout, rawAt); err == nil {
			expires := parsed.UTC()
			if !expires.After(time.Now().UTC()) {
				return nil, errors.New("expiration must be in the future")
			}
			return &expires, nil
		}
	}

	return nil, errors.New("invalid expiration format; use RFC3339 or datetime-local")
}

func parseExpiryDuration(raw string) (time.Duration, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1h":
		return time.Hour, nil
	case "1d", "24h":
		return 24 * time.Hour, nil
	case "1w", "7d":
		return 7 * 24 * time.Hour, nil
	case "1m", "30d":
		return 30 * 24 * time.Hour, nil
	case "1y", "365d":
		return 365 * 24 * time.Hour, nil
	case "", "never":
		return 0, nil
	default:
		return 0, errors.New("invalid expiration duration; use one of never, 1h, 1d, 1w, 1m, or 1y")
	}
}

func nullableTimeString(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func generateCode(length int) (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

	bytes := make([]byte, length)
	random := make([]byte, length)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}

	for i := range bytes {
		bytes[i] = alphabet[int(random[i])%len(alphabet)]
	}

	return string(bytes), nil
}

func isUniqueConstraintError(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "unique")
}

func readEnv(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func parsePositiveInt(raw string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("writeJSON error: %v", err)
	}
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		log.Printf("%s %s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
		log.Printf("completed in %s", time.Since(start))
	})
}

func newRateLimiters(requestsPerMinute, burst int) *rateLimiters {
	rl := &rateLimiters{
		visitors: make(map[string]*visitor),
		limit:    rate.Every(time.Minute / time.Duration(requestsPerMinute)),
		burst:    burst,
	}

	go rl.cleanup()

	return rl
}

func (rl *rateLimiters) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if !rl.allow(ip) {
			w.Header().Set("Retry-After", "60")
			writeJSON(w, http.StatusTooManyRequests, map[string]string{
				"error": "rate limit exceeded; please try again in a minute",
			})
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (rl *rateLimiters) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	entry, ok := rl.visitors[ip]
	if !ok {
		entry = &visitor{
			limiter:  rate.NewLimiter(rl.limit, rl.burst),
			lastSeen: time.Now(),
		}
		rl.visitors[ip] = entry
	}

	entry.lastSeen = time.Now()
	return entry.limiter.Allow()
}

func (rl *rateLimiters) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		threshold := time.Now().Add(-10 * time.Minute)

		rl.mu.Lock()
		for ip, entry := range rl.visitors {
			if entry.lastSeen.Before(threshold) {
				delete(rl.visitors, ip)
			}
		}
		rl.mu.Unlock()
	}
}

func clientIP(r *http.Request) string {
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		if len(parts) > 0 {
			return strings.TrimSpace(parts[0])
		}
	}

	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		return realIP
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}

	return r.RemoteAddr
}
