// Package agent implements the probe-side daemon (design.md §5): config,
// registration, the /proc collector, the WSS client with reconnection, and
// the service-manager abstraction (systemd / procd / fallback).
package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// DefaultConfigPath matches the install layout (design §4.2/§5.4):
// /etc/fobe-agent/config.json, 0600, root-owned.
const (
	DefaultConfigPath    = "/etc/fobe-agent/config.json"
	DefaultMachineIDPath = "/etc/fobe-agent/machine-id"
)

// Config is the agent's persisted state. node_id/node_secret arrive from
// registration; machine_id is stable across reinstalls.
type Config struct {
	ServerURL          string `json:"server_url"` // e.g. https://panel.example.com
	NodeID             string `json:"node_id,omitempty"`
	NodeSecret         string `json:"node_secret,omitempty"`
	MachineID          string `json:"machine_id"`
	Iface              string `json:"iface,omitempty"`                // traffic interface; empty = default-route
	MetricsIntervalSec int    `json:"metrics_interval_sec,omitempty"` // 0 = 60; larger on low-mem devices (§5.4)
}

// LoadConfig reads the config; ErrNoConfig when the file is absent.
func LoadConfig(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoConfig
	}
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	return &c, nil
}

var ErrNoConfig = errors.New("agent not configured (no config.json)")

// SaveConfig writes the config with 0600 (credentials at rest, design §4.2).
func SaveConfig(path string, c *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("mkdir config dir: %w", err)
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return os.Rename(tmp, path)
}

// LoadOrCreateMachineID returns the stable machine id, creating it on first
// run. Stored beside the config so reinstalls reuse the same node (§4.2).
func LoadOrCreateMachineID(path string) (string, error) {
	if raw, err := os.ReadFile(path); err == nil && len(raw) >= 16 {
		return string(trimSpaceBytes(raw)), nil
	}
	id, err := randomToken(16)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(id), 0o600); err != nil {
		return "", fmt.Errorf("write machine-id: %w", err)
	}
	return id, nil
}

func trimSpaceBytes(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && (b[start] == ' ' || b[start] == '\n' || b[start] == '\r' || b[start] == '\t') {
		start++
	}
	for end > start && (b[end-1] == ' ' || b[end-1] == '\n' || b[end-1] == '\r' || b[end-1] == '\t') {
		end--
	}
	return b[start:end]
}
