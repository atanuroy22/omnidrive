package provider

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestLocal(t *testing.T) (*local, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "note.md"), []byte("# note"), 0o644); err != nil {
		t.Fatal(err)
	}
	drv, err := newLocal(Config{Creds: Credentials{"root": root}})
	if err != nil {
		t.Fatal(err)
	}
	return drv.(*local), root
}

// The object ID arrives straight from an HTTP request, so a traversal would
// hand out the entire filesystem.
func TestLocalRejectsPathTraversal(t *testing.T) {
	l, root := newTestLocal(t)

	// A secret one level above the shared folder.
	secret := filepath.Join(filepath.Dir(root), "secret.txt")
	if err := os.WriteFile(secret, []byte("do not leak"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(secret)

	ctx := context.Background()
	for _, id := range []string{
		"../secret.txt",
		"../../secret.txt",
		"docs/../../secret.txt",
		`..\secret.txt`,
		"/../secret.txt",
		"docs/../../../../../../etc/passwd",
	} {
		// The invariant that matters is not "returns an error" — a traversal
		// collapses to a harmless path inside the root, where the operation may
		// legitimately succeed as a no-op. What must never happen is reaching
		// the file outside.
		if rc, _, err := l.Download(ctx, id); err == nil {
			body, _ := io.ReadAll(rc)
			rc.Close()
			if strings.Contains(string(body), "do not leak") {
				t.Fatalf("Download(%q) returned content from outside the root", id)
			}
		}
		if f, err := l.Stat(ctx, id); err == nil && strings.Contains(f.ID, "..") {
			t.Errorf("Stat(%q) produced an escaping id %q", id, f.ID)
		}
		_ = l.Delete(ctx, id)
		_, _ = l.Upload(ctx, filepath.Dir(id), "planted.txt", 1, strings.NewReader("x"), nil)

		if _, err := os.Stat(secret); err != nil {
			t.Fatalf("%q destroyed a file outside the shared folder", id)
		}
	}

	// Nothing may have been written outside the root either.
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "planted.txt")); err == nil {
		t.Fatal("upload escaped the shared folder")
	}
	if body, _ := os.ReadFile(secret); string(body) != "do not leak" {
		t.Fatal("the file outside the shared folder was modified")
	}
}

// Absolute-looking and dot-prefixed ids must resolve inside the root, not
// escape it.
func TestLocalNormalisesIDs(t *testing.T) {
	l, _ := newTestLocal(t)
	for _, id := range []string{"hello.txt", "/hello.txt", "./hello.txt", "docs/../hello.txt"} {
		f, err := l.Stat(context.Background(), id)
		if err != nil {
			t.Errorf("Stat(%q): %v", id, err)
			continue
		}
		if f.Name != "hello.txt" {
			t.Errorf("Stat(%q) = %q", id, f.Name)
		}
	}
}

func TestLocalListAndRead(t *testing.T) {
	l, _ := newTestLocal(t)
	ctx := context.Background()

	files, err := l.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(files), files)
	}
	// Folders sort first.
	if !files[0].IsDir || files[0].Name != "docs" {
		t.Errorf("first entry = %+v, want the docs folder", files[0])
	}
	if files[1].Name != "hello.txt" || files[1].Size != 11 {
		t.Errorf("second entry = %+v", files[1])
	}

	rc, size, err := l.Download(ctx, "hello.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	body, _ := io.ReadAll(rc)
	if string(body) != "hello world" || size != 11 {
		t.Fatalf("download = %q (size %d)", body, size)
	}

	nested, err := l.List(ctx, "docs")
	if err != nil || len(nested) != 1 || nested[0].Name != "note.md" {
		t.Fatalf("nested listing = %+v, %v", nested, err)
	}
}

func TestLocalUploadIsAtomicAndUnique(t *testing.T) {
	l, root := newTestLocal(t)
	ctx := context.Background()

	f, err := l.Upload(ctx, "", "new.txt", 5, strings.NewReader("abcde"), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(root, "new.txt"))
	if err != nil || string(got) != "abcde" {
		t.Fatalf("file not written: %q %v", got, err)
	}
	if f.Size != 5 {
		t.Errorf("reported size %d", f.Size)
	}

	// A second upload of the same name must not clobber the first.
	if _, err := l.Upload(ctx, "", "new.txt", 3, strings.NewReader("xyz"), nil); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "new.txt")); string(got) != "abcde" {
		t.Errorf("original overwritten: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, "new (2).txt")); err != nil {
		t.Errorf("renamed copy not created: %v", err)
	}

	// No temp files left behind.
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".omnidrive-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestLocalMkdirRenameDelete(t *testing.T) {
	l, root := newTestLocal(t)
	ctx := context.Background()

	if _, err := l.Mkdir(ctx, "", "fresh"); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(filepath.Join(root, "fresh")); err != nil || !info.IsDir() {
		t.Fatalf("mkdir failed: %v", err)
	}
	if err := l.Rename(ctx, "fresh", "renamed"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "renamed")); err != nil {
		t.Fatalf("rename failed: %v", err)
	}
	if err := l.Delete(ctx, "renamed"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "renamed")); !os.IsNotExist(err) {
		t.Fatal("delete failed")
	}

	// Separators in a name would let a rename write outside the folder.
	if _, err := l.Mkdir(ctx, "", "a/b"); err == nil {
		t.Error("mkdir accepted a path separator")
	}
	if err := l.Rename(ctx, "hello.txt", "../escaped.txt"); err == nil {
		t.Error("rename accepted a path separator")
	}
	if _, err := l.Upload(ctx, "", "../escaped.txt", 1, strings.NewReader("x"), nil); err == nil {
		t.Error("upload accepted a path separator")
	}
	// And the shared folder itself must not be removable.
	if err := l.Delete(ctx, ""); err == nil {
		t.Error("deleting the root was allowed")
	}
}

func TestLocalRejectsBadRoot(t *testing.T) {
	if _, err := newLocal(Config{Creds: Credentials{"root": ""}}); err == nil {
		t.Error("empty root accepted")
	}
	if _, err := newLocal(Config{Creds: Credentials{"root": filepath.Join(t.TempDir(), "nope")}}); err == nil {
		t.Error("missing folder accepted")
	}
	file := filepath.Join(t.TempDir(), "afile")
	_ = os.WriteFile(file, []byte("x"), 0o644)
	if _, err := newLocal(Config{Creds: Credentials{"root": file}}); err == nil {
		t.Error("a plain file was accepted as a root")
	}
}

func TestLocalQuotaReportsSomething(t *testing.T) {
	l, _ := newTestLocal(t)
	q, err := l.Quota(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// An unknown quota is legitimate; a negative one never is.
	if q.Total < 0 || q.Used < 0 {
		t.Fatalf("nonsensical quota: %+v", q)
	}
}
