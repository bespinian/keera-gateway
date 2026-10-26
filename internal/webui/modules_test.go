package webui

import (
	"io/fs"
	"path"
	"regexp"
	"strings"
	"testing"
)

var (
	// namedImport is `import { a, b as c } from "./x.js"`, over several lines.
	namedImport = regexp.MustCompile(`import\s*\{([^}]*)\}\s*from\s*"(\.[^"]+)"`)
	// declaredExport is `export function a`, `export const a` and the like.
	declaredExport = regexp.MustCompile(
		`(?m)^export\s+(?:async\s+)?(?:function\*?|const|let|class)\s+([A-Za-z_$][\w$]*)`)
	// listedExport is `export { a, b as c }`.
	listedExport = regexp.MustCompile(`(?m)^export\s*\{([^}]*)\}`)
	// otherExport is a form the two above cannot read. This test must learn
	// it before the panel uses one.
	otherExport = regexp.MustCompile(`(?m)^export\s+(?:default|\*)`)
)

// Importing a name a module does not export is a SyntaxError in the browser:
// the whole panel is blank, and with no build step nothing catches it earlier.
func TestEveryImportedNameIsExported(t *testing.T) {
	modules := map[string]string{}
	if err := fs.WalkDir(assets, "assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".js") {
			return err
		}
		body, err := fs.ReadFile(assets, p)
		modules[p] = string(body)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	exports := map[string]map[string]bool{}
	for p, body := range modules {
		if otherExport.MatchString(body) {
			t.Errorf("%s has an export this test cannot read; teach it the form", p)
		}
		names := map[string]bool{}
		for _, m := range declaredExport.FindAllStringSubmatch(body, -1) {
			names[m[1]] = true
		}
		for _, m := range listedExport.FindAllStringSubmatch(body, -1) {
			for _, entry := range listNames(m[1]) {
				// What it is exported as: the name after "as", if any.
				fields := strings.Fields(entry)
				names[fields[len(fields)-1]] = true
			}
		}
		exports[p] = names
	}

	checked := 0
	for p, body := range modules {
		for _, m := range namedImport.FindAllStringSubmatch(body, -1) {
			target := path.Join(path.Dir(p), m[2])
			names, ok := exports[target]
			if !ok {
				// TestEveryImportIsEmbedded reports it.
				continue
			}
			for _, entry := range listNames(m[1]) {
				// What it is exported as: the name before "as", if any.
				name := strings.Fields(entry)[0]
				if !names[name] {
					t.Errorf("%s imports %s from %s, which does not export it", p, name, m[2])
				}
				checked++
			}
		}
	}
	if checked < 100 {
		t.Errorf("only %d imported names were found; the patterns no longer match the panel", checked)
	}
}

// listNames splits the inside of braces into its entries, dropping comments
// and the empty entry a trailing comma leaves.
func listNames(inside string) []string {
	var out []string
	for line := range strings.SplitSeq(inside, "\n") {
		line, _, _ = strings.Cut(line, "//")
		for entry := range strings.SplitSeq(line, ",") {
			if entry = strings.TrimSpace(entry); entry != "" {
				out = append(out, entry)
			}
		}
	}
	return out
}

// styleAttribute is a style given as text, which h() and svg() would set as
// an attribute. The CSP refuses those, so the browser drops the style.
var styleAttribute = regexp.MustCompile("(?:style:\\s*[`\"']|setAttribute\\(\\s*\"style\")")

func TestNoModuleSetsAStyleAttribute(t *testing.T) {
	if err := fs.WalkDir(assets, "assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".js") {
			return err
		}
		body, err := fs.ReadFile(assets, p)
		for _, m := range styleAttribute.FindAllString(string(body), -1) {
			t.Errorf("%s sets a style attribute (%s); give h() an object, or set el.style", p, m)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
