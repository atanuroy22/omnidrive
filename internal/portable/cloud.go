package portable

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"omnidrive/internal/provider"
)

// Cloud sync stores the encrypted bundle inside a drive you have already
// connected. It is the answer to "I have eight accounts and just bought a new
// phone": connect any one of them on the new device, pull, and the other seven
// arrive with their tokens intact.
//
// The bundle is sealed with a passphrase before it ever leaves the device, so
// the provider holds ciphertext it cannot read.

// ConfigFolder is the folder created in the target drive.
const ConfigFolder = "OmniDrive"

// Push writes the sealed bundle into drv, replacing any previous copy.
func Push(ctx context.Context, drv provider.Driver, blob []byte) error {
	folderID, err := ensureFolder(ctx, drv, ConfigFolder)
	if err != nil {
		return fmt.Errorf("prepare %s folder: %w", ConfigFolder, err)
	}
	// Providers autorename on conflict, so an existing copy must go first or
	// the drive slowly fills with "config (1)", "config (2)"…
	if existing, err := findChild(ctx, drv, folderID, BundleName); err == nil {
		if delErr := drv.Delete(ctx, existing.ID); delErr != nil {
			return fmt.Errorf("replace previous bundle: %w", delErr)
		}
	}
	_, err = drv.Upload(ctx, folderID, BundleName, int64(len(blob)), bytes.NewReader(blob), nil)
	if err != nil {
		return fmt.Errorf("upload bundle: %w", err)
	}
	return nil
}

// Pull downloads the sealed bundle from drv.
func Pull(ctx context.Context, drv provider.Driver) ([]byte, error) {
	folderID, err := findFolder(ctx, drv, ConfigFolder)
	if err != nil {
		return nil, fmt.Errorf("no %s folder in this drive: %w", ConfigFolder, err)
	}
	entry, err := findChild(ctx, drv, folderID, BundleName)
	if err != nil {
		return nil, fmt.Errorf("no %s in the %s folder", BundleName, ConfigFolder)
	}
	rc, _, err := drv.Download(ctx, entry.ID)
	if err != nil {
		return nil, fmt.Errorf("download bundle: %w", err)
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, maxBundle))
}

// Status reports whether a bundle exists in drv and when it was last written.
func Status(ctx context.Context, drv provider.Driver) (exists bool, modified time.Time, size int64) {
	folderID, err := findFolder(ctx, drv, ConfigFolder)
	if err != nil {
		return false, time.Time{}, 0
	}
	entry, err := findChild(ctx, drv, folderID, BundleName)
	if err != nil {
		return false, time.Time{}, 0
	}
	return true, entry.Modified, entry.Size
}

func ensureFolder(ctx context.Context, drv provider.Driver, name string) (string, error) {
	if id, err := findFolder(ctx, drv, name); err == nil {
		return id, nil
	}
	f, err := drv.Mkdir(ctx, "", name)
	if err != nil {
		return "", err
	}
	// S3 has no real directories, so Mkdir returns a synthetic entry whose ID
	// is simply the prefix — which is exactly what List and Upload expect.
	return f.ID, nil
}

func findFolder(ctx context.Context, drv provider.Driver, name string) (string, error) {
	entries, err := drv.List(ctx, "")
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.IsDir && strings.EqualFold(e.Name, name) {
			return e.ID, nil
		}
	}
	return "", errors.New("folder not found")
}

func findChild(ctx context.Context, drv provider.Driver, folderID, name string) (provider.File, error) {
	entries, err := drv.List(ctx, folderID)
	if err != nil {
		return provider.File{}, err
	}
	for _, e := range entries {
		if !e.IsDir && strings.EqualFold(e.Name, name) {
			return e, nil
		}
	}
	return provider.File{}, errors.New("not found")
}
