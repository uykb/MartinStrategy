package api

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/uykb/MartinStrategy/internal/strategy"
)

//go:embed dashboard.html
var dashboardHTML string

// StrategyController is the interface the API server needs from the strategy.
type StrategyController interface {
	Snapshot() *strategy.DashboardState
	Pause() error
	Resume() error
	CloseAll() error
	RefreshTP() error
}

// Server serves the web dashboard and REST API.
type Server struct {
	sc        StrategyController
	authToken string
	port      int
	clients   map[chan []byte]struct{}
	clientsMu sync.Mutex
	publicIP  string // cached public IP for Binance whitelist display
}

// NewServer creates a new API server.
func NewServer(sc StrategyController, port int, authToken string) *Server {
	s := &Server{
		sc:        sc,
		authToken: authToken,
		port:      port,
		clients:   make(map[chan []byte]struct{}),
	}
	go s.initPublicIP()
	return s
}

// initPublicIP fetches the server's public IP once at startup.
func (s *Server) initPublicIP() {
	clients := []string{
		"https://api.ipify.org",
		"https://checkip.amazonaws.com",
		"https://httpbin.org/ip",
	}
	for _, url := range clients {
		resp, err := http.Get(url)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
		resp.Body.Close()
		if err != nil {
			continue
		}
		ip := strings.TrimSpace(string(body))
		// httpbin returns JSON {"origin":"x.x.x.x"}
		if strings.HasPrefix(ip, "{") {
			var m map[string]interface{}
			if json.Unmarshal([]byte(ip), &m) == nil {
				if origin, ok := m["origin"].(string); ok {
					ip = origin
				}
			}
		}
		if ip != "" {
			s.publicIP = ip
			log.Printf("[API] Public IP: %s", ip)
			return
		}
	}
	log.Printf("[API] Could not determine public IP")
}

// Start starts the HTTP server. Blocks until the server stops.
func (s *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleDashboard)
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/state", s.withAuth(s.handleState))
	mux.HandleFunc("/api/stream", s.handleStream)
	mux.HandleFunc("/api/pause", s.withAuth(s.handlePause))
	mux.HandleFunc("/api/resume", s.withAuth(s.handleResume))
	mux.HandleFunc("/api/close-all", s.withAuth(s.handleCloseAll))
	mux.HandleFunc("/api/refresh-tp", s.withAuth(s.handleRefreshTP))

	go s.pushLoop()

	addr := fmt.Sprintf(":%d", s.port)
	log.Printf("[API] Dashboard server listening on http://localhost%s", addr)
	server := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	return server.ListenAndServe()
}

// ── Auth ────────────────────────────────────────────────────────────

func (s *Server) checkAuth(r *http.Request) bool {
	if s.authToken == "" {
		return true
	}
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ") == s.authToken
	}
	q := r.URL.Query().Get("token")
	return q == s.authToken
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
			w.WriteHeader(204)
			return
		}
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if !s.checkAuth(r) {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func jsonErr(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, `{"error":"%s"}`, msg)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// ── Handlers ────────────────────────────────────────────────────────

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write([]byte(dashboardHTML))
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]interface{}{
		"ok":           true,
		"auth_enabled": s.authToken != "",
		"public_ip":    s.publicIP,
	})
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.sc.Snapshot())
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	if !s.checkAuth(r) {
		jsonErr(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	ch := make(chan []byte, 10)
	s.clientsMu.Lock()
	s.clients[ch] = struct{}{}
	s.clientsMu.Unlock()

	defer func() {
		s.clientsMu.Lock()
		delete(s.clients, ch)
		s.clientsMu.Unlock()
	}()

	// Send initial state
	data, _ := json.Marshal(s.sc.Snapshot())
	fmt.Fprintf(w, "data: %s\n\n", data)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	ctx := r.Context()
	for {
		select {
		case msg := <-ch:
			_, err := w.Write(msg)
			if err != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) pushLoop() {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		snapshot := s.sc.Snapshot()
		data, err := json.Marshal(snapshot)
		if err != nil {
			continue
		}
		msg := append([]byte("data: "), data...)
		msg = append(msg, '\n', '\n')

		s.clientsMu.Lock()
		for ch := range s.clients {
			select {
			case ch <- msg:
			default:
			}
		}
		s.clientsMu.Unlock()
	}
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.sc.Pause(); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.sc.Resume(); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleCloseAll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.sc.CloseAll(); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleRefreshTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := s.sc.RefreshTP(); err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
