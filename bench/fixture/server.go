// Package main is the ticket service: HTTP in front, store behind, worker async.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"
)

// Server wires the HTTP surface to the store and the worker queue. Every
// handler validates first, mutates second, and enqueues notifications last,
// so a failed validation can never leave a half-applied write behind.
type Server struct {
	store  *Store
	queue  chan<- Job
	limits map[string]int
}

func NewServer(st *Store, q chan<- Job) *Server {
	return &Server{store: st, queue: q, limits: map[string]int{"create": 50, "list": 200}}
}

func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /tickets", s.handleCreate)
	mux.HandleFunc("GET /tickets/{id}", s.handleGet)
	mux.HandleFunc("GET /tickets", s.handleList)
	mux.HandleFunc("POST /tickets/{id}/close", s.handleClose)
	mux.HandleFunc("GET /version", s.handleVersion)
	return mux
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title    string `json:"title"`
		Body     string `json:"body"`
		Priority int    `json:"priority"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Title == "" || len(req.Title) > 200 {
		http.Error(w, "title must be 1-200 chars", http.StatusBadRequest)
		return
	}
	if req.Priority < 0 || req.Priority > 3 {
		http.Error(w, "priority must be 0-3", http.StatusBadRequest)
		return
	}
	t := s.store.Create(req.Title, req.Body, req.Priority)
	select {
	case s.queue <- Job{Kind: "ticket.created", TicketID: t.ID, Attempt: 1}:
	default:
		log.Printf("queue full; notification for %d dropped", t.ID)
	}
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(t)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	t, ok := s.store.Get(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	json.NewEncoder(w).Encode(t)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	cursor, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > s.limits["list"] {
		limit = 50
	}
	items, next := s.store.List(cursor, limit)
	json.NewEncoder(w).Encode(map[string]any{"items": items, "next_cursor": next})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"version": "1.2.3"})
}

func (s *Server) handleClose(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	if !s.store.Close(id, time.Now()) {
		http.NotFound(w, r)
		return
	}
	s.queue <- Job{Kind: "ticket.closed", TicketID: id, Attempt: 1}
	w.WriteHeader(http.StatusNoContent)
}

func main() {
	st := NewStore()
	q := make(chan Job, 256)
	go RunWorker(q, st, time.Second)
	srv := NewServer(st, q)
	fmt.Println("listening on :8090")
	log.Fatal(http.ListenAndServe(":8090", srv.routes()))
}
