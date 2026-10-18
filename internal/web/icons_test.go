package web

import (
	"io/fs"
	"regexp"
	"testing"
)

// An icon name the map lacks renders as an empty box, silently.
func TestTemplatesUseKnownIcons(t *testing.T) {
	uses := regexp.MustCompile(`icon "([a-z-]+)"`)
	fs.WalkDir(assets, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		src, _ := fs.ReadFile(assets, path)
		for _, m := range uses.FindAllSubmatch(src, -1) {
			if _, ok := icons[string(m[1])]; !ok {
				t.Errorf("%s uses unknown icon %q", path, m[1])
			}
		}
		return nil
	})
}
