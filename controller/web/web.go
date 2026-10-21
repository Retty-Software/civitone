// Package web embeds the console. The mockup in docs/ becomes the real app
// here, screen by screen, as the controller grows API to back it.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var static embed.FS

func Handler() http.Handler {
	sub, _ := fs.Sub(static, "static")
	files := http.FileServer(http.FS(sub))
	// Embedded files have no mod time, so without this browsers cache the
	// console heuristically and keep running old code after an update.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
}
