package app

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed web/*
var embeddedAdminAssets embed.FS

var adminStaticFS = mustSubFS(embeddedAdminAssets, "web")

func mustSubFS(root embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(root, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

func serveAdminHTML(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	http.ServeFileFS(w, r, adminStaticFS, "admin.html")
}

func serveAdminStatic(w http.ResponseWriter, r *http.Request) {
	http.StripPrefix("/_admin/static/", http.FileServerFS(adminStaticFS)).ServeHTTP(w, r)
}
