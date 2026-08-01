package provider

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func newEmptyLocal(t *testing.T) (*local, string) {
	t.Helper()
	root := t.TempDir()
	drv, err := Open(KindLocal, Config{Creds: Credentials{"root": root}})
	if err != nil {
		t.Fatalf("open local: %v", err)
	}
	return drv.(*local), root
}

// A delete on the phone must be undoable: Android has no recycle bin of its
// own, so a mis-tap here would otherwise destroy the file outright.
func TestLocalDeleteIsRecoverable(t *testing.T) {
	l, root := newEmptyLocal(t)
	ctx := context.Background()

	if err := os.MkdirAll(filepath.Join(root, "DCIM", "Camera"), 0o755); err != nil {
		t.Fatal(err)
	}
	orig := filepath.Join(root, "DCIM", "Camera", "holiday.jpg")
	if err := os.WriteFile(orig, []byte("photo-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := l.Delete(ctx, "DCIM/Camera/holiday.jpg"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(orig); !os.IsNotExist(err) {
		t.Fatal("file is still in place after delete")
	}

	items, err := l.ListTrash(ctx)
	if err != nil {
		t.Fatalf("list trash: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("trash holds %d items, want 1", len(items))
	}
	if items[0].Name != "holiday.jpg" {
		t.Errorf("trashed name = %q, want the original name", items[0].Name)
	}
	if items[0].OriginalPath != "DCIM/Camera/holiday.jpg" {
		t.Errorf("original path = %q, want the folder it came from", items[0].OriginalPath)
	}
	if items[0].Size != int64(len("photo-bytes")) {
		t.Errorf("size = %d, want %d", items[0].Size, len("photo-bytes"))
	}

	if err := l.RestoreTrash(ctx, items[0].ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := os.ReadFile(orig)
	if err != nil {
		t.Fatalf("file did not come back: %v", err)
	}
	if string(got) != "photo-bytes" {
		t.Errorf("restored contents = %q, want the original bytes", got)
	}
	if items, _ := l.ListTrash(ctx); len(items) != 0 {
		t.Errorf("trash still holds %d items after restore", len(items))
	}
}

// Restoring into a folder that has since been deleted must still work, and must
// not overwrite whatever has taken the file's place.
func TestLocalRestoreRebuildsPathAndKeepsBothCopies(t *testing.T) {
	l, root := newEmptyLocal(t)
	ctx := context.Background()

	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "notes.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := l.Delete(ctx, "docs/notes.txt"); err != nil {
		t.Fatal(err)
	}
	// The folder disappears, and a new file claims the same name.
	if err := os.RemoveAll(filepath.Join(root, "docs")); err != nil {
		t.Fatal(err)
	}

	items, _ := l.ListTrash(ctx)
	if len(items) != 1 {
		t.Fatalf("trash holds %d items, want 1", len(items))
	}
	if err := l.RestoreTrash(ctx, items[0].ID); err != nil {
		t.Fatalf("restore after the folder was removed: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "docs", "notes.txt")); err != nil || string(b) != "v1" {
		t.Fatalf("restored file = %q, %v; want the original back in a recreated folder", b, err)
	}

	// Now the same name is occupied: the restore must not clobber it.
	if err := l.Delete(ctx, "docs/notes.txt"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "docs", "notes.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	items, _ = l.ListTrash(ctx)
	if err := l.RestoreTrash(ctx, items[0].ID); err != nil {
		t.Fatalf("restore over an occupied name: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "docs", "notes.txt")); string(b) != "v2" {
		t.Errorf("the newer file was overwritten; contents = %q, want v2", b)
	}
	entries, _ := os.ReadDir(filepath.Join(root, "docs"))
	if len(entries) != 2 {
		t.Errorf("docs holds %d files, want both the restored and the current one", len(entries))
	}
}

// The bin is an implementation detail of this drive; browsing must not show it,
// or the user sees a mysterious folder full of timestamped names.
func TestLocalTrashIsHiddenFromListings(t *testing.T) {
	l, root := newEmptyLocal(t)
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := l.Delete(ctx, "a.txt"); err != nil {
		t.Fatal(err)
	}
	files, err := l.List(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Name == localTrashDir {
			t.Fatalf("the recycle bin appears in the file listing as %q", f.Name)
		}
	}
	if len(files) != 0 {
		t.Errorf("root lists %d entries, want none", len(files))
	}
}

// Emptying and purging both have to actually remove the bytes, or "free up
// space" frees nothing.
func TestLocalPurgeAndEmpty(t *testing.T) {
	l, root := newEmptyLocal(t)
	ctx := context.Background()

	for _, n := range []string{"one.bin", "two.bin", "three.bin"} {
		if err := os.WriteFile(filepath.Join(root, n), make([]byte, 1024), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := l.Delete(ctx, n); err != nil {
			t.Fatal(err)
		}
	}

	size, count, err := l.TrashSize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 || size != 3*1024 {
		t.Fatalf("bin holds %d items / %d bytes, want 3 / 3072", count, size)
	}

	items, _ := l.ListTrash(ctx)
	if err := l.DeletePermanently(ctx, items[0].ID); err != nil {
		t.Fatalf("purge one: %v", err)
	}
	if _, count, _ := l.TrashSize(ctx); count != 2 {
		t.Errorf("bin holds %d items after purging one, want 2", count)
	}

	if err := l.EmptyTrash(ctx); err != nil {
		t.Fatalf("empty: %v", err)
	}
	if _, count, _ := l.TrashSize(ctx); count != 0 {
		t.Errorf("bin holds %d items after emptying", count)
	}
}

// Two files with the same name deleted from different folders must both survive
// in the bin, and each restore to its own place.
func TestLocalTrashKeepsNameCollisionsApart(t *testing.T) {
	l, root := newEmptyLocal(t)
	ctx := context.Background()

	for _, dir := range []string{"a", "b"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, dir, "same.txt"), []byte(dir), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := l.Delete(ctx, dir+"/same.txt"); err != nil {
			t.Fatal(err)
		}
	}

	items, _ := l.ListTrash(ctx)
	if len(items) != 2 {
		t.Fatalf("bin holds %d items, want both copies", len(items))
	}
	for _, it := range items {
		if err := l.RestoreTrash(ctx, it.ID); err != nil {
			t.Fatalf("restore %s: %v", it.OriginalPath, err)
		}
	}
	for _, dir := range []string{"a", "b"} {
		b, err := os.ReadFile(filepath.Join(root, dir, "same.txt"))
		if err != nil || string(b) != dir {
			t.Errorf("%s/same.txt = %q, %v; want it back with its own contents", dir, b, err)
		}
	}
}

// The bin is reached through its own screen. Anything arriving through the
// normal path API must not be able to reach outside the drive root either.
func TestLocalRestoreRejectsItemsOutsideTheBin(t *testing.T) {
	l, root := newEmptyLocal(t)
	ctx := context.Background()

	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"real.txt", "", "../escape", localTrashDir} {
		if err := l.RestoreTrash(ctx, id); err == nil {
			t.Errorf("RestoreTrash(%q) was allowed; only binned items may be restored", id)
		}
	}
}
