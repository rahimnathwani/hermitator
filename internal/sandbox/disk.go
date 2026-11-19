package sandbox

import (
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// DiskUsage is how much disk the sandbox's own files take: its upper
// layer, checkpoint, shared directory and logs. It counts allocated
// blocks, so a sparse checkpoint counts for what it really holds, and
// stays on the directory's filesystem, so the mounted root, which is
// mostly the shared image, is left out.
func (s *Spec) DiskUsage() (int64, error) {
	return diskUsage(s.Dir)
}

func diskUsage(dir string) (int64, error) {
	root, err := os.Lstat(dir)
	if err != nil {
		return 0, err
	}
	dev, ok := device(root)
	var total int64
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Files come and go while the agent runs.
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if d.IsDir() && path != dir && ok {
			if dv, ok := device(fi); ok && dv != dev {
				return filepath.SkipDir
			}
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			total += int64(st.Blocks) * 512
		} else if fi.Mode().IsRegular() {
			total += fi.Size()
		}
		return nil
	})
	return total, err
}

func device(fi os.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Dev), true
}
