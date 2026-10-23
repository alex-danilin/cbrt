package review

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRubricVersion(t *testing.T) {
	if RubricVersion == "unknown" || RubricVersion == "" {
		t.Fatal("rubric has no version line")
	}
}

func TestInstall(t *testing.T) {
	home := t.TempDir()
	rp, note, err := InstallRubric(home, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(rp); string(b) != Rubric() || note != "" {
		t.Fatal("rubric not written verbatim")
	}
}

var at = time.Date(2026, 10, 20, 7, 0, 0, 0, time.UTC)

func installOver(t *testing.T, installed string) (home, note string) {
	t.Helper()
	home = t.TempDir()
	if err := os.WriteFile(RubricPath(home), []byte(installed), 0o600); err != nil {
		t.Fatal(err)
	}
	_, note, err := InstallRubric(home, at)
	if err != nil {
		t.Fatal(err)
	}
	return home, note
}

func installed(t *testing.T, home string) string {
	t.Helper()
	b, err := os.ReadFile(RubricPath(home))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// An older copy, edited or not, is backed up before it is replaced; a second
// replacement in the same second does not overwrite the first backup.
func TestInstallRubricBacksUpOlder(t *testing.T) {
	old := "# Rubric\n\nRubric version: " + strconv.Itoa(versionNum(RubricVersion)-1) + "\n\nmy own edits\n"
	home, note := installOver(t, old)
	if installed(t, home) != Rubric() {
		t.Fatal("older rubric not replaced")
	}
	backup := RubricPath(home) + ".v" + strconv.Itoa(versionNum(RubricVersion)-1) + "-backup-20261020T070000Z"
	if b, err := os.ReadFile(backup); err != nil || string(b) != old {
		t.Fatalf("backup: %v, %q", err, b)
	}
	if !strings.Contains(note, "replaced") || !strings.Contains(note, backup) {
		t.Errorf("note %q does not name the backup", note)
	}
	os.WriteFile(RubricPath(home), []byte(old+"again\n"), 0o600)
	if _, note, err := InstallRubric(home, at); err != nil || !strings.Contains(note, backup+"-2") {
		t.Fatalf("second backup: %v, %q", err, note)
	}
	if b, _ := os.ReadFile(backup); string(b) != old {
		t.Error("an existing backup was overwritten")
	}
	// No version line counts as older.
	home, note = installOver(t, "# Rubric\n\nsomething\n")
	if installed(t, home) != Rubric() || !strings.Contains(note, "-backup-") {
		t.Errorf("unversioned copy: note %q", note)
	}
}

// A newer copy, and a hand-edited copy of the same version, are kept, with a
// note; an identical copy is left alone silently.
func TestInstallRubricKeeps(t *testing.T) {
	newer := "# Rubric\n\nRubric version: " + strconv.Itoa(versionNum(RubricVersion)+1) + "\n\nnew text\n"
	edited := strings.Replace(Rubric(), "## Ground rules", "## Ground rules (mine)", 1)
	for name, text := range map[string]string{"newer": newer, "edited": edited} {
		home, note := installOver(t, text)
		if installed(t, home) != text || !strings.HasPrefix(note, "kept ") {
			t.Errorf("%s: replaced, or no note (%q)", name, note)
		}
		if m, _ := filepath.Glob(RubricPath(home) + ".v*"); len(m) != 0 {
			t.Errorf("%s: backup written for a kept copy", name)
		}
	}
	if home, note := installOver(t, Rubric()); installed(t, home) != Rubric() || note != "" {
		t.Errorf("identical copy: note %q", note)
	}
}
