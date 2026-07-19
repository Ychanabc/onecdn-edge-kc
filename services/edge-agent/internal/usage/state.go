package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cdn-console/edge-agent/internal/control"
)

// Cursor identifies the next byte to read from an append-only access log.
type Cursor struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	Offset int64  `json:"offset"`
}

// Pending holds a prepared usage report that must be delivered at-least-once
// before the committed cursor may advance.
type Pending struct {
	Report     control.UsageReport `json:"report"`
	NextCursor Cursor              `json:"nextCursor"`
}

type durableState struct {
	Committed Cursor   `json:"committed"`
	Pending   *Pending `json:"pending,omitempty"`
}

func (collector *Collector) loadState() (durableState, error) {
	content, err := os.ReadFile(collector.statePath)
	if os.IsNotExist(err) {
		return durableState{}, nil
	}
	if err != nil {
		return durableState{}, err
	}
	var state durableState
	if err := json.Unmarshal(content, &state); err != nil {
		return durableState{}, fmt.Errorf("decode usage state: %w", err)
	}
	return state, nil
}

func (collector *Collector) saveState(state durableState) error {
	if err := os.MkdirAll(filepath.Dir(collector.statePath), 0o700); err != nil {
		return err
	}
	content, err := json.Marshal(state)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(collector.statePath), ".usage-state-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(append(content, '\n')); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, collector.statePath); err != nil {
		return err
	}
	return os.Chmod(collector.statePath, 0o600)
}
