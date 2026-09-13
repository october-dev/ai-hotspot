// Package store persists clients and usage to an atomically-written JSON file.
package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// maxRecent caps how many recent request logs are retained in memory / on disk.
const maxRecent = 200

// Client is an issued LAN client key plus its accumulated usage and budget.
type Client struct {
	Key              string    `json:"key"`
	Name             string    `json:"name"`
	BudgetTokens     int64     `json:"budgetTokens"` // 0 = unlimited
	Requests         int64     `json:"requests"`
	PromptTokens     int64     `json:"promptTokens"`
	CompletionTokens int64     `json:"completionTokens"`
	TotalTokens      int64     `json:"totalTokens"`
	CreatedAt        time.Time `json:"createdAt"`
}

// OverBudget reports whether the client has met or exceeded its token budget.
func (c *Client) OverBudget() bool {
	return c.BudgetTokens > 0 && c.TotalTokens >= c.BudgetTokens
}

// RequestLog is a single recorded request for the recent-activity feed.
type RequestLog struct {
	Time             time.Time `json:"time"`
	ClientKey        string    `json:"clientKey"`
	ClientName       string    `json:"clientName"`
	Model            string    `json:"model"`
	Upstream         string    `json:"upstream"`
	PromptTokens     int64     `json:"promptTokens"`
	CompletionTokens int64     `json:"completionTokens"`
	TotalTokens      int64     `json:"totalTokens"`
	Status           int       `json:"status"`
	Streaming        bool      `json:"streaming"`
}

// Aggregate is a roll-up of usage across all clients.
type Aggregate struct {
	ClientCount           int   `json:"clientCount"`
	TotalRequests         int64 `json:"totalRequests"`
	TotalPromptTokens     int64 `json:"totalPromptTokens"`
	TotalCompletionTokens int64 `json:"totalCompletionTokens"`
	TotalTokens           int64 `json:"totalTokens"`
}

// UsageReport is the payload served to the admin dashboard.
type UsageReport struct {
	Totals  Aggregate    `json:"totals"`
	Clients []Client     `json:"clients"`
	Recent  []RequestLog `json:"recent"`
}

type persisted struct {
	Clients map[string]*Client `json:"clients"`
	Recent  []RequestLog       `json:"recent"`
}

// Store is a concurrency-safe, file-backed store of clients and usage.
type Store struct {
	mu      sync.Mutex
	path    string
	clients map[string]*Client
	recent  []RequestLog
}

// New creates a Store backed by path, loading existing state if present.
func New(path string) (*Store, error) {
	s := &Store{
		path:    path,
		clients: make(map[string]*Client),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // fresh start
		}
		return fmt.Errorf("read data file %s: %w", s.path, err)
	}
	var p persisted
	if err := json.Unmarshal(b, &p); err != nil {
		return fmt.Errorf("parse data file %s: %w", s.path, err)
	}
	if p.Clients != nil {
		s.clients = p.Clients
	}
	s.recent = p.Recent
	return nil
}

// saveLocked writes state atomically (temp file + rename). Caller holds s.mu.
func (s *Store) saveLocked() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	p := persisted{Clients: s.clients, Recent: s.recent}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".hotspot-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, s.path)
}

// CreateClient issues a new client key with an optional token budget.
func (s *Store) CreateClient(name string, budgetTokens int64) (*Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, err := genKey()
	if err != nil {
		return nil, err
	}
	c := &Client{
		Key:          key,
		Name:         name,
		BudgetTokens: budgetTokens,
		CreatedAt:    time.Now().UTC(),
	}
	s.clients[key] = c
	if err := s.saveLocked(); err != nil {
		delete(s.clients, key)
		return nil, err
	}
	cp := *c
	return &cp, nil
}

// GetClient returns a copy of the client for key, if it exists.
func (s *Store) GetClient(key string) (*Client, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clients[key]
	if !ok {
		return nil, false
	}
	cp := *c
	return &cp, true
}

// ListClients returns all clients, oldest first.
func (s *Store) ListClients() []Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotClientsLocked()
}

func (s *Store) snapshotClientsLocked() []Client {
	out := make([]Client, 0, len(s.clients))
	for _, c := range s.clients {
		out = append(out, *c)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// DeleteClient removes a client key. Returns false if it did not exist.
func (s *Store) DeleteClient(key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.clients[key]; !ok {
		return false, nil
	}
	delete(s.clients, key)
	if err := s.saveLocked(); err != nil {
		return false, err
	}
	return true, nil
}

// RecordUsage folds a request's usage into its client and the recent feed.
func (s *Store) RecordUsage(log RequestLog) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.clients[log.ClientKey]; ok {
		c.Requests++
		c.PromptTokens += log.PromptTokens
		c.CompletionTokens += log.CompletionTokens
		c.TotalTokens += log.TotalTokens
	}
	s.recent = append(s.recent, log)
	if len(s.recent) > maxRecent {
		s.recent = s.recent[len(s.recent)-maxRecent:]
	}
	return s.saveLocked()
}

// Usage returns an aggregate report plus per-client and recent (newest-first) data.
func (s *Store) Usage() UsageReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	clients := s.snapshotClientsLocked()
	rep := UsageReport{Clients: clients}
	rep.Totals.ClientCount = len(clients)
	for _, c := range clients {
		rep.Totals.TotalRequests += c.Requests
		rep.Totals.TotalPromptTokens += c.PromptTokens
		rep.Totals.TotalCompletionTokens += c.CompletionTokens
		rep.Totals.TotalTokens += c.TotalTokens
	}
	recent := make([]RequestLog, len(s.recent))
	for i, r := range s.recent {
		recent[len(s.recent)-1-i] = r // newest first
	}
	rep.Recent = recent
	return rep
}

func genKey() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "hs_" + hex.EncodeToString(b), nil
}
