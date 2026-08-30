package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

const (
	maxBodyBytes   = 1 << 20 // 1MB, generous for a webhook payload
	dbQueryTimeout = 5 * time.Second
)

type Handler struct {
	db *sql.DB
}

func NewMux(db *sql.DB) *http.ServeMux {
	h := &Handler{db: db}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /jobs", h.createJob)
	mux.HandleFunc("GET /jobs", h.listDeadJobs)
	return mux
}

type createJobRequest struct {
	EventType      string          `json:"event_type"`
	Payload        json.RawMessage `json:"payload"`
	DestinationURL string          `json:"destination_url"`
}

type createJobResponse struct {
	ID string `json:"id"`
}

func (h *Handler) createJob(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	var req createJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		writeError(w, newValidationError("invalid JSON body"))
		return
	}
	if req.EventType == "" || len(req.Payload) == 0 || req.DestinationURL == "" {
		writeError(w, newValidationError("event_type, payload, and destination_url are required"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), dbQueryTimeout)
	defer cancel()

	var id string
	err := h.db.QueryRowContext(ctx, `
		INSERT INTO jobs (event_type, payload, destination_url)
		VALUES ($1, $2, $3)
		RETURNING id`,
		req.EventType, []byte(req.Payload), req.DestinationURL,
	).Scan(&id)
	if err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, createJobResponse{ID: id})
}

type deadJob struct {
	ID             string `json:"id"`
	EventType      string `json:"event_type"`
	DestinationURL string `json:"destination_url"`
	AttemptsCount  int    `json:"attempts_count"`
	LastError      string `json:"last_error"`
}

func (h *Handler) listDeadJobs(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("status") != "dead" {
		writeError(w, newValidationError(`only status=dead is supported`))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), dbQueryTimeout)
	defer cancel()

	rows, err := h.db.QueryContext(ctx, `
		SELECT id, event_type, destination_url, attempts_count, last_error
		FROM jobs WHERE status = 'dead'`)
	if err != nil {
		writeError(w, err)
		return
	}
	defer func() { _ = rows.Close() }()

	jobs := []deadJob{}
	for rows.Next() {
		var j deadJob
		var lastError sql.NullString
		if err := rows.Scan(&j.ID, &j.EventType, &j.DestinationURL, &j.AttemptsCount, &lastError); err != nil {
			writeError(w, err)
			return
		}
		j.LastError = lastError.String
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		writeError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, jobs)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if isValidationError(err) {
		status = http.StatusBadRequest
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
