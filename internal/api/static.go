package api

import (
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// spaHandler serves the built web UI. Unknown paths fall back to index.html
// so client side routes work; requests from mobile browsers are served from
// mobileDir when configured (same behaviour as the reference nginx config).
func spaHandler(dir, mobileDir string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		root := dir
		if mobileDir != "" && strings.Contains(r.UserAgent(), "Mobile") {
			root = mobileDir
		}
		clean := path.Clean("/" + r.URL.Path)
		file := filepath.Join(root, filepath.FromSlash(clean))
		if info, err := os.Stat(file); err == nil && !info.IsDir() {
			if strings.Contains(clean, "/assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			http.ServeFile(w, r, file)
			return
		}
		if ext := path.Ext(clean); ext != "" && ext != ".html" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFile(w, r, filepath.Join(root, "index.html"))
	})
}
