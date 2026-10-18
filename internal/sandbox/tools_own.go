package sandbox

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
)

// Harness is a coding agent the image ships, and the files an agent
// replaces when it installs its own copy over it.
type Harness struct {
	Name    string
	Package string // under usr/lib/node_modules
	Bin     string // under usr/bin
}

// Harnesses are the ones the universal image installs with npm.
var Harnesses = []Harness{
	{"Claude Code", "@anthropic-ai/claude-code", "claude"},
	{"pi", "@earendil-works/pi-coding-agent", "pi"},
}

// OwnCopy is a harness an agent installed itself, with both versions.
type OwnCopy struct {
	Name  string
	Own   string // the agent's version; empty if it removed the harness
	Image string // the image's version
}

const nodeModules = "usr/lib/node_modules"

// OwnCopies lists the harnesses the agent has its own copy of.
//
// Anything the agent writes lands in its upper layer and hides the
// image's file for good, so after `npm install -g @anthropic-ai/claude-code`
// later image upgrades never reach that agent's Claude Code. The upper
// layer is the agent's to write, symlinks included, so it is read
// through an os.Root.
func (s *Spec) OwnCopies() []OwnCopy {
	upper, err := os.OpenRoot(s.upper())
	if err != nil {
		return nil
	}
	defer upper.Close()
	var out []OwnCopy
	for _, h := range Harnesses {
		pkg := path.Join(nodeModules, h.Package)
		_, errPkg := upper.Lstat(pkg)
		_, errBin := upper.Lstat(path.Join("usr/bin", h.Bin))
		if errPkg != nil && errBin != nil {
			continue
		}
		c := OwnCopy{Name: h.Name, Own: packageVersion(upper, pkg)}
		if image, err := os.OpenRoot(s.Image.Rootfs); err == nil {
			c.Image = packageVersion(image, pkg)
			image.Close()
		}
		out = append(out, c)
	}
	return out
}

func packageVersion(root *os.Root, pkg string) string {
	b, err := root.ReadFile(path.Join(pkg, "package.json"))
	if err != nil {
		return ""
	}
	var p struct{ Version string }
	json.Unmarshal(b, &p)
	return p.Version
}

// DropOwnCopies removes the agent's own harness copies, so the image's
// show through again. The sandbox must be stopped: changing an upper
// layer under a mounted overlay is undefined.
func (s *Spec) DropOwnCopies() error {
	upper, err := os.OpenRoot(s.upper())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	defer upper.Close()
	for _, h := range Harnesses {
		pkg := path.Join(nodeModules, h.Package)
		for _, p := range []string{pkg, path.Join("usr/bin", h.Bin)} {
			if err := upper.RemoveAll(p); err != nil {
				return err
			}
		}
		// An emptied scope directory may be opaque (the agent deleted and
		// recreated it), which would go on hiding the image's; remove it
		// too unless the agent keeps other packages in it.
		if scope := path.Dir(pkg); scope != nodeModules {
			upper.Remove(scope) // fails, harmlessly, if not empty
		}
	}
	return nil
}
