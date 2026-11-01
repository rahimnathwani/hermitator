package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// Every control that deletes, disconnects or discards something asks
// first, saying what it will do. This keeps a new one from slipping in
// without the dialog.
func TestDestructiveFormsAskFirst(t *testing.T) {
	destructive := regexp.MustCompile(`/(delete|disconnect|new-session|upgrade-image|upgrade-all|use-image-harnesses|suspend|password)"`)
	forms := regexp.MustCompile(`(?s)<form\b[^>]*>.*?</form>`)
	fs.WalkDir(assets, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		src, _ := fs.ReadFile(assets, path)
		for _, form := range forms.FindAllString(string(src), -1) {
			open := form[:strings.Index(form, ">")+1]
			removes := strings.Contains(form, `name="remove"`)
			if !destructive.MatchString(open) && !removes {
				continue
			}
			// Reinstating a suspended user and changing your own password
			// take nothing away.
			if strings.Contains(form, "Reinstate") || strings.Contains(open, `action="/account/password"`) {
				continue
			}
			if !strings.Contains(open, "data-confirm=") || !strings.Contains(form, "<template data-confirm-body>") {
				t.Errorf("%s: this form destroys something without asking first:\n%s", path, open)
			}
		}
		return nil
	})
}

// The body boosts every form with htmx, which submits a form even when
// the page's own script has called preventDefault. A form with no method
// is one that script handles, so htmx would also send it as a GET and
// swap the page out from under it (and put its fields in the address
// bar). Such forms must opt out.
func TestScriptFormsAreNotBoosted(t *testing.T) {
	open := regexp.MustCompile(`<form\b[^>]*>`)
	fs.WalkDir(assets, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		src, _ := fs.ReadFile(assets, path)
		for _, tag := range open.FindAllString(string(src), -1) {
			if !strings.Contains(tag, "method=") && !strings.Contains(tag, `hx-boost="false"`) {
				t.Errorf("%s: %s has no method and is boosted; add hx-boost=\"false\"", path, tag)
			}
		}
		return nil
	})
}
