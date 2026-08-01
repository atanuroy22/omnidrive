// Package web embeds the browser UI into the binary. Nothing is read from
// disk at runtime, so the single executable is genuinely self-contained.
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed dist
var Files embed.FS

// SPAHandler serves static assets and falls back to index.html for unknown
// paths, so client-side routes survive a page reload.
func SPAHandler(files fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(files))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clean := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if clean == "" || clean == "." {
			clean = "index.html"
		}

		f, err := files.Open(clean)
		if err != nil {
			serveIndex(w, r, files)
			return
		}
		info, statErr := f.Stat()
		f.Close()
		if statErr != nil || info.IsDir() {
			serveIndex(w, r, files)
			return
		}

		// Hashed assets would let us cache forever, but this UI ships whole
		// with the binary — revalidating keeps an upgraded binary from serving
		// a stale app out of the browser cache.
		w.Header().Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	})
}

func serveIndex(w http.ResponseWriter, r *http.Request, files fs.FS) {
	b, err := fs.ReadFile(files, "index.html")
	if err != nil {
		http.Error(w, "UI not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}
