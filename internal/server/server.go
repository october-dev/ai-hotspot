// Package server wires HTTP endpoints for the AI Hotspot router: client-facing
// OpenAI-compatible proxy routes, an admin API, and the dashboard.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/harshsaver/ai-hotspot/internal/config"
	"github.com/harshsaver/ai-hotspot/internal/proxy"
	"github.com/harshsaver/ai-hotspot/internal/store"
	"github.com/harshsaver/ai-hotspot/web"
)

// maxBodyBytes limits the size of an incoming chat-completions body.
const maxBodyBytes = 16 << 20 // 16 MiB

// Server holds the dependencies shared across HTTP handlers.
type Server struct {
	cfg   *config.Config
	store *store.Store
	proxy *proxy.Proxy
}

// New builds a Server from a config and store.
func New(cfg *config.Config, st *store.Store) *Server {
	return &Server{cfg: cfg, store: st, proxy: proxy.New(cfg)}
}

// Handler returns the fully-wired HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/v1/chat/completions", s.handleChat)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/admin/clients", s.handleClients)
	mux.HandleFunc("/admin/clients/", s.handleClientByKey)
	mux.HandleFunc("/admin/usage", s.handleUsage)
	mux.HandleFunc("/", s.handleDashboard)
	return logMiddleware(mux)
}

// --- endpoints ---

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": "ai-hotspot",
		"time":    time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	client, ok := s.authClient(w, r)
	if !ok {
		return
	}
	if client.OverBudget() {
		writeError(w, http.StatusPaymentRequired, "client is over its token budget")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	route, streaming, upBody, err := s.proxy.Prepare(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	resp, err := s.proxy.SendChat(r.Context(), route, upBody)
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream request failed: "+err.Error())
		return
	}
	usage := proxy.Relay(w, resp, streaming)
	s.record(client, route, usage, resp.StatusCode, streaming)
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if _, ok := s.authClient(w, r); !ok {
		return
	}
	resp, err := s.proxy.SendModels(r.Context())
	if err != nil {
		writeError(w, http.StatusBadGateway, "upstream request failed: "+err.Error())
		return
	}
	proxy.Relay(w, resp, false)
}

func (s *Server) handleClients(w http.ResponseWriter, r *http.Request) {
	if !s.authAdmin(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"clients": s.store.ListClients()})
	case http.MethodPost:
		var req struct {
			Name         string `json:"name"`
			BudgetTokens int64  `json:"budgetTokens"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if strings.TrimSpace(req.Name) == "" {
			writeError(w, http.StatusBadRequest, "name is required")
			return
		}
		if req.BudgetTokens < 0 {
			writeError(w, http.StatusBadRequest, "budgetTokens must be >= 0")
			return
		}
		c, err := s.store.CreateClient(strings.TrimSpace(req.Name), req.BudgetTokens)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to create client")
			return
		}
		writeJSON(w, http.StatusCreated, c)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleClientByKey(w http.ResponseWriter, r *http.Request) {
	if !s.authAdmin(w, r) {
		return
	}
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/admin/clients/")
	if key == "" {
		writeError(w, http.StatusBadRequest, "client key required")
		return
	}
	ok, err := s.store.DeleteClient(key)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to delete client")
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "client not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": key})
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	if !s.authAdmin(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, s.store.Usage())
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, web.DashboardHTML)
}

// --- helpers ---

func (s *Server) record(c *store.Client, route proxy.Route, usage proxy.Usage, status int, streaming bool) {
	upName := ""
	if route.Upstream != nil {
		upName = route.Upstream.Name
	}
	err := s.store.RecordUsage(store.RequestLog{
		Time:             time.Now().UTC(),
		ClientKey:        c.Key,
		ClientName:       c.Name,
		Model:            route.Model,
		Upstream:         upName,
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		Status:           status,
		Streaming:        streaming,
	})
	if err != nil {
		log.Printf("record usage: %v", err)
	}
}

func (s *Server) authAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !tokenEqual(bearer(r), s.cfg.AdminToken) {
		writeError(w, http.StatusUnauthorized, "invalid or missing admin token")
		return false
	}
	return true
}

func (s *Server) authClient(w http.ResponseWriter, r *http.Request) (*store.Client, bool) {
	key := bearer(r)
	if key == "" {
		writeError(w, http.StatusUnauthorized, "missing client key (Authorization: Bearer <clientKey>)")
		return nil, false
	}
	c, ok := s.store.GetClient(key)
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid client key")
		return nil, false
	}
	return c, true
}

func bearer(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) >= len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
		return strings.TrimSpace(h[len(prefix):])
	}
	return ""
}

func tokenEqual(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "ai_hotspot_error",
			"code":    status,
		},
	})
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip the noisy dashboard poll to keep logs readable.
		if r.URL.Path == "/admin/usage" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s %s", clientIP(r), r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func clientIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}
