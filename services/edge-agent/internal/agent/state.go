package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/cdn-console/edge-agent/internal/control"
)

type State struct {
	AppliedGeneration  string       `json:"appliedGeneration"`
	ETag               string       `json:"etag"`
	LastCommandID      string       `json:"lastCommandId"`
	PendingAck         *control.Ack `json:"pendingAck,omitempty"`
	PreviousGeneration string       `json:"previousGeneration"`
	Status             string       `json:"status"`
}

type StateStore struct {
	path string
}

func NewStateStore(dataDirectory string) *StateStore {
	return &StateStore{path: filepath.Join(dataDirectory, "agent-state.json")}
}

func (store *StateStore) Load() (State, error) {
	content, err := os.ReadFile(store.path)
	if os.IsNotExist(err) {
		return State{Status: "healthy"}, nil
	}
	if err != nil {
		return State{}, err
	}
	info, err := os.Stat(store.path)
	if err != nil {
		return State{}, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return State{}, fmt.Errorf("agent state permissions are too broad: %s", store.path)
	}
	var state State
	if err := json.Unmarshal(content, &state); err != nil {
		return State{}, fmt.Errorf("decode agent state: %w", err)
	}
	if state.Status == "" {
		state.Status = "healthy"
	}
	return state, nil
}

func (store *StateStore) Save(state State) error {
	if err := os.MkdirAll(filepath.Dir(store.path), 0o700); err != nil {
		return err
	}
	content, err := json.Marshal(state)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(store.path), ".agent-state-*")
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
	if err := os.Rename(temporaryPath, store.path); err != nil {
		return err
	}
	return os.Chmod(store.path, 0o600)
}
