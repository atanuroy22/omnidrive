package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Deleting from phone or SD card is the one case where the file is gone for
// good the instant it happens — no vendor recycle bin sits behind it. So this
// file provides one: deletes move into a hidden folder on the same volume and
// can be put back where they came from.
//
// The move is a rename, never a copy, which is why the bin has to live inside
// the drive's own root: renaming across volumes fails, and copying a 4 GB video
// just to delete it would be absurd.
const localTrashDir = ".omnidrive-trash"

// trashRecord is the sidecar written next to each binned item. Without it a
// restore would not know where the file came from.
type trashRecord struct {
	Name     string    `json:"name"`
	Original string    `json:"original"` // id (root-relative path) it was deleted from
	Size     int64     `json:"size"`
	IsDir    bool      `json:"isDir"`
	Deleted  time.Time `json:"deleted"`
}

func (l *local) trashRoot() string { return filepath.Join(l.root, localTrashDir) }

// inTrash reports whether a resolved path is the bin or lives inside it, so the
// bin cannot be browsed, uploaded into, or deleted as if it were ordinary data.
func (l *local) inTrash(full string) bool {
	tr := l.trashRoot()
	return full == tr || strings.HasPrefix(full, tr+string(filepath.Separator))
}

// Delete moves the item into this drive's recycle bin rather than destroying
// it. Use DeletePermanently for the final step.
func (l *local) Delete(ctx context.Context, id string) error {
	full, err := l.resolve(id)
	if err != nil {
		return err
	}
	if full == l.root {
		return errors.New("local: refusing to delete the shared folder itself")
	}
	if l.inTrash(full) {
		// Already binned; a second delete means "get rid of it".
		return os.RemoveAll(full)
	}
	info, err := os.Stat(full)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(l.trashRoot(), 0o755); err != nil {
		return fmt.Errorf("local: cannot create the recycle bin: %w", err)
	}

	key := l.newTrashKey(info.Name())
	dest := filepath.Join(l.trashRoot(), key)
	if err := os.Rename(full, dest); err != nil {
		return fmt.Errorf("local: could not move %q to the recycle bin: %w", info.Name(), err)
	}

	rec := trashRecord{
		Name:     info.Name(),
		Original: l.idFor(full),
		IsDir:    info.IsDir(),
		Deleted:  time.Now(),
	}
	if !info.IsDir() {
		rec.Size = info.Size()
	}
	// If the sidecar cannot be written the file is still safe in the bin; it
	// just loses its original location, so restore falls back to the root.
	if b, err := json.Marshal(rec); err == nil {
		_ = os.WriteFile(dest+".json", b, 0o644)
	}
	return nil
}

// newTrashKey picks a name that is free inside the bin. Two files called
// "photo.jpg" deleted from different folders must not collide.
func (l *local) newTrashKey(name string) string {
	base := strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + sanitiseTrashName(name)
	key := base
	for i := 1; ; i++ {
		if _, err := os.Lstat(filepath.Join(l.trashRoot(), key)); os.IsNotExist(err) {
			return key
		}
		key = base + "-" + strconv.Itoa(i)
	}
}

func sanitiseTrashName(name string) string {
	name = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\:*?"<>|`, r) || r < 0x20 {
			return '_'
		}
		return r
	}, name)
	// Keep the key short enough that the ".json" sidecar still fits comfortably
	// within the 255-byte limit most Android filesystems impose.
	if len(name) > 120 {
		name = name[:120]
	}
	if name == "" {
		name = "item"
	}
	return name
}

// DeletePermanently destroys the item outright, whether it is still in place or
// already sitting in the bin.
func (l *local) DeletePermanently(ctx context.Context, id string) error {
	full, err := l.resolve(id)
	if err != nil {
		return err
	}
	if full == l.root {
		return errors.New("local: refusing to delete the shared folder itself")
	}
	if err := os.RemoveAll(full); err != nil {
		return err
	}
	if l.inTrash(full) {
		_ = os.Remove(full + ".json")
	}
	return nil
}

// ListTrash reports what is recoverable, newest first.
func (l *local) ListTrash(ctx context.Context) ([]TrashItem, error) {
	entries, err := os.ReadDir(l.trashRoot())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // nothing has ever been deleted here
		}
		return nil, err
	}

	out := make([]TrashItem, 0, len(entries))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			continue // sidecar, not an item
		}
		full := filepath.Join(l.trashRoot(), e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}

		it := TrashItem{
			ID:    filepath.ToSlash(filepath.Join(localTrashDir, e.Name())),
			Name:  e.Name(),
			IsDir: info.IsDir(),
			Size:  info.Size(),
		}
		if info.IsDir() {
			it.Size = 0
		}
		it.Deleted = info.ModTime()

		if b, err := os.ReadFile(full + ".json"); err == nil {
			var rec trashRecord
			if json.Unmarshal(b, &rec) == nil && rec.Name != "" {
				it.Name = rec.Name
				it.OriginalPath = rec.Original
				it.Deleted = rec.Deleted
				it.IsDir = rec.IsDir
				if !rec.IsDir {
					it.Size = rec.Size
				}
			}
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Deleted.After(out[j].Deleted) })
	return out, nil
}

// RestoreTrash puts an item back where it was deleted from, recreating any
// folders that have since disappeared and never overwriting something that has
// taken its place.
func (l *local) RestoreTrash(ctx context.Context, id string) error {
	full, err := l.resolve(id)
	if err != nil {
		return err
	}
	if !l.inTrash(full) || full == l.trashRoot() {
		return errors.New("local: that item is not in the recycle bin")
	}
	if _, err := os.Lstat(full); err != nil {
		return err
	}

	dest := ""
	if b, err := os.ReadFile(full + ".json"); err == nil {
		var rec trashRecord
		if json.Unmarshal(b, &rec) == nil && rec.Original != "" {
			if p, err := l.resolve(rec.Original); err == nil && !l.inTrash(p) {
				dest = p
			}
		}
	}
	if dest == "" {
		// No usable record: the best we can do is drop it at the top level.
		dest = filepath.Join(l.root, sanitiseTrashName(filepath.Base(full)))
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("local: cannot recreate %s: %w", filepath.Dir(dest), err)
	}
	dest = uniquePath(dest)
	if err := os.Rename(full, dest); err != nil {
		return fmt.Errorf("local: could not restore %q: %w", filepath.Base(dest), err)
	}
	_ = os.Remove(full + ".json")
	return nil
}

// EmptyTrash discards the whole bin.
func (l *local) EmptyTrash(ctx context.Context) error {
	if err := os.RemoveAll(l.trashRoot()); err != nil {
		return err
	}
	// Recreate it so the bin keeps working without a later MkdirAll race.
	return os.MkdirAll(l.trashRoot(), 0o755)
}

// TrashSize totals what the bin is holding on to, so the UI can say how much
// space emptying it would give back.
func (l *local) TrashSize(ctx context.Context) (int64, int, error) {
	var total int64
	var count int
	items, err := l.ListTrash(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, it := range items {
		count++
		if it.IsDir {
			p, err := l.resolve(it.ID)
			if err != nil {
				continue
			}
			_ = filepath.WalkDir(p, func(_ string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() {
					return nil //nolint:nilerr // an unreadable entry must not abort the tally
				}
				if info, err := d.Info(); err == nil {
					total += info.Size()
				}
				return nil
			})
			continue
		}
		total += it.Size
	}
	return total, count, nil
}
