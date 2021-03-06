package fs

import (
	"context"
	"io"
	"os"
	"sort"
	"time"
)

// Entry represents a filesystem entry, which can be Directory, File, or Symlink
type Entry interface {
	os.FileInfo
	Owner() OwnerInfo
}

// OwnerInfo describes owner of a filesystem entry
type OwnerInfo struct {
	UserID  uint32
	GroupID uint32
}

// Entries is a list of entries sorted by name.
type Entries []Entry

// Reader allows reading from a file and retrieving its up-to-date file info.
type Reader interface {
	io.ReadCloser
	io.Seeker

	Entry() (Entry, error)
}

// File represents an entry that is a file.
type File interface {
	Entry
	Open(ctx context.Context) (Reader, error)
}

// Directory represents contents of a directory.
type Directory interface {
	Entry
	Readdir(ctx context.Context) (Entries, error)
	Summary() *DirectorySummary
}

// DirectorySummary represents summary information about a directory.
type DirectorySummary struct {
	TotalFileSize    int64     `json:"size"`
	TotalFileCount   int64     `json:"files"`
	TotalDirCount    int64     `json:"dirs"`
	MaxModTime       time.Time `json:"maxTime"`
	IncompleteReason string    `json:"incomplete,omitempty"`
}

// Symlink represents a symbolic link entry.
type Symlink interface {
	Entry
	Readlink(ctx context.Context) (string, error)
}

// FindByName returns an entry with a given name, or nil if not found.
func (e Entries) FindByName(n string) Entry {
	i := sort.Search(
		len(e),
		func(i int) bool {
			return e[i].Name() >= n
		},
	)
	if i < len(e) && e[i].Name() == n {
		return e[i]
	}

	return nil
}

// Sort sorts the entries by name.
func (e Entries) Sort() {
	sort.Slice(e, func(i, j int) bool {
		return e[i].Name() < e[j].Name()
	})
}
