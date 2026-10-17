package sandbox

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Images finds base images exported by deploy/build-image.sh:
//
//	<Dir>/universal                -> universal-20261017   (symlink)
//	<Dir>/universal-20261017/rootfs
//	<Dir>/universal-20261017/ENV
type Images struct {
	Dir string
}

// Current resolves a family name such as "universal" to its newest dated
// version, which is what a new agent should pin.
func (im *Images) Current(family string) (string, error) {
	target, err := os.Readlink(filepath.Join(im.Dir, family))
	if err != nil {
		return "", fmt.Errorf("no image %q in %s (build one with deploy/build-image.sh)", family, im.Dir)
	}
	return filepath.Base(target), nil
}

// Families lists the image families that have a current version.
func (im *Images) Families() []string {
	entries, _ := os.ReadDir(im.Dir)
	var out []string
	for _, e := range entries {
		if e.Type()&os.ModeSymlink != 0 {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// Resolve returns a pinned image version.
func (im *Images) Resolve(name string) (Image, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		return Image{}, fmt.Errorf("invalid image name %q", name)
	}
	dir := filepath.Join(im.Dir, name)
	rootfs := filepath.Join(dir, "rootfs")
	if fi, err := os.Stat(rootfs); err != nil || !fi.IsDir() {
		return Image{}, fmt.Errorf("image %q not found in %s", name, im.Dir)
	}
	img := Image{Name: name, Rootfs: rootfs}
	if f, err := os.Open(filepath.Join(dir, "ENV")); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if line := strings.TrimSpace(sc.Text()); strings.Contains(line, "=") {
				img.Env = append(img.Env, line)
			}
		}
		f.Close()
	}
	return img, nil
}
