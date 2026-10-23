package tasks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// OverridesVersion is the current Overrides.V.
const OverridesVersion = 1

// Overrides is task_overrides.json: the user's corrections, applied after
// the automatic rules (confidence manual). Task ids may be current ids or
// aliases.
type Overrides struct {
	V int `json:"v"`
	// Merge: each list of task ids becomes one task (the oldest id survives).
	Merge [][]string `json:"merge,omitempty"`
	// Split detaches ranges from a task into a new task.
	Split []SplitRule `json:"split,omitempty"`
	// Rename sets a task's Label.
	Rename map[string]string `json:"rename,omitempty"`
	// Exclude drops tasks from the output (their ids stay reserved).
	Exclude []string `json:"exclude,omitempty"`
}

// SplitRule detaches Members (range keys "<adapter>/<store>/<native>#<from
// seq>", as RangeKey prints them) from Task into one new task with
// SplitFrom = Task. Task keeps its id.
type SplitRule struct {
	Task    string   `json:"task"`
	Members []string `json:"members"`
}

// LoadOverrides reads an overrides file. A missing file is no overrides;
// unknown fields and a version newer than OverridesVersion are errors.
func LoadOverrides(path string) (Overrides, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Overrides{V: OverridesVersion}, nil
	}
	if err != nil {
		return Overrides{}, err
	}
	var o Overrides
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		return Overrides{}, fmt.Errorf("%s: %w", path, err)
	}
	if o.V > OverridesVersion {
		return Overrides{}, fmt.Errorf("%s: version %d is newer than this cbrt understands (%d)", path, o.V, OverridesVersion)
	}
	return o, nil
}
