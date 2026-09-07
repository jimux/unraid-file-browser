package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"unraid-filebrowser/internal/types"
)

func TestDefault(t *testing.T) {
	d := Default()
	if d.DataDir != "/mnt/user/appdata/filebrowser" {
		t.Errorf("DataDir = %q", d.DataDir)
	}
	if !reflect.DeepEqual(d.Index.Roots, []string{"/mnt/user"}) {
		t.Errorf("Roots = %v, want [/mnt/user]", d.Index.Roots)
	}
	if d.Index.Schedule != "0 3 * * *" {
		t.Errorf("Schedule = %q, want the nightly cron", d.Index.Schedule)
	}
	if d.Index.Parallelism != 2 {
		t.Errorf("Parallelism = %d, want 2", d.Index.Parallelism)
	}
	if d.Index.Content.Enabled {
		t.Errorf("content indexing must be opt-in")
	}
	if d.Index.Content.MaxFileBytes != 10485760 {
		t.Errorf("MaxFileBytes = %d, want 10485760", d.Index.Content.MaxFileBytes)
	}
	// The index package owns the extension allowlist; config leaves it nil so
	// the policy can evolve without rewriting existing files.
	if d.Index.Content.Extensions != nil {
		t.Errorf("Content.Extensions = %v, want nil", d.Index.Content.Extensions)
	}
	if d.Index.Content.IncludePaths != nil {
		t.Errorf("Content.IncludePaths = %v, want nil", d.Index.Content.IncludePaths)
	}
	// Default() must hand out independent copies: mutating one must not leak.
	other := Default()
	d.Index.Roots[0] = "/mutated"
	if other.Index.Roots[0] != "/mnt/user" {
		t.Errorf("Default() shares its slices between callers")
	}
}

func TestLoadMissingFileYieldsDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does", "not", "exist.json")
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load of a missing file must not error, got %v", err)
	}
	if !reflect.DeepEqual(got, Default()) {
		t.Errorf("Load(missing) = %+v, want Default()", got)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	want := Config{
		DataDir: "/mnt/user/appdata/fb",
		Index: types.IndexConfig{
			Roots:       []string{"/mnt/user", "/mnt/disk1"},
			Schedule:    "15 4 * * 0",
			Parallelism: 4,
			Content: types.ContentRules{
				Enabled:      true,
				IncludePaths: []string{"/mnt/user/code", "/mnt/user/docs"},
				Extensions:   []string{"go", "md", "log"},
				MaxFileBytes: 1 << 20,
			},
		},
	}
	if err := want.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestSavePermissionsAndAtomicity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	if err := Default().Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// The file can hold index paths and lives on the flash drive: 0600 only.
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 0600", perm)
	}

	// The temp file must be renamed away, not left behind.
	names := listDir(t, dir)
	if len(names) != 1 || names[0] != "config.json" {
		t.Errorf("directory after Save = %v, want just config.json", names)
	}

	// Overwriting keeps exactly one file, and the old content never survives
	// as a partial write: the reader either sees the old or the new whole file.
	updated := Default()
	updated.DataDir = "/mnt/user/appdata/other"
	if err := updated.Save(path); err != nil {
		t.Fatalf("Save (overwrite): %v", err)
	}
	if names := listDir(t, dir); len(names) != 1 {
		t.Errorf("directory after overwrite = %v, want one file", names)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.DataDir != "/mnt/user/appdata/other" {
		t.Errorf("DataDir = %q, overwrite did not take", got.DataDir)
	}
	if perm := mustStat(t, path).Mode().Perm(); perm != 0o600 {
		t.Errorf("mode after overwrite = %o, want 0600", perm)
	}
}

// A failed Save (unwritable directory) must leave the previous file intact and
// drop the temp file — that is the point of tmp+rename.
func TestSaveFailureLeavesPreviousFileIntact(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: directory permissions do not deny writes")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := Default().Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil { // r-x: no new files
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	bad := Default()
	bad.DataDir = "/should/never/land"
	if err := bad.Save(path); err == nil {
		t.Fatalf("Save into a read-only directory unexpectedly succeeded")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.DataDir != Default().DataDir {
		t.Errorf("DataDir = %q, the failed Save corrupted the file", got.DataDir)
	}
	if names := listDir(t, dir); len(names) != 1 {
		t.Errorf("directory = %v, want only config.json (no temp leftovers)", names)
	}
}

// Fields absent from the file keep their defaults: a hand-written config that
// only sets one knob must not blank out the rest.
func TestLoadMergesOntoDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"index":{"content":{"enabled":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !got.Index.Content.Enabled {
		t.Errorf("the one field that was set did not take")
	}
	if got.DataDir != Default().DataDir {
		t.Errorf("DataDir = %q, want the default", got.DataDir)
	}
	if !reflect.DeepEqual(got.Index.Roots, Default().Index.Roots) {
		t.Errorf("Roots = %v, want the default", got.Index.Roots)
	}
	if got.Index.Content.MaxFileBytes != DefaultMaxFileBytes {
		t.Errorf("MaxFileBytes = %d, want the default", got.Index.Content.MaxFileBytes)
	}
}

// A hand-edited file with absurd values is repaired rather than rejected: the
// daemon has to boot.
func TestLoadNormalizesBadValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{
	  "dataDir": "",
	  "index": {
	    "roots": [],
	    "schedule": "",
	    "parallelism": 0,
	    "content": {"enabled": true, "maxFileBytes": -5}
	  }
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	d := Default()
	if got.DataDir != d.DataDir {
		t.Errorf("DataDir = %q", got.DataDir)
	}
	if !reflect.DeepEqual(got.Index.Roots, d.Index.Roots) {
		t.Errorf("Roots = %v", got.Index.Roots)
	}
	if got.Index.Schedule != d.Index.Schedule {
		t.Errorf("Schedule = %q", got.Index.Schedule)
	}
	if got.Index.Parallelism != d.Index.Parallelism {
		t.Errorf("Parallelism = %d", got.Index.Parallelism)
	}
	if got.Index.Content.MaxFileBytes != d.Index.Content.MaxFileBytes {
		t.Errorf("MaxFileBytes = %d", got.Index.Content.MaxFileBytes)
	}
	if !got.Index.Content.Enabled {
		t.Errorf("normalize must not undo a deliberate setting")
	}
}

func TestLoadMalformedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err == nil {
		t.Fatalf("Load of malformed JSON must report the problem")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q should name the offending file", err)
	}
	// ...and still hand back something usable so the daemon can start.
	if !reflect.DeepEqual(got, Default()) {
		t.Errorf("Load(malformed) = %+v, want Default()", got)
	}
}

// The on-disk shape is the contract the settings page reads and writes.
func TestSavedJSONShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Default().Save(path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("saved file is not valid JSON: %v", err)
	}
	for _, key := range []string{"dataDir", "index"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("saved config is missing the %q key", key)
		}
	}
	var idx map[string]json.RawMessage
	if err := json.Unmarshal(raw["index"], &idx); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"roots", "schedule", "parallelism", "content"} {
		if _, ok := idx[key]; !ok {
			t.Errorf("saved index config is missing the %q key", key)
		}
	}
	if b[len(b)-1] != '\n' {
		t.Errorf("saved file should end with a newline")
	}
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(des))
	for _, de := range des {
		out = append(out, de.Name())
	}
	return out
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}
