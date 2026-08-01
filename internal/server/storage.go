package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"omnidrive/internal/provider"
	"omnidrive/internal/store"
)

// Local storage discovery.
//
// Rather than asking the user to type "/storage/emulated/0" from memory, offer
// the volumes this device actually has. Android names SD cards by UUID
// (/storage/1A2B-3C4D), which nobody can be expected to know.

type storageVolume struct {
	Label    string `json:"label"`
	Path     string `json:"path"`
	Readable bool   `json:"readable"`
	Entries  int    `json:"entries"`
	Primary  bool   `json:"primary,omitempty"`
}

func (s *Server) handleStorageList(w http.ResponseWriter, r *http.Request) {
	volumes := discoverVolumes()
	// Report whether anything is actually usable, so the UI can offer the
	// permission prompt rather than an empty list.
	usable := 0
	for _, v := range volumes {
		if v.Readable {
			usable++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"volumes":     volumes,
		"platform":    runtime.GOOS,
		"readable":    usable,
		"needsAccess": usable == 0 && len(volumes) > 0,
	})
}

// handleStorageAdd connects a device volume as a drive. With no path it adds
// the primary volume, which is what the UI calls after the user grants
// "All files access".
func (s *Server) handleStorageAdd(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path  string `json:"path"`
		Label string `json:"label"`
	}
	// A body is optional here.
	if r.ContentLength > 0 && !decodeBody(w, r, &body) {
		return
	}
	added, err := s.addVolumes(body.Path, body.Label)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"added": added, "count": len(added)})
}

// addVolumes connects one folder, or the device's primary volumes when path is
// empty.
func (s *Server) addVolumes(path, label string) ([]string, error) {
	var candidates []storageVolume

	if path != "" {
		// Any folder may be added, not only a whole discovered volume — being
		// able to share just ~/Pictures is often what you actually want.
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("cannot open %s: %w", path, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("%s is not a folder", path)
		}
		if _, err := os.ReadDir(path); err != nil {
			return nil, fmt.Errorf("cannot read %s — on Android, grant \"All files access\" first (%w)", path, err)
		}
		if label == "" {
			label = filepath.Base(strings.TrimRight(path, `/\`))
			if label == "" || label == "." {
				label = path
			}
		}
		candidates = []storageVolume{{Label: label, Path: path, Readable: true}}
	} else {
		// Exactly one: the device's main storage. Adding every "primary"
		// candidate is how the same files ended up listed twice.
		for _, v := range discoverVolumes() {
			if v.Readable && v.Primary {
				candidates = append(candidates, v)
				break
			}
		}
		if len(candidates) == 0 {
			return nil, errors.New("no readable storage found — grant \"All files access\" first")
		}
	}

	// Never add the same folder twice.
	existing := map[string]bool{}
	for _, a := range s.st.Accounts() {
		if a.Kind == provider.KindLocal {
			existing[filepath.Clean(a.Creds["root"])] = true
		}
	}

	var added []string
	for _, v := range candidates {
		if existing[filepath.Clean(v.Path)] {
			continue
		}
		// Record the folder as the account's display name so two local drives
		// can be told apart in the UI.
		acc, err := s.st.AddAccount(provider.KindLocal, v.Label,
			provider.Credentials{"root": v.Path, provider.CredAccountName: v.Path})
		if err != nil {
			return added, err
		}
		if drv, derr := s.driver(acc); derr == nil {
			if q, qerr := drv.Quota(context.Background()); qerr == nil {
				_ = s.st.SetQuota(acc.ID, q)
			}
		}
		added = append(added, v.Label)
	}
	return added, nil
}

// EnsureLocalStorage connects the device's own storage the first time it is
// readable, so a phone appears as a drive without the user hunting for a
// button. It runs once: if the drive is later removed on purpose, it stays
// removed.
func (s *Server) EnsureLocalStorage() {
	if s.st.Settings().LocalAutoAdded {
		return
	}
	// Only meaningful where "this device" is a place you keep files.
	if runtime.GOOS != "android" && !isAndroidRuntime() {
		return
	}
	added, err := s.addVolumes("", "")
	if err != nil || len(added) == 0 {
		// Not yet permitted; try again on a later start or when the UI asks.
		return
	}
	_ = s.st.Mutate(func(st *store.State) error {
		st.Settings.LocalAutoAdded = true
		return nil
	})
	log.Printf("added device storage: %s", strings.Join(added, ", "))
}

// isAndroidRuntime detects Android when built as a plain linux binary, which
// is how this ships (GOOS=android would need the NDK).
func isAndroidRuntime() bool {
	if os.Getenv("ANDROID_ROOT") != "" || os.Getenv("ANDROID_DATA") != "" {
		return true
	}
	_, err := os.Stat("/system/build.prop")
	return err == nil
}

func discoverVolumes() []storageVolume {
	var out []storageVolume
	seen := map[string]bool{}
	var roots []string

	add := func(label, path string, primary bool) {
		if path == "" {
			return
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return
		}
		// Resolve symlinks so /sdcard and /storage/emulated/0 do not both
		// appear as separate volumes.
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			abs = resolved
		}
		if seen[abs] {
			return
		}
		// Nor should a folder that already sits inside a listed volume: it
		// would offer the same files under a second name.
		for _, root := range roots {
			if isWithin(abs, root) {
				return
			}
		}
		info, err := os.Stat(abs)
		if err != nil || !info.IsDir() {
			return
		}
		seen[abs] = true
		roots = append(roots, abs)

		v := storageVolume{Label: label, Path: abs, Primary: primary}
		// Reading it is the only honest test of whether the permission is
		// actually granted — Android happily reports the directory as existing
		// while refusing to list it.
		if entries, err := os.ReadDir(abs); err == nil {
			v.Readable = true
			v.Entries = len(entries)
		}
		out = append(out, v)
	}

	if runtime.GOOS == "windows" {
		for drive := 'C'; drive <= 'Z'; drive++ {
			add(string(drive)+":", string(drive)+`:\`, drive == 'C')
		}
		if home, err := os.UserHomeDir(); err == nil {
			add("Home", home, false)
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
		return out
	}

	onAndroid := isAndroidRuntime()

	// Android: the primary volume, then any removable cards beside it.
	add("Internal storage", "/storage/emulated/0", true)
	add("Internal storage", "/sdcard", true)

	if entries, err := os.ReadDir("/storage"); err == nil {
		for _, e := range entries {
			name := e.Name()
			if name == "emulated" || name == "self" || name == "container" {
				continue
			}
			// SD cards are named by UUID, e.g. "1A2B-3C4D".
			label := "SD card"
			if strings.Contains(name, "-") {
				label = "SD card (" + name + ")"
			}
			add(label, filepath.Join("/storage", name), false)
		}
	}

	// The home directory is a useful shortcut on a desktop. On Android it is
	// the app's own private folder — nothing the user put there, and never
	// what "my files" means — so it is left out entirely.
	//
	// Note this cannot key off runtime.GOOS: the Android build is compiled as
	// GOOS=linux for portability, so it reports "linux" on a phone.
	if !onAndroid {
		if home, err := os.UserHomeDir(); err == nil {
			add("Home", home, true)
		}
		for _, p := range []string{"/media", "/mnt", "/Volumes"} {
			if entries, err := os.ReadDir(p); err == nil {
				for _, e := range entries {
					if e.IsDir() {
						add(e.Name(), filepath.Join(p, e.Name()), false)
					}
				}
			}
		}
	}
	return out
}

// isWithin reports whether path sits inside root.
func isWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
