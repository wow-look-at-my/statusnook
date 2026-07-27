package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var parseCallPattern = regexp.MustCompile(
	`\b(parseTmpl|parseEmailTmpl|parseTextTmpl|writeTemplate)\((?:w, )?"([^"]+)"\)`,
)

// goSources returns the package's non-test source files.
func goSources(t *testing.T) map[string]string {
	t.Helper()

	paths, err := filepath.Glob("*.go")
	require.NoError(t, err)

	sources := map[string]string{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}

		contents, err := os.ReadFile(path)
		require.NoError(t, err)
		sources[path] = string(contents)
	}

	return sources
}

// TestEveryReferencedTemplateParses walks every template reference in the
// package and parses the file it names. A renamed or misspelled template used
// to be a 500 discovered by whoever opened that page.
func TestEveryReferencedTemplateParses(t *testing.T) {
	referenced := map[string]bool{}

	for path, source := range goSources(t) {
		for _, match := range parseCallPattern.FindAllStringSubmatch(source, -1) {
			function, file := match[1], match[2]
			referenced[file] = true

			t.Run(path+":"+file, func(t *testing.T) {
				var err error

				switch function {
				case "parseTmpl":
					_, err = parseTmpl(file)
				case "parseEmailTmpl":
					_, err = parseEmailTmpl(file)
				case "parseTextTmpl":
					_, err = parseTextTmpl(file)
				case "writeTemplate":
					_, err = templateSource(file)
				}

				assert.NoError(t, err)
			})
		}
	}

	// The dynamic monitor alert templates are named through a variable.
	for _, file := range []string{
		"monitor_down_email.html", "monitor_up_email.html",
	} {
		_, err := parseEmailTmpl(file)
		assert.NoError(t, err, file)
		referenced[file] = true
	}

	for _, file := range []string{
		"monitor_down_slack.json", "monitor_up_slack.json",
	} {
		_, err := parseTextTmpl(file)
		assert.NoError(t, err, file)
		referenced[file] = true
	}

	assert.Greater(t, len(referenced), 40, "expected the whole template set")
}

// TestNoOrphanTemplates keeps templates/ from collecting files nothing renders.
func TestNoOrphanTemplates(t *testing.T) {
	entries, err := templatesFS.ReadDir("templates")
	require.NoError(t, err)
	require.NotEmpty(t, entries)

	sources := goSources(t)

	for _, entry := range entries {
		name := entry.Name()

		found := false
		for _, source := range sources {
			if strings.Contains(source, `"`+name+`"`) {
				found = true
				break
			}
		}

		assert.True(t, found, "templates/%s is not referenced by any handler", name)
	}
}

// TestNoMarkupInGoSources keeps HTML out of .go files: templates belong in
// templates/, not in string literals inside handlers.
func TestNoMarkupInGoSources(t *testing.T) {
	// A raw-string constant holding a template document, which is what used to
	// sit inside every handler.
	markupConst := regexp.MustCompile("(?m)^\t*const (markup|rootTmpl|\\w*Markup|emailTmpl) = `")

	for path, source := range goSources(t) {
		assert.NotRegexp(
			t, markupConst, source,
			"%s declares a template literal; move it to templates/", path,
		)
	}
}
