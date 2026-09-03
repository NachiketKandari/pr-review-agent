package xlog

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// redirect stderr for capture-free level tests: Setup writes text to
// os.Stderr, so for filtering tests we point a fresh setup at a temp file
// where the JSON copy carries every record we need to assert on.
func TestLevelFiltering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")

	for _, tc := range []struct {
		name  string
		level slog.Level
		want  bool // whether the Debug line makes it through
	}{
		{"debug", slog.LevelDebug, true},
		{"info", slog.LevelInfo, false},
		{"warn", slog.LevelWarn, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanup, err := Setup(tc.level, path)
			if err != nil {
				t.Fatalf("Setup: %v", err)
			}
			Debug("debug line", "k", "v")
			Info("info line")
			Warn("warn line")
			Error("error line")
			cleanup()

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}

			hasDebug := strings.Contains(string(data), `"debug line"`)
			if hasDebug != tc.want {
				t.Errorf("debug record present = %v, want %v (level %v)", hasDebug, tc.want, tc.level)
			}
			wantInfo := tc.level <= slog.LevelInfo
			hasInfo := strings.Contains(string(data), `"info line"`)
			if hasInfo != wantInfo {
				t.Errorf("info record present = %v, want %v (level %v)", hasInfo, wantInfo, tc.level)
			}
			// Warn and above must always be present at every tested level.
			for _, wantMsg := range []string{"warn line", "error line"} {
				if !strings.Contains(string(data), wantMsg) {
					t.Errorf("level %v: missing %q", tc.level, wantMsg)
				}
			}
		})
	}
}

func TestWarnLevelSuppressesInfo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	cleanup, err := Setup(slog.LevelWarn, path)
	if err != nil {
		t.Fatal(err)
	}
	Info("info suppressed")
	Error("error kept")
	cleanup()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "info suppressed") {
		t.Error("Info record leaked at Warn level")
	}
	if !strings.Contains(string(data), "error kept") {
		t.Error("Error record missing at Warn level")
	}
}

func TestJSONFileWriter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review.jsonl")

	cleanup, err := Setup(slog.LevelDebug, path)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	Info("first write", "pr", "owner/repo#1")
	cleanup()

	cleanup2, err := Setup(slog.LevelDebug, path)
	if err != nil {
		t.Fatalf("second Setup: %v", err)
	}
	Info("second write", "pr", "owner/repo#2")
	cleanup2()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 appended JSON lines, got %d", len(lines))
	}
	var first, second map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 1 not JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("line 2 not JSON: %v", err)
	}
	if first["msg"] != "first write" || second["msg"] != "second write" {
		t.Errorf("unexpected records: %v / %v", first["msg"], second["msg"])
	}
	if _, ok := first["time"]; !ok {
		t.Error("JSON record missing time field")
	}
}
