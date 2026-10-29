package main

// THE VOCABULARY: the class names a template may use, and the only ones. A page that reads as
// one thing is built from a short list of parts; every class outside the list is a part nobody
// named, and an inline style is a part nobody can find. Two lists, held here in the open so
// adding to either is a visible act: Basecoat's components the templates use, each checked to
// exist in the built stylesheet, and THE PARTS of Briard's own, each defined in css/input.css --
// checked both ways, so neither a template's unknown class nor a part nothing uses survives.

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// components is what the templates take from Basecoat.
var components = map[string]bool{
	"sidebar": true, "btn": true, "card": true, "alert": true, "badge": true,
	"field": true, "label": true, "input": true, "table": true, "table-container": true,
	"item": true, "item-group": true,
}

// parts is Briard's own: the shell, the page, and the few things inside a card that Basecoat
// has no name for.
var parts = map[string]bool{
	"topbar": true, "page": true, "page-head": true, "tiles": true, "wide": true, "group-title": true,
	"note": true, "ok": true, "when": true, "key": true, "code": true,
	"history": true, "what": true, "bad": true, "undo": true,
}

var (
	classAttr = regexp.MustCompile(`class="([^"]*)"`)
	action    = regexp.MustCompile(`\{\{[^}]*\}\}`)
	partRule  = regexp.MustCompile(`(?m)^\.[^{]*`)
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
				t.Errorf("%s:%d: an inline style; name the part in css/input.css instead", e.Name(), i+1)
			}
			for _, m := range classAttr.FindAllStringSubmatch(line, -1) {
				for _, c := range strings.Fields(action.ReplaceAllString(m[1], " ")) {
					used[c] = true
					if !components[c] && !parts[c] {
						t.Errorf("%s:%d: class %q is neither a Basecoat component nor a part", e.Name(), i+1, c)
					}
				}
			}
		}
	}
	css := string(appCSS)
	for c := range components {
		if !strings.Contains(css, "."+c+"{") && !strings.Contains(css, "."+c+"[") && !strings.Contains(css, "."+c+">") && !strings.Contains(css, "."+c+" ") && !strings.Contains(css, "."+c+",") {
			t.Errorf("the stylesheet has no %q; the components list names something Basecoat does not ship", c)
		}
		if !used[c] {
			t.Errorf("the components list names %q, which no template uses", c)
		}
	}
	src, err := fs.ReadFile(pageFS, "css/input.css")
	if err != nil {
		t.Fatal(err)
	}
	defined := map[string]bool{}
	for _, sel := range partRule.FindAllString(string(src), -1) {
		for _, m := range cssClass.FindAllStringSubmatch(sel, -1) {
			defined[m[1]] = true
			if !parts[m[1]] && !components[m[1]] {
				t.Errorf("css/input.css styles %q, which is neither a Basecoat component nor a part", m[1])
			}
		}
	}
	for c := range parts {
		if !defined[c] {
			t.Errorf("the parts list names %q, which css/input.css does not define", c)
		}
		if !used[c] {
			t.Errorf("the parts list names %q, which no template uses", c)
		}
	}
}
