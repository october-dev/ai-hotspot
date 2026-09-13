// Package config loads and validates the AI Hotspot server configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
)

// Upstream describes one OpenAI-compatible provider the router can forward to.
type Upstream struct {
	Name         string            `json:"name"`
	BaseURL      string            `json:"baseURL"`
	APIKey       string            `json:"apiKey"`
	ExtraHeaders map[string]string `json:"extraHeaders,omitempty"`
}

// Config is the full server configuration, loaded from a JSON file.
type Config struct {
	Port            int        `json:"port"`
	AdminToken      string     `json:"adminToken"`
	DefaultUpstream string     `json:"defaultUpstream"`
	DataFile        string     `json:"dataFile"`
	Upstreams       []Upstream `json:"upstreams"`
}

// Defaults applied when a field is left empty.
const (
	DefaultPort     = 8760
	DefaultDataFile = "./data/hotspot.json"
)

// Load reads a config file from path, applies defaults, and validates it.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Port == 0 {
		c.Port = DefaultPort
	}
	if c.DataFile == "" {
		c.DataFile = DefaultDataFile
	}
	if c.DefaultUpstream == "" && len(c.Upstreams) > 0 {
		c.DefaultUpstream = c.Upstreams[0].Name
	}
}

func (c *Config) validate() error {
	if c.AdminToken == "" {
		return fmt.Errorf("config: adminToken must be set")
	}
	if len(c.Upstreams) == 0 {
		return fmt.Errorf("config: at least one upstream is required")
	}
	seen := make(map[string]bool, len(c.Upstreams))
	for _, u := range c.Upstreams {
		if u.Name == "" {
			return fmt.Errorf("config: every upstream must have a name")
		}
		if u.BaseURL == "" {
			return fmt.Errorf("config: upstream %q must have a baseURL", u.Name)
		}
		if seen[u.Name] {
			return fmt.Errorf("config: duplicate upstream name %q", u.Name)
		}
		seen[u.Name] = true
	}
	if c.Upstream(c.DefaultUpstream) == nil {
		return fmt.Errorf("config: defaultUpstream %q not found among upstreams", c.DefaultUpstream)
	}
	return nil
}

// Upstream returns the upstream with the given name, or nil if not found.
func (c *Config) Upstream(name string) *Upstream {
	for i := range c.Upstreams {
		if c.Upstreams[i].Name == name {
			return &c.Upstreams[i]
		}
	}
	return nil
}
