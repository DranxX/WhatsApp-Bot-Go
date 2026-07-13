package config

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// Config holds all runtime configuration loaded from config.json.
type Config struct {
	Name        string `json:"name"`
	Owner       string `json:"owner"`
	Bot         string `json:"bot"`
	Prefix      string `json:"prefix"`
	Status      string `json:"status"`      // public | ponly | gonly | self
	Autoread    string `json:"autoread"`    // enable | disable
	LoginMethod string `json:"loginMethod"` // qr | pairs
	Markdown    bool   `json:"markdown"`    // WA native markdown hint for plugins
}

var (
	mu         sync.RWMutex
	current    Config
	configPath string
)

var statusAliases = map[string]string{
	"public": "public", "ponly": "ponly", "pconly": "ponly",
	"gonly": "gonly", "gconly": "gonly",
	"self": "self", "selfonly": "self",
}

// Load reads the config file and populates the global singleton.
// If the file is missing, it creates one with defaults and uses those.
func Load(path string) error {
	configPath = path
	data, err := os.ReadFile(path)
	if err != nil {
		current = defaultConfig()
		fmt.Printf("[CONFIG] Failed to read %s, using defaults: %v\n", path, err)
		return nil
	}
	mu.Lock()
	defer mu.Unlock()
	current = defaultConfig()
	if err = json.Unmarshal(data, &current); err != nil {
		return fmt.Errorf("config parse error: %w", err)
	}
	normalize(&current)
	return nil
}

// Get returns a copy of the current config (safe for concurrent reads).
func Get() Config {
	mu.RLock()
	defer mu.RUnlock()
	return current
}

// Update atomically modifies and persists the config.
func Update(fn func(*Config)) {
	mu.Lock()
	defer mu.Unlock()
	fn(&current)
	normalize(&current)
	save()
}

func save() {
	data, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		fmt.Println("[CONFIG] Marshal error:", err)
		return
	}
	data = append(data, '\n')
	if err = os.WriteFile(configPath, data, 0644); err != nil {
		fmt.Println("[CONFIG] Write error:", err)
	}
}

func normalize(c *Config) {
	if mapped, ok := statusAliases[c.Status]; ok {
		c.Status = mapped
	} else {
		c.Status = "public"
	}
	if c.Prefix == "" {
		c.Prefix = "."
	}
	if c.Name == "" {
		c.Name = "Template Go"
	}
	if c.LoginMethod != "qr" && c.LoginMethod != "pairs" {
		c.LoginMethod = "qr"
	}
	if c.Autoread != "enable" && c.Autoread != "disable" {
		c.Autoread = "enable"
	}
}

func defaultConfig() Config {
	return Config{
		Name:        "Template Go",
		Prefix:      ".",
		Status:      "public",
		Autoread:    "enable",
		LoginMethod: "qr",
		Markdown:    true,
	}
}
