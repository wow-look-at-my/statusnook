package main

import (
	"bytes"
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

// The htmx fragments below are the out-of-band bits of markup handlers swap
// into a page: form alerts, banners, field errors. Their markup lives in
// templates/fragment_*.html, and the message text stays in Go, escaped by the
// template rather than concatenated into it.

var fragmentTmpls = map[string]*template.Template{}

func parseFragment(file string) (*template.Template, error) {
	tmplMu.Lock()
	defer tmplMu.Unlock()

	if tmpl, ok := fragmentTmpls[file]; ok {
		return tmpl, nil
	}

	markup, err := templateSource(file)
	if err != nil {
		return nil, err
	}

	tmpl, err := template.New(file).Parse(markup)
	if err != nil {
		return tmpl, fmt.Errorf("parseFragment.Parse %s: %w", file, err)
	}

	fragmentTmpls[file] = tmpl

	return tmpl, nil
}

// renderFragment renders a fragment template. A failure here is a programming
// error, and an empty response body is better than a half-written page.
func renderFragment(file string, data any) []byte {
	tmpl, err := parseFragment(file)
	if err != nil {
		log.Printf("renderFragment.parseFragment %s: %s", file, err)
		return nil
	}

	rendered := bytes.Buffer{}
	if err := tmpl.Execute(&rendered, data); err != nil {
		log.Printf("renderFragment.Execute %s: %s", file, err)
		return nil
	}

	return rendered.Bytes()
}

// alertOOB renders the alert box above a form.
func alertOOB(message string) []byte {
	return alertOOBClass(message, "")
}

// alertOOBClass renders the alert box with an extra class, which the domain
// setup form uses for its own styling.
func alertOOBClass(message string, class string) []byte {
	return renderFragment("fragment_alert.html", struct {
		Message string
		Class   string
	}{message, class})
}

// bannerOOB renders the settings page banner.
func bannerOOB(message string) []byte {
	return renderFragment("fragment_banner.html", struct{ Message string }{message})
}

// fieldAlertOOB renders an alert attached to one form field.
func fieldAlertOOB(id string, message string) []byte {
	return renderFragment("fragment_field_alert.html", struct {
		ID      string
		Message string
	}{id, message})
}

// inlineErrorOOB renders the inline error beside a monitor form field.
func inlineErrorOOB(message string) []byte {
	return renderFragment("fragment_inline_error.html", struct{ Message string }{message})
}

// saveErrorsOOB renders the config editor's error list.
func saveErrorsOOB(messages []string) []byte {
	return renderFragment("fragment_save_errors.html", struct{ Messages []string }{messages})
}
