package host

import (
	"cmp"
	"errors"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

func within(root, name string) bool {
	relative, _ := filepath.Rel(root, name)
	return filepath.IsLocal(relative)
}

func frontend(directory, source string) (http.Handler, error) {
	if directory == "" {
		return http.NotFoundHandler(), nil
	}
	source, err := filepath.EvalSymlinks(source)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(directory) {
		directory = filepath.Join(source, directory)
	}
	if !within(source, directory) {
		return nil, errors.New("frontend must be a directory inside the application")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains("/"+r.URL.Path, "/.") || strings.ContainsAny(r.URL.Path, "\\\x00") ||
			strings.Contains(strings.ToLower(r.URL.EscapedPath()), "%2f") {
			http.NotFound(w, r)
			return
		}
		root, err := filepath.EvalSymlinks(directory)
		if err != nil || !within(source, root) {
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
		http.ServeContent(w, r, info.Name(), info.ModTime(), file)
	}), nil
}
