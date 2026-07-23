package profile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const runtimeStateVersion = 1

type runtimeState struct {
	Version  int    `json:"version"`
	Timezone string `json:"timezone,omitempty"`
}

// Location resolves the mutable runtime override first and the committed
// profile default second. Domain database contents never control scheduling.
func (p *Profile) Location() (*time.Location, error) {
	name := p.Config.Timezone
	state, err := p.loadRuntimeState()
	if err != nil {
		return nil, err
	}
	if state.Timezone != "" {
		name = state.Timezone
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("load profile timezone %q: %w", name, err)
	}
	return loc, nil
}

// Timezone reports the effective zone and whether it came from config or the
// machine-local runtime override.
func (p *Profile) Timezone() (name, source string, err error) {
	state, err := p.loadRuntimeState()
	if err != nil {
		return "", "", err
	}
	if state.Timezone != "" {
		return state.Timezone, "runtime override", nil
	}
	return p.Config.Timezone, "config", nil
}

// SetTimezone writes a validated runtime override atomically. An empty name
// clears the override and restores config.toml as the source of truth.
func (p *Profile) SetTimezone(name string) error {
	if name != "" {
		if _, err := time.LoadLocation(name); err != nil {
			return fmt.Errorf("unknown timezone %q: %w", name, err)
		}
	}
	if err := os.MkdirAll(p.StateDir, 0o700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	return writeRuntimeState(filepath.Join(p.StateDir, "runtime.json"), runtimeState{
		Version: runtimeStateVersion, Timezone: name,
	})
}

func (p *Profile) loadRuntimeState() (runtimeState, error) {
	path := filepath.Join(p.StateDir, "runtime.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return runtimeState{Version: runtimeStateVersion}, nil
		}
		return runtimeState{}, fmt.Errorf("read runtime state: %w", err)
	}
	var state runtimeState
	if err := json.Unmarshal(raw, &state); err != nil {
		return runtimeState{}, fmt.Errorf("decode runtime state: %w", err)
	}
	if state.Version != runtimeStateVersion {
		return runtimeState{}, fmt.Errorf("unsupported runtime state version %d", state.Version)
	}
	if state.Timezone != "" {
		if _, err := time.LoadLocation(state.Timezone); err != nil {
			return runtimeState{}, fmt.Errorf("runtime timezone %q: %w", state.Timezone, err)
		}
	}
	return state, nil
}

func writeRuntimeState(path string, state runtimeState) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".runtime-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(state); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
