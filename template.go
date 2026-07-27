package main

import (
	"embed"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"path"
	"sync"
	textTemplate "text/template"
)

// Templates live in templates/ as .html (pages and emails) and .json (Slack
// payloads), rather than as string literals inside handlers. Each parse
// function takes a file name from that directory.
//
//go:embed templates
var templatesFS embed.FS

// templateSource reads one template file.
func templateSource(file string) (string, error) {
	contents, err := templatesFS.ReadFile(path.Join("templates", file))
	if err != nil {
		return "", fmt.Errorf("templateSource: %w", err)
	}

	return string(contents), nil
}

// Parsed templates are cached: handlers are hot paths and the source never
// changes at runtime. The mutex matters because handlers run concurrently.
var (
	tmplMu     sync.Mutex
	tmpls      = map[string]*template.Template{}
	emailTmpls = map[string]*template.Template{}
	textTmpls  = map[string]*textTemplate.Template{}
)

// parseTmpl returns the page template in templates/<file>, wrapped in the
// shared root layout.
func parseTmpl(file string) (*template.Template, error) {
	tmplMu.Lock()
	defer tmplMu.Unlock()

	if tmpl, ok := tmpls[file]; ok {
		return tmpl, nil
	}

	root, err := templateSource("root.html")
	if err != nil {
		return nil, err
	}

	markup, err := templateSource(file)
	if err != nil {
		return nil, err
	}

	tmpl, err := template.New(file).Parse(root)
	if err != nil {
		return tmpl, fmt.Errorf("parseTmpl.ParseRoot %s: %w", file, err)
	}

	tmpl, err = tmpl.Parse(markup)
	if err != nil {
		return tmpl, fmt.Errorf("parseTmpl.Parse %s: %w", file, err)
	}

	tmpls[file] = tmpl

	return tmpl, nil
}

// parseEmailTmpl returns an HTML email body template.
func parseEmailTmpl(file string) (*template.Template, error) {
	tmplMu.Lock()
	defer tmplMu.Unlock()

	if tmpl, ok := emailTmpls[file]; ok {
		return tmpl, nil
	}

	markup, err := templateSource(file)
	if err != nil {
		return nil, err
	}

	tmpl, err := template.New(file).Parse(markup)
	if err != nil {
		return tmpl, fmt.Errorf("parseEmailTmpl.Parse %s: %w", file, err)
	}

	emailTmpls[file] = tmpl

	return tmpl, nil
}

// parseTextTmpl returns a plain-text template, used for Slack payloads, where
// HTML escaping would corrupt the message.
func parseTextTmpl(file string) (*textTemplate.Template, error) {
	tmplMu.Lock()
	defer tmplMu.Unlock()

	if tmpl, ok := textTmpls[file]; ok {
		return tmpl, nil
	}

	markup, err := templateSource(file)
	if err != nil {
		return nil, err
	}

	tmpl, err := textTemplate.New(file).Parse(markup)
	if err != nil {
		return tmpl, fmt.Errorf("parseTextTmpl.Parse %s: %w", file, err)
	}

	textTmpls[file] = tmpl

	return tmpl, nil
}

// writeTemplate writes a static markup file to the response. Some responses are
// fixed htmx fragments with nothing to render.
func writeTemplate(w http.ResponseWriter, file string) {
	markup, err := templateSource(file)
	if err != nil {
		log.Printf("writeTemplate %s: %s", file, err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.Write([]byte(markup))
}
