package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"omnidrive/internal/store"
)

func TestParseRange(t *testing.T) {
	const total = 1000
	cases := []struct {
		header           string
		start, end       int64
		partial, wantErr bool
	}{
		{"", 0, 999, false, false},                   // no header: whole file
		{"bytes=0-499", 0, 499, true, false},         // opening window
		{"bytes=500-", 500, 999, true, false},        // seek to the middle
		{"bytes=0-", 0, 999, true, false},            // full file as a range
		{"bytes=-500", 500, 999, true, false},        // trailing bytes: MP4 moov atom
		{"bytes=990-2000", 990, 999, true, false},    // end clamped to the file
		{"bytes=999-999", 999, 999, true, false},     // final byte
		{"bytes=0-0", 0, 0, true, false},             // probe for range support
		{"items=0-10", 0, 999, false, false},         // unknown unit: ignore
		{"bytes=0-99,200-299", 0, 999, false, false}, // multi-range: send it all
		{"bytes=1000-", 0, 0, false, true},           // start past the end
		{"bytes=500-100", 0, 0, false, true},         // end before start
		{"bytes=abc-def", 0, 0, false, true},         // nonsense
		{"bytes=-0", 0, 0, false, true},              // zero-length suffix
	}
	for _, tc := range cases {
		start, end, partial, err := parseRange(tc.header, total)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseRange(%q) should have failed, got %d-%d", tc.header, start, end)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseRange(%q): unexpected error %v", tc.header, err)
			continue
		}
		if start != tc.start || end != tc.end || partial != tc.partial {
			t.Errorf("parseRange(%q) = %d-%d partial=%v, want %d-%d partial=%v",
				tc.header, start, end, partial, tc.start, tc.end, tc.partial)
		}
	}
}

func TestContentTypeForPlayback(t *testing.T) {
	// A phone's mime table routinely lacks exactly the types playback needs,
	// and an octet-stream will not autoplay in a <video> element.
	want := map[string]string{
		"film.mp4": "video/mp4", "clip.mkv": "video/x-matroska",
		"song.mp3": "audio/mpeg", "song.flac": "audio/flac",
		"pic.jpg": "image/jpeg", "pic.HEIC": "image/heic",
		"doc.pdf": "application/pdf", "unknown.zzz": "application/octet-stream",
	}
	for name, ct := range want {
		if got := contentTypeFor(name); got != ct {
			t.Errorf("contentTypeFor(%q) = %q, want %q", name, got, ct)
		}
	}
}

// The whole point: a player must be able to seek without downloading the file.
func TestStreamServesByteRanges(t *testing.T) {
	dir := t.TempDir()
	// Recognisable content so a wrong offset is obvious.
	content := []byte(strings.Repeat("0123456789", 500)) // 5000 bytes
	if err := os.WriteFile(filepath.Join(dir, "movie.mp4"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	st, err := store.Open(t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	srv := New(Options{Version: "test", Store: st})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	var acc map[string]any
	body := strings.NewReader(fmt.Sprintf(
		`{"kind":"local","label":"Disk","fields":{"root":%q}}`, filepath.ToSlash(dir)))
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/connect/direct", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&acc); err != nil {
		t.Fatalf("connect: %v", err)
	}
	resp.Body.Close()
	id, _ := acc["id"].(string)
	if id == "" {
		t.Fatalf("no account created: %v", acc)
	}

	streamURL := ts.URL + "/api/files/stream?account=" + id + "&id=movie.mp4"

	t.Run("advertises range support and a playable type", func(t *testing.T) {
		resp, err := ts.Client().Get(streamURL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if got := resp.Header.Get("Accept-Ranges"); got != "bytes" {
			t.Errorf("Accept-Ranges = %q; without it players refuse to seek", got)
		}
		if got := resp.Header.Get("Content-Type"); got != "video/mp4" {
			t.Errorf("Content-Type = %q, want video/mp4", got)
		}
		if got := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(got, "inline") {
			t.Errorf("Content-Disposition = %q, want inline (attachment would download it)", got)
		}
	})

	t.Run("serves a middle window", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, streamURL, nil)
		req.Header.Set("Range", "bytes=1000-1099")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("status = %d, want 206", resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Range"); got != "bytes 1000-1099/5000" {
			t.Errorf("Content-Range = %q", got)
		}
		got := readAllString(t, resp)
		if len(got) != 100 {
			t.Fatalf("got %d bytes, want 100", len(got))
		}
		if got != string(content[1000:1100]) {
			t.Errorf("wrong window returned:\n got %q\nwant %q", got, content[1000:1100])
		}
	})

	t.Run("serves a suffix range", func(t *testing.T) {
		// How players locate an MP4 index stored at the end of the file.
		req, _ := http.NewRequest(http.MethodGet, streamURL, nil)
		req.Header.Set("Range", "bytes=-50")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusPartialContent {
			t.Fatalf("status = %d, want 206", resp.StatusCode)
		}
		if got := readAllString(t, resp); got != string(content[4950:]) {
			t.Errorf("suffix range wrong: %q", got)
		}
	})

	t.Run("whole file without a range header", func(t *testing.T) {
		resp, err := ts.Client().Get(streamURL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if got := readAllString(t, resp); len(got) != len(content) {
			t.Fatalf("got %d bytes, want %d", len(got), len(content))
		}
	})

	t.Run("rejects an unsatisfiable range", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, streamURL, nil)
		req.Header.Set("Range", "bytes=99999-")
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
			t.Fatalf("status = %d, want 416", resp.StatusCode)
		}
		if got := resp.Header.Get("Content-Range"); got != "bytes */5000" {
			t.Errorf("Content-Range = %q, want bytes */5000", got)
		}
	})
}

func readAllString(t *testing.T, resp *http.Response) string {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}
