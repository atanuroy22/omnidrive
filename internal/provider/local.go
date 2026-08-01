package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// local exposes a directory on the device — internal storage, an SD card, or
// any folder — through the same interface as the cloud backends. That is what
// lets a file move from the phone to a drive (or between drives) without the
// rest of the program knowing the difference.
//
// Object IDs are slash-separated paths relative to the configured root.
type local struct {
	cfg  Config
	root string
}

func newLocal(cfg Config) (Driver, error) {
	root := strings.TrimSpace(cfg.Creds["root"])
	if root == "" {
		return nil, errors.New("local: a folder is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("local: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("local: cannot open %s: %w", abs, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("local: %s is not a folder", abs)
	}
	return &local{cfg: cfg, root: abs}, nil
}

func (l *local) Kind() Kind { return KindLocal }

// resolve maps an object ID to an absolute path, refusing anything that would
// escape the configured root. Without this a crafted id of "../.." would hand
// out the whole filesystem — the id arrives straight from an HTTP request.
func (l *local) resolve(id string) (string, error) {
	clean := filepath.Clean("/" + strings.ReplaceAll(strings.TrimSpace(id), "\\", "/"))
	full := filepath.Join(l.root, filepath.FromSlash(clean))

	// filepath.Join already applies Clean, so a traversal collapses; verifying
	// the prefix afterwards catches anything it did not.
	rel, err := filepath.Rel(l.root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("local: path %q is outside the shared folder", id)
	}
	return full, nil
}

// idFor produces the ID for a path inside the root.
func (l *local) idFor(full string) string {
	rel, err := filepath.Rel(l.root, full)
	if err != nil {
		return ""
	}
	if rel == "." {
		return ""
	}
	return filepath.ToSlash(rel)
}

func (l *local) fileFor(full string, info os.FileInfo) File {
	f := File{
		ID:       l.idFor(full),
		Name:     info.Name(),
		IsDir:    info.IsDir(),
		Modified: info.ModTime(),
	}
	if !info.IsDir() {
		f.Size = info.Size()
		f.MIME = mime.TypeByExtension(filepath.Ext(info.Name()))
	}
	return f
}

func (l *local) List(ctx context.Context, folderID string) ([]File, error) {
	dir, err := l.resolve(folderID)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsPermission(err) {
			return nil, fmt.Errorf("no permission to read %s — grant \"All files access\" "+
				"to OmniDrive in Android settings", dir)
		}
		return nil, err
	}

	out := make([]File, 0, len(entries))
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		// The recycle bin is reached through its own screen, not by browsing.
		if l.inTrash(full) {
			continue
		}
		// A broken symlink or a file removed mid-scan must not fail the whole
		// listing; phone storage changes under you constantly.
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, l.fileFor(full, info))
	}
	SortFiles(out)
	return out, nil
}

func (l *local) Stat(ctx context.Context, id string) (File, error) {
	full, err := l.resolve(id)
	if err != nil {
		return File{}, err
	}
	info, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return File{}, ErrNotFound
		}
		return File{}, err
	}
	return l.fileFor(full, info), nil
}

func (l *local) Download(ctx context.Context, id string) (io.ReadCloser, int64, error) {
	full, err := l.resolve(id)
	if err != nil {
		return nil, 0, err
	}
	info, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, ErrNotFound
		}
		return nil, 0, err
	}
	if info.IsDir() {
		return nil, 0, fmt.Errorf("local: %s is a folder", id)
	}
	f, err := os.Open(full)
	if err != nil {
		return nil, 0, err
	}
	return f, info.Size(), nil
}

// DownloadRange serves a byte window, so the player can seek.
func (l *local) DownloadRange(ctx context.Context, id string, start, end int64) (io.ReadCloser, error) {
	full, err := l.resolve(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if start > 0 {
		if _, err := f.Seek(start, io.SeekStart); err != nil {
			f.Close()
			return nil, err
		}
	}
	if end < 0 {
		return f, nil
	}
	// Bound the reader to the window but keep the file handle closable.
	return struct {
		io.Reader
		io.Closer
	}{io.LimitReader(f, end-start+1), f}, nil
}

func (l *local) Upload(ctx context.Context, parentID, name string, size int64, r io.Reader, p Progress) (File, error) {
	if strings.ContainsAny(name, `/\`) {
		return File{}, errors.New("local: name must not contain path separators")
	}
	dir, err := l.resolve(parentID)
	if err != nil {
		return File{}, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return File{}, err
	}

	dest := filepath.Join(dir, name)
	dest = uniquePath(dest)

	// Write to a temp file and rename, so an interrupted transfer never leaves
	// a half-written file looking complete.
	tmp, err := os.CreateTemp(dir, ".omnidrive-*")
	if err != nil {
		return File{}, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	written, err := io.Copy(tmp, newProgressReader(r, p))
	if err != nil {
		tmp.Close()
		return File{}, err
	}
	if err := tmp.Close(); err != nil {
		return File{}, err
	}
	if err := os.Rename(tmpName, dest); err != nil {
		return File{}, err
	}

	return File{
		ID: l.idFor(dest), Name: filepath.Base(dest),
		Size: written, Modified: time.Now(),
		MIME: mime.TypeByExtension(filepath.Ext(dest)),
	}, nil
}

// uniquePath appends " (2)", " (3)" … rather than overwriting, matching what
// every provider in this program does on a name clash.
func uniquePath(dest string) string {
	if _, err := os.Stat(dest); os.IsNotExist(err) {
		return dest
	}
	ext := filepath.Ext(dest)
	base := strings.TrimSuffix(dest, ext)
	for i := 2; i < 1000; i++ {
		cand := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
	return dest
}

func (l *local) Mkdir(ctx context.Context, parentID, name string) (File, error) {
	if strings.ContainsAny(name, `/\`) {
		return File{}, errors.New("local: name must not contain path separators")
	}
	parent, err := l.resolve(parentID)
	if err != nil {
		return File{}, err
	}
	full := filepath.Join(parent, name)
	if err := os.Mkdir(full, 0o755); err != nil && !os.IsExist(err) {
		return File{}, err
	}
	return File{ID: l.idFor(full), Name: name, IsDir: true, Modified: time.Now()}, nil
}

func (l *local) Rename(ctx context.Context, id, newName string) error {
	if strings.ContainsAny(newName, `/\`) {
		return errors.New("local: name must not contain path separators")
	}
	full, err := l.resolve(id)
	if err != nil {
		return err
	}
	dest := filepath.Join(filepath.Dir(full), newName)
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("local: %q already exists", newName)
	}
	return os.Rename(full, dest)
}

func (l *local) Quota(ctx context.Context) (Quota, error) {
	total, free, err := diskUsage(l.root)
	if err != nil || total == 0 {
		// Unknown quota is legitimate; the allocator treats it as spacious.
		return Quota{}, nil
	}
	return Quota{Used: total - free, Total: total}, nil
}

func (l *local) accountName(ctx context.Context) string { return l.root }

// Root exposes the configured folder, used when displaying the account.
func (l *local) Root() string { return l.root }
