package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

// indexFile is the SPA entry point and the fallback for unknown UI routes.
const indexFile = "index.html"

// staticUI serves the web UI from an fs.FS.
//
// There is no build step and the files have no hashed names, so every
// response is "Cache-Control: no-cache" with a strong ETag: browsers keep a
// copy but revalidate it, and an unchanged file costs a 304. The ETags are
// content hashes computed once, when the UI is created; the FS is therefore
// expected not to change while the server runs (an embed.FS never does).
type staticUI struct {
	fsys  fs.FS
	etags map[string]string // cleaned path (no leading slash) -> quoted ETag
}

// newStaticUI hashes every file of fsys. A nil fsys, or one without files,
// disables the UI.
func newStaticUI(fsys fs.FS, log *slog.Logger) *staticUI {
	if fsys == nil {
		return nil
	}
	u := &staticUI{fsys: fsys, etags: make(map[string]string)}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		f, err := fsys.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			return err
		}
		u.etags[p] = `"` + hex.EncodeToString(h.Sum(nil)[:16]) + `"`
		return nil
	})
	if err != nil {
		log.Error("api: cannot index the web UI files; serving none", "error", err)
		return nil
	}
	if len(u.etags) == 0 {
		return nil
	}
	if _, ok := u.etags[indexFile]; !ok {
		log.Warn("api: the web UI has no index.html; unknown routes will answer 404")
	}
	return u
}

// ServeHTTP serves the file for the request path, or index.html for
// extension-less paths that match no file (client-side routes).
func (u *staticUI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSON(w, http.StatusMethodNotAllowed, errorBody{Error: "method not allowed"})
		return
	}
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = indexFile
	}
	if _, ok := u.etags[name]; !ok {
		// An unknown path with a file extension is a missing asset; without
		// one it is a UI route.
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
		name = indexFile
	}
	etag, ok := u.etags[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := u.fsys.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()

	h := w.Header()
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", etag)
	h.Set("Content-Type", contentType(name))
	if rs, ok := f.(io.ReadSeeker); ok {
		http.ServeContent(w, r, name, time.Time{}, rs)
		return
	}
	// A file that cannot seek is read whole; UI files are small.
	data, err := io.ReadAll(f)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(data))
}

// contentTypes pins the types the UI needs, so serving does not depend on the
// host's mime database (X-Content-Type-Options: nosniff makes a wrong type
// fatal for scripts and styles).
var contentTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".mjs":   "text/javascript; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".json":  "application/json",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".ico":   "image/x-icon",
	".woff2": "font/woff2",
	".txt":   "text/plain; charset=utf-8",
	".map":   "application/json",
}

func contentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if t, ok := contentTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}
