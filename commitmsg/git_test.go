package commitmsg

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func put(t *testing.T, dir, name, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func repository(t *testing.T, committed bool) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "config", "user.email", "test@example.com")
	gitRun(t, dir, "config", "user.name", "Test")
	if committed {
		put(t, dir, "tracked", "original\n")
		put(t, dir, "deleted", "delete me\n")
		gitRun(t, dir, "add", ".")
		gitRun(t, dir, "commit", "-qm", "test: initial subject")
	}
	return dir
}

func checkContains(t *testing.T, got string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if !strings.Contains(got, part) {
			t.Errorf("%q does not contain %q", got, part)
		}
	}
}

func checkExcludes(t *testing.T, got string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if strings.Contains(got, part) {
			t.Errorf("%q unexpectedly contains %q", got, part)
		}
	}
}

func TestCollectScope(t *testing.T) {
	t.Parallel()
	dir := repository(t, true)
	put(t, dir, "tracked", "staged version\n")
	put(t, dir, "addition", "staged addition\n")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "rm", "deleted")
	put(t, dir, "tracked", "working version\n")
	put(t, dir, "untracked", "untracked secret\n")
	before := gitRun(t, dir, "status", "--porcelain=v1")
	diff, history, truncated, err := collect(t.Context(), dir, false)
	if err != nil || truncated || history != "test: initial subject\n" {
		t.Fatalf("collect: history=%q truncated=%v err=%v", history, truncated, err)
	}
	checkContains(t, diff, "+staged version", "+staged addition", "deleted file mode")
	checkExcludes(t, diff, "working version", "untracked")
	diff, _, _, err = collect(t.Context(), dir, true)
	if err != nil {
		t.Fatal(err)
	}
	checkContains(t, diff, "+working version", "+staged addition", "deleted file mode")
	checkExcludes(t, diff, "staged version", "untracked")
	if after := gitRun(t, dir, "status", "--porcelain=v1"); after != before {
		t.Fatalf("repository modified: before %q, after %q", before, after)
	}
}

func TestCollectUnborn(t *testing.T) {
	t.Parallel()
	dir := repository(t, false)
	put(t, dir, "addition", "staged\n")
	gitRun(t, dir, "add", ".")
	put(t, dir, "addition", "working\n")
	put(t, dir, "untracked", "secret\n")
	for _, all := range []bool{false, true} {
		diff, history, _, err := collect(t.Context(), dir, all)
		if err != nil || history != "" {
			t.Fatalf("collect: history=%q err=%v", history, err)
		}
		checkExcludes(t, diff, "untracked")
		if all {
			checkContains(t, diff, "+working")
		} else {
			checkContains(t, diff, "+staged")
		}
	}
}

func TestCollectBinaryAndBounds(t *testing.T) {
	t.Parallel()
	dir := repository(t, true)
	put(t, dir, "binary", "\x00\x01\x02")
	gitRun(t, dir, "add", ".")
	diff, _, _, err := collect(t.Context(), dir, false)
	if err != nil {
		t.Fatal(err)
	}
	checkContains(t, diff, "Binary files")
	put(t, dir, "large", strings.Repeat("a long line of diff content\n", 4000))
	gitRun(t, dir, "add", ".")
	for i := range 7 {
		gitRun(t, dir, "commit", "--allow-empty", "-qm", strings.Repeat(string(rune('a'+i)), 600))
	}
	put(t, dir, "large", strings.Repeat("changed diff content\n", 4000))
	gitRun(t, dir, "add", ".")
	diff, history, truncated, err := collect(t.Context(), dir, false)
	if err != nil || !truncated || len(diff) > diffLimit || len(history) > historyLimit {
		t.Fatalf("collect: diff bytes=%d history bytes=%d truncated=%v err=%v", len(diff), len(history), truncated, err)
	}
	checkExcludes(t, history, "initial subject")
}

func TestCollectTrackedDeletionAndRecentFive(t *testing.T) {
	t.Parallel()
	dir := repository(t, true)
	for _, subject := range []string{"first", "second", "third", "fourth", "fifth", "sixth"} {
		gitRun(t, dir, "commit", "--allow-empty", "-qm", subject)
	}
	if err := os.Remove(filepath.Join(dir, "tracked")); err != nil {
		t.Fatal(err)
	}
	diff, _, _, err := collect(t.Context(), dir, false)
	if err != nil || diff != "" {
		t.Fatalf("staged diff=%q err=%v", diff, err)
	}
	diff, history, _, err := collect(t.Context(), dir, true)
	if err != nil || history != "sixth\nfifth\nfourth\nthird\nsecond\n" {
		t.Fatalf("history=%q err=%v", history, err)
	}
	checkContains(t, diff, "deleted file mode")
}

func TestCollectFromSubdirectoryIgnoresRelativeConfig(t *testing.T) {
	t.Parallel()
	dir := repository(t, true)
	subdir := filepath.Join(dir, "sub")
	if err := os.Mkdir(subdir, 0o700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "config", "diff.relative", "true")
	put(t, dir, "tracked", "root change\n")
	gitRun(t, dir, "add", "tracked")
	for _, all := range []bool{false, true} {
		diff, _, _, err := collect(t.Context(), subdir, all)
		if err != nil {
			t.Fatal(err)
		}
		checkContains(t, diff, "+root change")
	}
}

func TestCollectExcludesSubmoduleDirt(t *testing.T) {
	t.Parallel()
	source := repository(t, true)
	dir := repository(t, true)
	gitRun(t, dir, "-c", "protocol.file.allow=always", "submodule", "add", source, "sub")
	gitRun(t, dir, "commit", "-qam", "test: add submodule")
	subdir := filepath.Join(dir, "sub")
	put(t, subdir, "tracked", "uncommitted submodule change\n")
	diff, _, _, err := collect(t.Context(), dir, true)
	if err != nil || diff != "" {
		t.Fatalf("dirty submodule diff=%q err=%v", diff, err)
	}
	gitRun(t, subdir, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qam", "test: change submodule")
	diff, _, _, err = collect(t.Context(), dir, true)
	if err != nil {
		t.Fatal(err)
	}
	checkContains(t, diff, "Subproject commit")
	checkExcludes(t, diff, "-dirty")
}

func TestCappedOutput(t *testing.T) {
	b := &cappedOutput{limit: 10}
	n, err := b.Write([]byte(strings.Repeat("x", 100)))
	if err != nil || n != 100 || len(b.data) != 10 || !b.truncated {
		t.Fatalf("first write: n=%d err=%v buffer=%+v", n, err, b)
	}
	n, err = b.Write([]byte("more"))
	if err != nil || n != 4 || len(b.data) != 10 {
		t.Fatalf("second write: n=%d err=%v buffer=%+v", n, err, b)
	}
}
