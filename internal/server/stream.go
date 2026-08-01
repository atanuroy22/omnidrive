package server

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"omnidrive/internal/provider"
)

// Streaming playback.
//
// Tapping a film should start it, not download two gigabytes first. That needs
// HTTP Range: the browser asks for a window, seeks by asking for a different
// window, and never holds the whole file. Every backend here can serve a byte
// range, so the endpoint below is a thin translation between the two.

// mimeOverrides fills gaps in the platform's mime table, which on a phone is
// often missing exactly the types that matter for playback.
var mimeOverrides = map[string]string{
	".mp4": "video/mp4", ".m4v": "video/mp4", ".mov": "video/quicktime",
	".mkv": "video/x-matroska", ".webm": "video/webm", ".avi": "video/x-msvideo",
	".3gp": "video/3gpp", ".ts": "video/mp2t", ".flv": "video/x-flv",
	".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".aac": "audio/aac",
	".flac": "audio/flac", ".wav": "audio/wav", ".ogg": "audio/ogg",
	".opus": "audio/opus", ".wma": "audio/x-ms-wma",
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png",
	".gif": "image/gif", ".webp": "image/webp", ".avif": "image/avif",
	".bmp": "image/bmp", ".svg": "image/svg+xml", ".heic": "image/heic",
	".pdf": "application/pdf",
	".txt": "text/plain; charset=utf-8", ".md": "text/plain; charset=utf-8",
	".log": "text/plain; charset=utf-8", ".json": "application/json",
	".csv": "text/csv; charset=utf-8", ".xml": "application/xml",
}

func contentTypeFor(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if ct, ok := mimeOverrides[ext]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// handleStream serves a file inline with Range support, so <video> can seek
// and playback starts immediately.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	acc, drv, ok := s.account(w, q.Get("account"))
	if !ok {
		return
	}
	id := q.Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, errors.New("id is required"))
		return
	}

	meta, err := drv.Stat(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if meta.IsDir {
		writeErr(w, http.StatusBadRequest, errors.New("that is a folder"))
		return
	}
	name := meta.Name
	if name == "" {
		name = q.Get("name")
	}
	total := meta.Size

	h := w.Header()
	h.Set("Content-Type", contentTypeFor(name))
	// Inline, not attachment: this is for viewing, not saving.
	h.Set("Content-Disposition", "inline; filename=\""+sanitizeASCII(name)+"\"")
	// Everything here is private to this device, but a media element will
	// re-request ranges constantly; letting it cache briefly avoids hammering
	// the provider during a seek.
	h.Set("Cache-Control", "private, max-age=60")

	ranger, canRange := drv.(provider.RangeDownloader)
	if !canRange || total <= 0 {
		// No range support, or an unknown length: fall back to a plain stream.
		// Playback still works, seeking does not.
		rc, size, err := drv.Download(r.Context(), id)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		defer rc.Close()
		if size > 0 {
			h.Set("Content-Length", strconv.FormatInt(size, 10))
		}
		_, _ = io.Copy(w, rc)
		return
	}

	h.Set("Accept-Ranges", "bytes")

	start, end, partial, err := parseRange(r.Header.Get("Range"), total)
	if err != nil {
		h.Set("Content-Range", "bytes */"+strconv.FormatInt(total, 10))
		writeErr(w, http.StatusRequestedRangeNotSatisfiable, err)
		return
	}

	rc, err := ranger.DownloadRange(r.Context(), id, start, end)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	defer rc.Close()

	length := end - start + 1
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	if partial {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, total))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}

	// A seek cancels the request mid-copy; that is normal, not an error.
	_, _ = io.CopyN(w, rc, length)

	_ = acc
}

// parseRange interprets a single-range "bytes=" header against a known total.
// Multi-range requests are answered with the whole file, which is legal and
// which no media element actually needs.
func parseRange(header string, total int64) (start, end int64, partial bool, err error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, total - 1, false, nil
	}
	if !strings.HasPrefix(header, "bytes=") {
		return 0, total - 1, false, nil
	}
	spec := strings.TrimPrefix(header, "bytes=")
	if strings.Contains(spec, ",") {
		return 0, total - 1, false, nil
	}

	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false, errors.New("malformed range")
	}
	fromText := strings.TrimSpace(spec[:dash])
	toText := strings.TrimSpace(spec[dash+1:])

	switch {
	case fromText == "" && toText == "":
		return 0, 0, false, errors.New("malformed range")

	case fromText == "":
		// "bytes=-500" means the final 500 bytes, which is how some players
		// read a trailing index (MP4 moov atom at the end of the file).
		n, convErr := strconv.ParseInt(toText, 10, 64)
		if convErr != nil || n <= 0 {
			return 0, 0, false, errors.New("malformed suffix range")
		}
		if n > total {
			n = total
		}
		start, end = total-n, total-1

	default:
		start, err = strconv.ParseInt(fromText, 10, 64)
		if err != nil || start < 0 {
			return 0, 0, false, errors.New("malformed range start")
		}
		if start >= total {
			return 0, 0, false, fmt.Errorf("range start %d beyond end of file", start)
		}
		if toText == "" {
			end = total - 1
		} else {
			end, err = strconv.ParseInt(toText, 10, 64)
			if err != nil {
				return 0, 0, false, errors.New("malformed range end")
			}
			if end >= total {
				end = total - 1
			}
		}
	}

	if end < start {
		return 0, 0, false, errors.New("range end before start")
	}
	return start, end, true, nil
}
