package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiskUsageCountsWhatIsWritten(t *testing.T) {
	s := &Spec{Dir: t.TempDir()}
	empty, err := s.DiskUsage()
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(s.Dir, "upper", "root"), 0o755)
	if err := os.WriteFile(filepath.Join(s.Dir, "upper", "root", "big"), make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	// A symlink out of the directory must not be followed.
	os.Symlink("/", filepath.Join(s.Dir, "upper", "escape"))
	got, err := s.DiskUsage()
	if err != nil {
		t.Fatal(err)
	}
	if grew := got - empty; grew < 1<<20 || grew > 2<<20 {
		t.Fatalf("a 1 MiB file added %d bytes", grew)
	}
}

func TestDiskUsageOfAMissingDirectory(t *testing.T) {
	if _, err := (&Spec{Dir: filepath.Join(t.TempDir(), "gone")}).DiskUsage(); err == nil {
		t.Fatal("no error for a directory that does not exist")
	}
}
