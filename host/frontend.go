package host

import (
	"bytes"
	"cmp"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/holon-sh/protocol"
	"github.com/holon-sh/runtime/metadata"
)

func within(root, name string) bool {
	relative, _ := filepath.Rel(root, name)
	return filepath.IsLocal(relative)
}

func frontend(directory string) (http.Handler, error) {
	if directory == "" {
		return http.NotFoundHandler(), nil
	}
	var site *metadata.Runtime
	if raw := os.Getenv(metadata.Environment); raw != "" {
		site = new(metadata.Runtime)
		if err := protocol.DecodeJSONNonNull([]byte(raw), site); err != nil {
			return nil, err
		}
		if err := site.Validate(); err != nil {
			return nil, err
		}
	}
	application, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	application, err = filepath.EvalSymlinks(application)
	if err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(directory)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() || !within(application, root) {
		return nil, errors.New("frontend must be a directory inside the application")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains("/"+r.URL.Path, "/.") || strings.ContainsAny(r.URL.Path, "\\\x00") ||
			strings.Contains(strings.ToLower(r.URL.EscapedPath()), "%2f") {
			http.NotFound(w, r)
			return
		}
		name, err := filepath.EvalSymlinks(filepath.Join(root, r.URL.Path))
		if err != nil || !within(root, name) {
			http.NotFound(w, r)
			return
		}
		info, err := os.Stat(name)
		if err == nil && info.IsDir() {
			name, err = filepath.EvalSymlinks(filepath.Join(name, "index.html"))
		}
		if err != nil || !within(root, name) {
			http.NotFound(w, r)
			return
		}
		file, err := os.Open(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer file.Close()
		info, err = file.Stat()
		if err != nil || !info.Mode().IsRegular() {
			http.NotFound(w, r)
			return
		}
		contentType := cmp.Or(mime.TypeByExtension(filepath.Ext(name)), "application/octet-stream")
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if site != nil && strings.HasPrefix(contentType, "text/html") {
			document, err := io.ReadAll(file)
			if err != nil {
				http.Error(w, "Frontend unavailable", http.StatusInternalServerError)
				return
			}
			document = metadata.Inherit(document, *site, r.URL.Path)
			http.ServeContent(w, r, info.Name(), time.Time{}, bytes.NewReader(document))
			return
		}
		http.ServeContent(w, r, info.Name(), info.ModTime(), file)
	}), nil
}
