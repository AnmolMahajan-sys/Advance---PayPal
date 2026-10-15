// Package web serves the single-page dashboard embedded in the binary.
package web

import (
	"embed"
	"net/http"
)

//go:embed index.html
var files embed.FS

func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := files.ReadFile("index.html")
		if err != nil {
			http.Error(w, "dashboard missing", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(b)
	})
}
