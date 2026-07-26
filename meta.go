package main

import (
	"database/sql"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"strings"
)

func updateMetaValue(tx *sql.Tx, name string, value string) error {
	const query = `
		insert into meta(name, value) values(?, ?)
		on conflict(name) do update set value = excluded.value
	`

	_, err := tx.Exec(query, name, value)
	if err != nil {
		return fmt.Errorf("updateMetaValue.Exec: %w", err)
	}

	return nil
}

func getMetaValue(tx *sql.Tx, name string) (string, error) {
	const query = `
		select value from meta where name = ?
	`

	var v string

	err := tx.QueryRow(query, name).Scan(&v)
	if err != nil {
		return v, fmt.Errorf("getMetaValue.Scan: %w", err)
	}

	return v, nil
}

func neuter(next http.Handler) http.Handler {
	gzAvailable := map[string]string{}
	err := fs.WalkDir(staticFS, "static", func(path string, d fs.DirEntry, err error) error {
		if !d.IsDir() {
			if strings.HasSuffix(d.Name(), ".gz") {
				gzAvailable[strings.Replace(path, ".gz", "", 1)] = path
			}
		}
		return nil
	})
	if err != nil {
		log.Fatalf("neuter.WalkDir: %s", err)
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}

		if gzPath, ok := gzAvailable[strings.TrimPrefix(r.URL.Path, "/")]; ok {
			r.URL.Path = gzPath
			split := strings.Split(r.URL.Path, ".")
			ext := split[len(split)-2]
			w.Header().Add("Content-Type", mime.TypeByExtension("."+ext))
			w.Header().Add("Content-Encoding", "gzip")
		}

		next.ServeHTTP(w, r)
	})
}
