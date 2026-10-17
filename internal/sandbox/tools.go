package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// InstallTools copies the guest binary and support files into a
// content-addressed directory under root and returns its path. Sandboxes
// mount it read-only at /opt/hermitator. Each sandbox keeps the bundle it
// booted with until its next cold boot, because a checkpoint must be
// restored against exactly the files it was taken with.
func InstallTools(root, guestBinary string, files fs.FS) (string, error) {
	h := sha256.New()
	bin, err := os.ReadFile(guestBinary)
	if err != nil {
		return "", err
	}
	h.Write(bin)
	var names []string
	fs.WalkDir(files, ".", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, path)
		}
		return nil
	})
	sort.Strings(names)
	for _, n := range names {
		b, err := fs.ReadFile(files, n)
		if err != nil {
			return "", err
		}
		h.Write([]byte(n))
		h.Write(b)
	}
	dir := filepath.Join(root, hex.EncodeToString(h.Sum(nil))[:16])
	if _, err := os.Stat(dir); err == nil {
		return dir, nil
	}

	tmp := dir + ".partial"
	os.RemoveAll(tmp)
	if err := os.MkdirAll(filepath.Join(tmp, "bin"), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmp, "bin", "hermitator-guest"), bin, 0o755); err != nil {
		return "", err
	}
	if err := os.Symlink("hermitator-guest", filepath.Join(tmp, "bin", "whatsapp-send")); err != nil {
		return "", err
	}
	for _, n := range names {
		dst := filepath.Join(tmp, "lib", n)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", err
		}
		src, err := files.Open(n)
		if err != nil {
			return "", err
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if err != nil {
			src.Close()
			return "", err
		}
		_, err = io.Copy(out, src)
		src.Close()
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return "", err
		}
	}
	if err := os.Rename(tmp, dir); err != nil {
		return "", err
	}
	return dir, nil
}

// PruneTools deletes bundles under root that neither current nor any of
// the given sandbox directories still refer to.
func PruneTools(root, current string, sandboxDirs []string) {
	keep := map[string]bool{current: true}
	for _, d := range sandboxDirs {
		if t := bundleTools(d); t != "" {
			keep[t] = true
		}
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		if !keep[p] {
			os.RemoveAll(p)
		}
	}
}
