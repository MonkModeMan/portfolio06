package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type app struct {
	db            *pgxpool.Pool
	client        *http.Client
	checkInterval time.Duration
}

type service struct {
	ID             int64      `json:"id"`
	Name           string     `json:"name"`
	URL            string     `json:"url"`
	ExpectedStatus int        `json:"expected_status"`
	Enabled        bool       `json:"enabled"`
	LastStatus     *int       `json:"last_status"`
	LastLatencyMS  *int       `json:"last_latency_ms"`
	LastCheckedAt  *time.Time `json:"last_checked_at"`
	LastOK         *bool      `json:"last_ok"`
	CreatedAt      time.Time  `json:"created_at"`
}

type checkResult struct {
	ID          int64     `json:"id"`
	ServiceID   int64     `json:"service_id"`
	StatusCode  *int      `json:"status_code"`
	LatencyMS   int       `json:"latency_ms"`
	OK          bool      `json:"ok"`
	Error       string    `json:"error"`
	CheckedAt   time.Time `json:"checked_at"`
	ServiceName string    `json:"service_name,omitempty"`
	ServiceURL  string    `json:"service_url,omitempty"`
}

func main() {
	ctx := context.Background()
	databaseURL := env("DATABASE_URL", "postgres://monitor:monitor@localhost:5432/monitor?sslmode=disable")
	port := env("PORT", "8080")

	db, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		log.Fatalf("connect database: %v", err)
	}
	defer db.Close()

	if err := waitForDatabase(ctx, db); err != nil {
		log.Fatalf("database unavailable: %v", err)
	}
	if err := migrate(ctx, db); err != nil {
		log.Fatalf("migrate database: %v", err)
	}
	if err := seed(ctx, db); err != nil {
		log.Fatalf("seed database: %v", err)
	}

	timeout := durationFromEnv("REQUEST_TIMEOUT_SECONDS", 10) * time.Second
	a := &app{
		db:            db,
		client:        &http.Client{Timeout: timeout},
		checkInterval: durationFromEnv("CHECK_INTERVAL_SECONDS", 60) * time.Second,
	}

	go a.monitorLoop(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.health)
	mux.HandleFunc("GET /api/services", a.listServices)
	mux.HandleFunc("POST /api/services", a.createService)
	mux.HandleFunc("DELETE /api/services/{id}", a.deleteService)
	mux.HandleFunc("POST /api/services/{id}/check", a.runCheckNow)
	mux.HandleFunc("GET /api/checks", a.listChecks)
	mux.HandleFunc("GET /api/stats", a.stats)
	mux.Handle("/", staticHandler("web"))

	log.Printf("portfolio06 listening on http://localhost:%s", port)
	if err := http.ListenAndServe(":"+port, withCORS(mux)); err != nil {
		log.Fatal(err)
	}
}

func waitForDatabase(ctx context.Context, db *pgxpool.Pool) error {
	deadline := time.Now().Add(60 * time.Second)
	for {
		if err := db.Ping(ctx); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for database")
		}
		time.Sleep(time.Second)
	}
}

func migrate(ctx context.Context, db *pgxpool.Pool) error {
	_, err := db.Exec(ctx, `
CREATE TABLE IF NOT EXISTS services (
	id BIGSERIAL PRIMARY KEY,
	name TEXT NOT NULL,
	url TEXT NOT NULL UNIQUE,
	expected_status INTEGER NOT NULL DEFAULT 200,
	enabled BOOLEAN NOT NULL DEFAULT TRUE,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS checks (
	id BIGSERIAL PRIMARY KEY,
	service_id BIGINT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
	status_code INTEGER,
	latency_ms INTEGER NOT NULL,
	ok BOOLEAN NOT NULL,
	error TEXT NOT NULL DEFAULT '',
	checked_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_checks_service_checked_at ON checks(service_id, checked_at DESC);
CREATE INDEX IF NOT EXISTS idx_checks_checked_at ON checks(checked_at DESC);
`)
	return err
}

func seed(ctx context.Context, db *pgxpool.Pool) error {
	var count int64
	if err := db.QueryRow(ctx, `SELECT COUNT(*) FROM services`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	_, err := db.Exec(ctx, `
INSERT INTO services (name, url, expected_status) VALUES
	('Google', 'https://www.google.com', 200),
	('GitHub', 'https://github.com', 200),
	('Example', 'https://example.com', 200)
ON CONFLICT (url) DO NOTHING;
`)
	return err
}

func (a *app) monitorLoop(ctx context.Context) {
	ticker := time.NewTicker(a.checkInterval)
	defer ticker.Stop()

	a.checkEnabledServices(ctx)
	for range ticker.C {
		a.checkEnabledServices(ctx)
	}
}

func (a *app) checkEnabledServices(ctx context.Context) {
	rows, err := a.db.Query(ctx, `SELECT id, name, url, expected_status FROM services WHERE enabled = TRUE ORDER BY id`)
	if err != nil {
		log.Printf("load services: %v", err)
		return
	}
	defer rows.Close()

	for rows.Next() {
		var svc service
		if err := rows.Scan(&svc.ID, &svc.Name, &svc.URL, &svc.ExpectedStatus); err != nil {
			log.Printf("scan service: %v", err)
			continue
		}
		if _, err := a.performCheck(ctx, svc); err != nil {
			log.Printf("check %s: %v", svc.URL, err)
		}
	}
}

func (a *app) performCheck(ctx context.Context, svc service) (checkResult, error) {
	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, svc.URL, nil)
	if err != nil {
		return checkResult{}, err
	}
	req.Header.Set("User-Agent", "portfolio06-monitor/1.0")

	resp, err := a.client.Do(req)
	latency := int(time.Since(started).Milliseconds())
	result := checkResult{
		ServiceID: svc.ID,
		LatencyMS: latency,
		CheckedAt: time.Now(),
	}
	if err != nil {
		result.OK = false
		result.Error = err.Error()
	} else {
		defer resp.Body.Close()
		result.StatusCode = &resp.StatusCode
		result.OK = resp.StatusCode == svc.ExpectedStatus
	}

	err = a.db.QueryRow(ctx, `
INSERT INTO checks (service_id, status_code, latency_ms, ok, error)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, checked_at
`, result.ServiceID, result.StatusCode, result.LatencyMS, result.OK, result.Error).Scan(&result.ID, &result.CheckedAt)

	return result, err
}

func (a *app) health(w http.ResponseWriter, r *http.Request) {
	if err := a.db.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database is not available")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *app) listServices(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(r.Context(), `
SELECT
	s.id,
	s.name,
	s.url,
	s.expected_status,
	s.enabled,
	c.status_code,
	c.latency_ms,
	c.checked_at,
	c.ok,
	s.created_at
FROM services s
LEFT JOIN LATERAL (
	SELECT status_code, latency_ms, checked_at, ok
	FROM checks
	WHERE service_id = s.id
	ORDER BY checked_at DESC
	LIMIT 1
) c ON TRUE
ORDER BY s.created_at DESC
`)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	services := []service{}
	for rows.Next() {
		var svc service
		var status sql.NullInt32
		var latency sql.NullInt32
		var checkedAt sql.NullTime
		var ok sql.NullBool
		if err := rows.Scan(&svc.ID, &svc.Name, &svc.URL, &svc.ExpectedStatus, &svc.Enabled, &status, &latency, &checkedAt, &ok, &svc.CreatedAt); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if status.Valid {
			value := int(status.Int32)
			svc.LastStatus = &value
		}
		if latency.Valid {
			value := int(latency.Int32)
			svc.LastLatencyMS = &value
		}
		if checkedAt.Valid {
			svc.LastCheckedAt = &checkedAt.Time
		}
		if ok.Valid {
			svc.LastOK = &ok.Bool
		}
		services = append(services, svc)
	}
	writeJSON(w, http.StatusOK, services)
}

func (a *app) createService(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name           string `json:"name"`
		URL            string `json:"url"`
		ExpectedStatus int    `json:"expected_status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	input.URL = strings.TrimSpace(input.URL)
	if input.ExpectedStatus == 0 {
		input.ExpectedStatus = 200
	}
	if input.Name == "" || input.URL == "" {
		writeError(w, http.StatusBadRequest, "name and url are required")
		return
	}
	parsed, err := url.ParseRequestURI(input.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		writeError(w, http.StatusBadRequest, "url must start with http:// or https://")
		return
	}

	var svc service
	err = a.db.QueryRow(r.Context(), `
INSERT INTO services (name, url, expected_status)
VALUES ($1, $2, $3)
RETURNING id, name, url, expected_status, enabled, created_at
`, input.Name, input.URL, input.ExpectedStatus).Scan(&svc.ID, &svc.Name, &svc.URL, &svc.ExpectedStatus, &svc.Enabled, &svc.CreatedAt)
	if err != nil {
		writeError(w, http.StatusConflict, "url already exists or could not be created")
		return
	}
	writeJSON(w, http.StatusCreated, svc)
}

func (a *app) deleteService(w http.ResponseWriter, r *http.Request) {
	id, err := idFromPath(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	tag, err := a.db.Exec(r.Context(), `DELETE FROM services WHERE id = $1`, id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tag.RowsAffected() == 0 {
		writeError(w, http.StatusNotFound, "service not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (a *app) runCheckNow(w http.ResponseWriter, r *http.Request) {
	id, err := idFromPath(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	var svc service
	err = a.db.QueryRow(r.Context(), `SELECT id, name, url, expected_status FROM services WHERE id = $1`, id).Scan(&svc.ID, &svc.Name, &svc.URL, &svc.ExpectedStatus)
	if err != nil {
		writeError(w, http.StatusNotFound, "service not found")
		return
	}
	result, err := a.performCheck(r.Context(), svc)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (a *app) listChecks(w http.ResponseWriter, r *http.Request) {
	var serviceID int64
	hasServiceID := false
	if raw := r.URL.Query().Get("service_id"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid service_id")
			return
		}
		serviceID = value
		hasServiceID = true
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if value, err := strconv.Atoi(raw); err == nil && value > 0 && value <= 200 {
			limit = value
		}
	}

	query := `
SELECT c.id, c.service_id, c.status_code, c.latency_ms, c.ok, c.error, c.checked_at, s.name, s.url
FROM checks c
JOIN services s ON s.id = c.service_id
`
	args := []any{limit}
	if hasServiceID {
		query += `WHERE c.service_id = $2 `
		args = append(args, serviceID)
	}
	query += `ORDER BY c.checked_at DESC LIMIT $1`

	rows, err := a.db.Query(r.Context(), query, args...)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	results := []checkResult{}
	for rows.Next() {
		var result checkResult
		var status sql.NullInt32
		if err := rows.Scan(&result.ID, &result.ServiceID, &status, &result.LatencyMS, &result.OK, &result.Error, &result.CheckedAt, &result.ServiceName, &result.ServiceURL); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if status.Valid {
			value := int(status.Int32)
			result.StatusCode = &value
		}
		results = append(results, result)
	}
	writeJSON(w, http.StatusOK, results)
}

func (a *app) stats(w http.ResponseWriter, r *http.Request) {
	var total, healthy, down int64
	var avgLatency sql.NullFloat64
	err := a.db.QueryRow(r.Context(), `
WITH latest AS (
	SELECT DISTINCT ON (service_id) service_id, ok, latency_ms
	FROM checks
	ORDER BY service_id, checked_at DESC
)
SELECT
	(SELECT COUNT(*) FROM services),
	COALESCE(SUM(CASE WHEN latest.ok THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN latest.ok = FALSE THEN 1 ELSE 0 END), 0),
	AVG(latest.latency_ms)
FROM latest
`).Scan(&total, &healthy, &down, &avgLatency)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	avg := 0
	if avgLatency.Valid {
		avg = int(avgLatency.Float64)
	}
	writeJSON(w, http.StatusOK, map[string]int{
		"total":          int(total),
		"healthy":        int(healthy),
		"down":           int(down),
		"averageLatency": avg,
	})
}

func idFromPath(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func staticHandler(root string) http.Handler {
	files := http.FileServer(http.Dir(root))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(root, filepath.Clean(r.URL.Path))
		if _, err := os.Stat(path); err != nil && !strings.HasPrefix(r.URL.Path, "/api/") {
			http.ServeFile(w, r, filepath.Join(root, "index.html"))
			return
		}
		files.ServeHTTP(w, r)
	})
}

func env(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func durationFromEnv(key string, fallback int) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return time.Duration(fallback)
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return time.Duration(fallback)
	}
	return time.Duration(value)
}
