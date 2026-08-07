package main

import (
	textTemplate "text/template"

	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

var tmplsMu sync.RWMutex
var tmpls = map[string]*template.Template{}

func parseTmpl(name string, markup string) (*template.Template, error) {
	tmplsMu.RLock()
	tmpl, ok := tmpls[name]
	tmplsMu.RUnlock()
	if ok {
		return tmpl, nil
	}

	tmpl, err := template.New(name).Parse(parseTmplRootTmpl)
	if err != nil {
		return tmpl, err
	}

	tmpl, err = tmpl.Parse(markup)
	if err != nil {
		return tmpl, err
	}

	tmplsMu.Lock()
	tmpls[name] = tmpl
	tmplsMu.Unlock()

	return tmpl, nil
}

var textTmplsMu sync.RWMutex
var textTmpls = map[string]*textTemplate.Template{}

func parseTextTmpl(name string, markup string) (*textTemplate.Template, error) {
	textTmplsMu.RLock()
	tmpl, ok := textTmpls[name]
	textTmplsMu.RUnlock()
	if ok {
		return tmpl, nil
	}

	tmpl = textTemplate.New(name)

	tmpl, err := tmpl.Parse(markup)
	if err != nil {
		return tmpl, fmt.Errorf("parseTextTmpl.Parse: %w", err)
	}

	textTmplsMu.Lock()
	textTmpls[name] = tmpl
	textTmplsMu.Unlock()

	return tmpl, nil
}

//go:embed static/*
var staticFS embed.FS

type pageCtx struct {
	Status                   string
	Auth                     authCtx
	Index                    bool
	Name                     string
	HXRequest                bool
	HXBoosted                bool
	AdminArea                bool
	Nav                      string
	UnconfirmedDomainProblem string
	UnconfirmedDomain        string
	HideUnconfirmedDomain    bool
	ShouldAttemptRedirect    bool
	Domain                   string
	ConfigFile               bool
}

func getPageCtx(r *http.Request) pageCtx {
	status := ""
	if val, ok := r.Context().Value(statusCtxKey{}).(string); ok {
		status = val
	}

	authCtx := getAuthCtx(r)

	adminURLPrefix := ""
	adminArea := false
	if strings.HasPrefix(r.URL.Path, "/admin/") {
		adminURLPrefix = strings.Split(r.URL.Path, "/")[2]
		adminArea = true
	}

	// r.Host is client-supplied and net/http admits bytes that fail to parse
	// here ("%zz", "[bad", "a:b:c"), which returned a nil URL that the
	// redirect check below dereferenced.
	hostname := ""
	if parsedURL, err := url.ParseRequestURI("https://" + r.Host); err == nil {
		hostname = parsedURL.Hostname()
	}

	return pageCtx{
		Status:                   status,
		Auth:                     authCtx,
		Index:                    r.URL.Path == "/" || r.URL.Path == "/history",
		Name:                     metaName.Load(),
		HXRequest:                r.Header.Get("HX-Request") == "true",
		HXBoosted:                r.Header.Get("HX-Boosted") == "true",
		AdminArea:                adminArea,
		Nav:                      adminURLPrefix,
		UnconfirmedDomainProblem: metaUnconfirmedDomainProblem.Load(),
		UnconfirmedDomain:        metaUnconfirmedDomain.Load(),
		HideUnconfirmedDomain:    r.URL.Path == "/admin/settings",
		ShouldAttemptRedirect: metaSSL.Load() == "true" && authCtx.ID != 0 &&
			metaDomain.Load() != "" && hostname != metaDomain.Load(),
		Domain:     metaDomain.Load(),
		ConfigFile: metaConfigFileEnabled.Load(),
	}
}

var staticETags = map[string]string{}

func neuter(next http.Handler) http.Handler {
	gzAvailable := map[string]string{}
	err := fs.WalkDir(staticFS, "static", func(path string, d fs.DirEntry, err error) error {
		if !d.IsDir() {
			if strings.HasSuffix(d.Name(), ".gz") {
				gzAvailable[strings.Replace(path, ".gz", "", 1)] = path
			}

			contents, err := staticFS.ReadFile(path)
			if err != nil {
				return fmt.Errorf("neuter.ReadFile %s: %w", path, err)
			}
			sum := sha256.Sum256(contents)
			staticETags[path] = `"` + hex.EncodeToString(sum[:16]) + `"`
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

		// embed.FS reports a zero ModTime, so ServeContent emits no
		// Last-Modified and there is nothing to revalidate against. Without a
		// freshness directive every page view re-fetched all 2.1 MB of
		// static/, monaco included. The assets are baked into the binary, so
		// they cannot change without the ETag changing with them.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		if etag, ok := staticETags[strings.TrimPrefix(r.URL.Path, "/")]; ok {
			w.Header().Set("ETag", etag)
			if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}

		// These eleven assets are embedded ONLY in their compressed form --
		// there is no plain copy to fall back to -- so a client that does not
		// advertise gzip is served a decompressed copy rather than a response
		// labelled with an encoding it never asked for.
		if gzPath, ok := gzAvailable[strings.TrimPrefix(r.URL.Path, "/")]; ok {
			split := strings.Split(gzPath, ".")
			ext := split[len(split)-2]
			w.Header().Add("Content-Type", mime.TypeByExtension("."+ext))
			w.Header().Add("Vary", "Accept-Encoding")

			if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				r.URL.Path = gzPath
				w.Header().Add("Content-Encoding", "gzip")
				next.ServeHTTP(w, r)
				return
			}

			serveDecompressed(w, gzPath)
			return
		}

		next.ServeHTTP(w, r)
	})
}
