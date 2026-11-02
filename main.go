package main

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	maxBodyBytes  = 64 << 10 // 64 KiB
	defaultDays   = 90
	defaultDBPath = "events.db"
	timeLayout    = time.RFC3339Nano
)

type reportRequest struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Success *bool  `json:"success"`
	Message string `json:"message"`
	Error   string `json:"error"`
}

type event struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Success    bool   `json:"success"`
	Message    string `json:"message,omitempty"`
	Error      string `json:"error,omitempty"`
	ReportedAt string `json:"reportedAt"`
}

type store struct {
	db   *sql.DB
	days int
}

func main() {
	token := strings.TrimSpace(os.Getenv("EVENT_TOKEN"))
	if token == "" {
		log.Fatal("EVENT_TOKEN is required")
	}

	days := defaultDays
	if v := strings.TrimSpace(os.Getenv("EVENT_DAYS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			log.Fatalf("invalid EVENT_DAYS: %q", v)
		}
		days = n
	}

	dbPath := strings.TrimSpace(os.Getenv("EVENT_DB_PATH"))
	if dbPath == "" {
		dbPath = defaultDBPath
	}

	st, err := openStore(dbPath, days)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.db.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("pong"))
	})
	mux.Handle("/report", auth(token, http.HandlerFunc(st.handleReport)))
	mux.Handle("/latest", auth(token, http.HandlerFunc(st.handleLatest)))

	port := os.Getenv("FC_SERVER_PORT")
	if port == "" {
		port = "9000"
	}

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
	}

	log.Printf("listening on :%s, db=%s, keep=%d", port, dbPath, days)
	log.Fatal(srv.ListenAndServe())
}

func auth(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(token)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func openStore(path string, days int) (*store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// SQLite 单写者，限制连接数避免 "database is locked"
	db.SetMaxOpenConns(1)

	for _, stmt := range []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA busy_timeout = 5000`,
		`CREATE TABLE IF NOT EXISTS events (
			id          TEXT    NOT NULL,
			name        TEXT    NOT NULL,
			success     INTEGER NOT NULL,
			message     TEXT    NOT NULL DEFAULT '',
			error       TEXT    NOT NULL DEFAULT '',
			reported_at TEXT    NOT NULL,
			PRIMARY KEY (id, reported_at)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_latest ON events (id, reported_at DESC)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			return nil, fmt.Errorf("%s: %w", stmt, err)
		}
	}
	return &store{db: db, days: days}, nil
}

func (s *store) handleReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	var req reportRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err := dec.Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json body"})
		return
	}
	if dec.More() {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unexpected trailing data"})
		return
	}

	req.ID = strings.TrimSpace(req.ID)
	req.Name = strings.TrimSpace(req.Name)
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id is required"})
		return
	}
	if req.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	if req.Success == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "success is required"})
		return
	}

	// 先删超过 N 条的旧记录，再插入本次结果，保证写入成功即生效
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage error"})
		return
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(timeLayout)
	if _, err := tx.ExecContext(r.Context(),
		`INSERT INTO events (id, name, success, message, error, reported_at) VALUES (?, ?, ?, ?, ?, ?)`,
		req.ID, req.Name, *req.Success, req.Message, req.Error, now,
	); err != nil {
		log.Printf("insert event %q: %v", req.ID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage error"})
		return
	}

	if _, err := tx.ExecContext(r.Context(),
		`DELETE FROM events WHERE id = ? AND reported_at NOT IN (
			SELECT reported_at FROM events WHERE id = ? ORDER BY reported_at DESC LIMIT ?
		)`, req.ID, req.ID, s.days,
	); err != nil {
		log.Printf("prune event %q: %v", req.ID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage error"})
		return
	}

	if err := tx.Commit(); err != nil {
		log.Printf("commit event %q: %v", req.ID, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage error"})
		return
	}

	log.Printf("reported id=%q success=%t", req.ID, *req.Success)
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *store) handleLatest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	rows, err := s.db.QueryContext(r.Context(), `
		SELECT e.id, e.name, e.success, e.message, e.error, e.reported_at
		FROM events e
		JOIN (
			SELECT id, MAX(reported_at) AS reported_at FROM events GROUP BY id
		) latest ON latest.id = e.id AND latest.reported_at = e.reported_at
		ORDER BY e.reported_at DESC`)
	if err != nil {
		log.Printf("query latest: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage error"})
		return
	}
	defer rows.Close()

	list := make([]event, 0)
	for rows.Next() {
		var (
			e       event
			success int
		)
		if err := rows.Scan(&e.ID, &e.Name, &success, &e.Message, &e.Error, &e.ReportedAt); err != nil {
			log.Printf("scan latest: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage error"})
			return
		}
		e.Success = success != 0
		list = append(list, e)
	}
	if err := rows.Err(); err != nil {
		log.Printf("iterate latest: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "storage error"})
		return
	}

	writeJSON(w, http.StatusOK, list)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
