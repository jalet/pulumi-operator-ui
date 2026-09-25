package s3hist

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/jalet/pulumi-operator-ui/internal/store"
)

// A history file is about 1.5 KB; 1 MiB is far above any real one and bounds memory per key.
const historyBytesMax = 1 << 20

// historyFile names only the fields that are stored. Everything else in the file, including
// config (encrypted secret values) and the other environment keys (author and committer
// emails), is never decoded into memory.
type historyFile struct {
	Kind            string           `json:"kind"`
	StartTime       int64            `json:"startTime"`
	EndTime         int64            `json:"endTime"`
	Result          string           `json:"result"`
	ResourceChanges map[string]int64 `json:"resourceChanges"`
	Environment     struct {
		GitHead string `json:"git.head"`
	} `json:"environment"`
}

var (
	_kinds = map[string]store.RunType{"update": store.RunTypeUp, "refresh": store.RunTypeRefresh,
		"destroy": store.RunTypeDestroy}
	_results = map[string]store.RunState{"succeeded": store.RunStateSucceeded,
		"failed": store.RunStateFailed}
)

// ParseEntry reduces one history file to the stored fields.
func ParseEntry(t Target, key string, body []byte) (store.HistoryEntry, error) {
	if len(body) > historyBytesMax {
		return store.HistoryEntry{}, ErrTooLarge
	}
	var f historyFile
	if err := json.Unmarshal(body, &f); err != nil {
		return store.HistoryEntry{}, fmt.Errorf("parse %s: %w", key, err)
	}
	typ, ok := _kinds[f.Kind]
	if !ok {
		return store.HistoryEntry{}, fmt.Errorf("%w: %q", ErrBadKind, f.Kind)
	}
	state, ok := _results[f.Result]
	if !ok {
		return store.HistoryEntry{}, fmt.Errorf("%w: %q", ErrBadResult, f.Result)
	}
	return store.HistoryEntry{
		Key: key, Bucket: t.Bucket, Namespace: t.Namespace, StackName: t.Stack,
		Type: typ, State: state,
		StartedAt: time.Unix(f.StartTime, 0).UTC(), EndedAt: time.Unix(f.EndTime, 0).UTC(),
		Commit: f.Environment.GitHead, Counts: f.ResourceChanges,
	}, nil
}
