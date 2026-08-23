package api

import (
	_ "embed"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
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
	GetKlines(interval string, limit int) ([]strategy.KlineBar, error)
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
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/pause", s.withAuth(s.handlePause))
	mux.HandleFunc("/api/resume", s.withAuth(s.handleResume))
	mux.HandleFunc("/api/klines", s.withAuth(s.handleKlines))

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

// sessionDuration controls how long a login session lasts before requiring re-auth.
const sessionDuration = 24 * time.Hour

// generateSessionToken creates a time-bounded HMAC-signed session token.
// Format: base64url(expiry_unix).base64url(hmac_signature)
func generateSessionToken(secret string) string {
	expiry := time.Now().Add(sessionDuration).Unix()
	expiryStr := strconv.FormatInt(expiry, 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(expiryStr))
	sig := mac.Sum(nil)
	return base64.URLEncoding.EncodeToString([]byte(expiryStr)) +
		"." + base64.URLEncoding.EncodeToString(sig)
}

// validateSessionToken verifies a session token's HMAC and checks expiry.
func validateSessionToken(token, secret string) bool {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return false
	}
	expiryBytes, err := base64.URLEncoding.DecodeString(parts[0])
	if err != nil {
		return false
	}
	expiry, err := strconv.ParseInt(string(expiryBytes), 10, 64)
	if err != nil {
		return false
	}
	if time.Now().Unix() > expiry {
		return false
	}
	sigBytes, err := base64.URLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(expiryBytes)
	expected := mac.Sum(nil)
	return hmac.Equal(sigBytes, expected)
}

func (s *Server) checkAuth(r *http.Request) bool {
	if s.authToken == "" {
		return true
	}
	token := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		token = strings.TrimPrefix(h, "Bearer ")
	} else if q := r.URL.Query().Get("token"); q != "" {
		token = q
	}
	if token == "" {
		return false
	}
	// Accept either the raw auth_token or a valid session token
	if token == s.authToken {
		return true
	}
	return validateSessionToken(token, s.authToken)
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

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		jsonErr(w, "missing token", http.StatusBadRequest)
		return
	}
	if s.authToken != "" && body.Token != s.authToken {
		jsonErr(w, "invalid token", http.StatusUnauthorized)
		return
	}
	session := generateSessionToken(s.authToken)
	writeJSON(w, map[string]string{"session": session})
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

func (s *Server) handleKlines(w http.ResponseWriter, r *http.Request) {
	interval := r.URL.Query().Get("interval")
	if interval == "" {
		interval = "1m"
	}
	limit := 200
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := fmt.Sscanf(l, "%d", &limit); err != nil || n != 1 || limit < 1 || limit > 1000 {
			limit = 200
		}
	}
	bars, err := s.sc.GetKlines(interval, limit)
	if err != nil {
		jsonErr(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, bars)
}
