package main

// THE VOCABULARY: the class names a template may use, and the only ones. A page that reads as
// one thing is built from a short list of parts; every class outside the list is a part nobody
// named, and an inline style is a part nobody can find. The list is held here, in the open, so
// adding to it is a visible act -- and it is checked both ways: no template uses a class the
// list lacks, and the stylesheet styles nothing the list lacks, so dead CSS cannot accumulate.

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// vocabulary is every class the templates and the stylesheet may name. The severity classes
// (warning, critical, resolved) are set from an alert's own severity at render.
var vocabulary = map[string]bool{
	"nav": true, "card": true, "status": true, "muted": true, "banner": true, "quiet": true, "action": true,
	"field": true, "wide": true, "rows": true, "when": true, "warning": true, "critical": true, "resolved": true, "ok": true,
	"key": true, "code": true,
	"history": true, "what": true, "bad": true, "undo": true,
}

var (
	classAttr = regexp.MustCompile(`class="([^"]*)"`)
	action    = regexp.MustCompile(`\{\{[^}]*\}\}`)
	cssClass  = regexp.MustCompile(`\.([a-z][a-z-]*)`)
)

func TestTemplatesUseOnlyTheVocabulary(t *testing.T) {
	entries, err := fs.ReadDir(pageFS, "templates")
	if err != nil {
		t.Fatal(err)
	}
	used := map[string]bool{}
	for _, e := range entries {
		src, err := fs.ReadFile(pageFS, "templates/"+e.Name())
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if strings.Contains(line, "style=") {
				t.Errorf("%s:%d: an inline style; name the part in the vocabulary instead", e.Name(), i+1)
			}
			for _, m := range classAttr.FindAllStringSubmatch(line, -1) {
				for _, c := range strings.Fields(action.ReplaceAllString(m[1], " ")) {
					used[c] = true
					if !vocabulary[c] {
						t.Errorf("%s:%d: class %q is not in the vocabulary", e.Name(), i+1, c)
					}
				}
			}
		}
		if e.Name() == "shell.html" {
			css := string(src)[strings.Index(string(src), "<style>"):strings.Index(string(src), "</style>")]
			for _, m := range cssClass.FindAllStringSubmatch(css, -1) {
				used[m[1]] = true
				if !vocabulary[m[1]] {
					t.Errorf("the stylesheet styles %q, which is not in the vocabulary", m[1])
				}
			}
		}
	}
	for c := range vocabulary {
		if !used[c] {
			t.Errorf("the vocabulary lists %q, which nothing uses or styles", c)
		}
	}
}
