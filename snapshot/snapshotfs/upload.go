package snapshotfs

import (
	"bytes"
	"context"
	"encoding/json"
	"hash/fnv"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/errors"

	"github.com/kopia/kopia/fs"
	"github.com/kopia/kopia/fs/ignorefs"
	"github.com/kopia/kopia/internal/kopialogging"
	"github.com/kopia/kopia/repo"
	"github.com/kopia/kopia/repo/object"
	"github.com/kopia/kopia/snapshot"
)

var log = kopialogging.Logger("kopia/upload")

var errCancelled = errors.New("canceled")

// Uploader supports efficient uploading files and directories to repository.
type Uploader struct {
	Progress UploadProgress

	FilesPolicy ignorefs.FilesPolicyGetter

	// automatically cancel the Upload after certain number of bytes
	MaxUploadBytes int64

	// ignore file read errors
	IgnoreFileErrors bool

	// probability with cached entries will be ignored, must be [0..100]
	// 0=always use cached object entries if possible
	// 100=never use cached entries
	ForceHashPercentage int

	// Number of files to hash and upload in parallel.
	ParallelUploads int

	repo *repo.Repository

	stats    snapshot.Stats
	canceled int32

	progressMutex          sync.Mutex
	nextProgressReportTime time.Time
	currentProgressDir     string // current directory for reporting progress
	currentDirNumFiles     int    // number of files in current directory
	currentDirCompleted    int64  // bytes completed in current directory
	currentDirTotalSize    int64  // total # of bytes in current directory
}

// IsCancelled returns true if the upload is canceled.
func (u *Uploader) IsCancelled() bool {
	return u.cancelReason() != ""
}

func (u *Uploader) cancelReason() string {
	if c := atomic.LoadInt32(&u.canceled) != 0; c {
		return "canceled"
	}

	if mub := u.MaxUploadBytes; mub > 0 && u.repo.Content.Stats().WrittenBytes > mub {
		return "limit reached"
	}

	return ""
}

func (u *Uploader) uploadFileInternal(ctx context.Context, f fs.File) entryResult {
	file, err := f.Open(ctx)
	if err != nil {
		return entryResult{err: errors.Wrap(err, "unable to open file")}
	}
	defer file.Close() //nolint:errcheck

	writer := u.repo.Objects.NewWriter(ctx, object.WriterOptions{
		Description: "FILE:" + f.Name(),
	})
	defer writer.Close() //nolint:errcheck

	written, err := u.copyWithProgress(writer, file, 0, f.Size())
	if err != nil {
		return entryResult{err: err}
	}

	fi2, err := file.Entry()
	if err != nil {
		return entryResult{err: err}
	}

	r, err := writer.Result()
	if err != nil {
		return entryResult{err: err}
	}

	de, err := newDirEntry(fi2, r)
	if err != nil {
		return entryResult{err: errors.Wrap(err, "unable to create dir entry")}
	}
	de.FileSize = written

	return entryResult{de: de}
}

func (u *Uploader) uploadSymlinkInternal(ctx context.Context, f fs.Symlink) entryResult {
	target, err := f.Readlink(ctx)
	if err != nil {
		return entryResult{err: errors.Wrap(err, "unable to read symlink")}
	}

	writer := u.repo.Objects.NewWriter(ctx, object.WriterOptions{
		Description: "SYMLINK:" + f.Name(),
	})
	defer writer.Close() //nolint:errcheck

	written, err := u.copyWithProgress(writer, bytes.NewBufferString(target), 0, f.Size())
	if err != nil {
		return entryResult{err: err}
	}

	r, err := writer.Result()
	if err != nil {
		return entryResult{err: err}
	}

	de, err := newDirEntry(f, r)
	if err != nil {
		return entryResult{err: errors.Wrap(err, "unable to create dir entry")}
	}
	de.FileSize = written
	return entryResult{de: de}
}

func (u *Uploader) addDirProgress(length int64) {
	u.progressMutex.Lock()
	u.currentDirCompleted += length
	c := u.currentDirCompleted
	shouldReport := false
	if time.Now().After(u.nextProgressReportTime) {
		shouldReport = true
		u.nextProgressReportTime = time.Now().Add(100 * time.Millisecond)
	}
	if c == u.currentDirTotalSize {
		shouldReport = true
	}
	u.progressMutex.Unlock()

	if shouldReport {
		u.Progress.Progress(u.currentProgressDir, u.currentDirNumFiles, c, u.currentDirTotalSize, &u.stats)
	}
}

func (u *Uploader) copyWithProgress(dst io.Writer, src io.Reader, completed, length int64) (int64, error) {
	uploadBuf := make([]byte, 128*1024) // 128 KB buffer

	var written int64

	for {
		if u.IsCancelled() {
			return 0, errCancelled
		}

		readBytes, readErr := src.Read(uploadBuf)
		if readBytes > 0 {
			wroteBytes, writeErr := dst.Write(uploadBuf[0:readBytes])
			if wroteBytes > 0 {
				written += int64(wroteBytes)
				completed += int64(wroteBytes)
				u.addDirProgress(int64(wroteBytes))
				if length < completed {
					length = completed
				}
			}
			if writeErr != nil {
				return written, writeErr
			}
			if readBytes != wroteBytes {
				return written, io.ErrShortWrite
			}
		}

		if readErr != nil {
			if readErr == io.EOF {
				break
			}

			return written, readErr
		}
	}

	return written, nil
}

func newDirEntry(md fs.Entry, oid object.ID) (*snapshot.DirEntry, error) {
	var entryType snapshot.EntryType

	switch md := md.(type) {
	case fs.Directory:
		entryType = snapshot.EntryTypeDirectory
	case fs.Symlink:
		entryType = snapshot.EntryTypeSymlink
	case fs.File:
		entryType = snapshot.EntryTypeFile
	default:
		return nil, errors.Errorf("invalid entry type %T", md)
	}

	return &snapshot.DirEntry{
		Name:        md.Name(),
		Type:        entryType,
		Permissions: snapshot.Permissions(md.Mode() & os.ModePerm),
		FileSize:    md.Size(),
		ModTime:     md.ModTime(),
		UserID:      md.Owner().UserID,
		GroupID:     md.Owner().GroupID,
		ObjectID:    oid,
	}, nil
}

// uploadFile uploads the specified File to the repository.
func (u *Uploader) uploadFile(ctx context.Context, file fs.File) (*snapshot.DirEntry, error) {
	res := u.uploadFileInternal(ctx, file)
	if res.err != nil {
		return nil, res.err
	}

	de, err := newDirEntry(file, res.de.ObjectID)
	if err != nil {
		return nil, errors.Wrap(err, "unable to create dir entry")
	}
	de.DirSummary = &fs.DirectorySummary{
		TotalFileCount: 1,
		TotalFileSize:  res.de.FileSize,
		MaxModTime:     res.de.ModTime,
	}

	return de, nil
}

// uploadDir uploads the specified Directory to the repository.
// An optional ID of a hash-cache object may be provided, in which case the Uploader will use its
// contents to avoid hashing
func (u *Uploader) uploadDir(ctx context.Context, rootDir fs.Directory, previousDirs []fs.Directory) (*snapshot.DirEntry, error) {
	oid, summ, err := uploadDirInternal(ctx, u, rootDir, previousDirs, ".")
	if err != nil {
		return nil, err
	}

	de, err := newDirEntry(rootDir, oid)
	if err != nil {
		return nil, errors.Wrap(err, "unable to create dir entry")
	}

	de.DirSummary = &summ
	return de, err
}

func (u *Uploader) foreachEntryUnlessCancelled(relativePath string, entries fs.Entries, cb func(entry fs.Entry, entryRelativePath string) error) error {
	for _, entry := range entries {
		if u.IsCancelled() {
			return errCancelled
		}

		entryRelativePath := relativePath + "/" + entry.Name()

		if err := cb(entry, entryRelativePath); err != nil {
			return err
		}
	}

	return nil
}

type entryResult struct {
	err error
	de  *snapshot.DirEntry
}

func (u *Uploader) processSubdirectories(ctx context.Context, relativePath string, entries fs.Entries, previousEntries []fs.Entries, dirManifest *snapshot.DirManifest, summ *fs.DirectorySummary) error {
	return u.foreachEntryUnlessCancelled(relativePath, entries, func(entry fs.Entry, entryRelativePath string) error {
		dir, ok := entry.(fs.Directory)
		if !ok {
			// skip non-directories
			return nil
		}

		var previousDirs []fs.Directory
		for _, e := range previousEntries {
			if d, _ := e.FindByName(entry.Name()).(fs.Directory); d != nil {
				previousDirs = append(previousDirs, d)
			}
		}

		previousDirs = uniqueDirectories(previousDirs)

		oid, subdirsumm, err := uploadDirInternal(ctx, u, dir, previousDirs, entryRelativePath)
		if err == errCancelled {
			return err
		}

		summ.TotalFileCount += subdirsumm.TotalFileCount
		summ.TotalFileSize += subdirsumm.TotalFileSize
		summ.TotalDirCount += subdirsumm.TotalDirCount
		if subdirsumm.MaxModTime.After(summ.MaxModTime) {
			summ.MaxModTime = subdirsumm.MaxModTime
		}

		if err != nil {
			return errors.Errorf("unable to process directory %q: %s", entry.Name(), err)
		}

		de, err := newDirEntry(dir, oid)
		if err != nil {
			return errors.Wrap(err, "unable to create dir entry")
		}

		de.DirSummary = &subdirsumm
		dirManifest.Entries = append(dirManifest.Entries, de)
		return nil
	})
}

func (u *Uploader) prepareProgress(relativePath string, entries fs.Entries) {
	u.currentProgressDir = relativePath
	u.currentDirTotalSize = 0
	u.currentDirCompleted = 0
	u.currentDirNumFiles = 0

	// Phase #2 - compute the total size of files in current directory
	_ = u.foreachEntryUnlessCancelled(relativePath, entries, func(entry fs.Entry, entryRelativePath string) error {
		if _, ok := entry.(fs.File); !ok {
			// skip directories
			return nil
		}

		u.currentDirNumFiles++
		u.currentDirTotalSize += entry.Size()
		return nil
	})
}

type uploadWorkItem struct {
	entry             fs.Entry
	entryRelativePath string
	uploadFunc        func() entryResult
	resultChan        chan entryResult
}

func metadataEquals(e1, e2 fs.Entry) bool {
	if l, r := e1.ModTime(), e2.ModTime(); !l.Equal(r) {
		return false
	}
	if l, r := e1.Mode(), e2.Mode(); l != r {
		return false
	}
	if l, r := e1.Size(), e2.Size(); l != r {
		return false
	}
	if l, r := e1.Owner(), e2.Owner(); l != r {
		return false
	}
	return true
}

func findCachedEntry(entry fs.Entry, prevEntries []fs.Entries) fs.Entry {
	for _, e := range prevEntries {
		if ent := e.FindByName(entry.Name()); ent != nil {
			if metadataEquals(entry, ent) {
				return ent
			}

			log.Debugf("found non-matching entry for %v: %v %v %v", entry.Name(), ent.Mode(), ent.Size(), ent.ModTime())
		}
	}
	log.Debugf("could not find cache entry for %v", entry.Name())
	return nil
}

// objectIDPercent arbitrarily maps given object ID onto a number 0.99
func objectIDPercent(obj object.ID) int {
	h := fnv.New32a()
	io.WriteString(h, obj.String()) //nolint:errcheck
	return int(h.Sum32() % 100)
}

func (u *Uploader) maybeIgnoreCachedEntry(ent fs.Entry) fs.Entry {
	if h, ok := ent.(object.HasObjectID); ok {
		if objectIDPercent(h.ObjectID()) < u.ForceHashPercentage {
			log.Debugf("ignoring valid cached object: %v", h.ObjectID())
			return nil
		}
		return ent
	}

	return nil
}

func (u *Uploader) prepareWorkItems(ctx context.Context, dirRelativePath string, entries fs.Entries, prevEntries []fs.Entries, summ *fs.DirectorySummary) ([]*uploadWorkItem, error) {
	var result []*uploadWorkItem

	resultErr := u.foreachEntryUnlessCancelled(dirRelativePath, entries, func(entry fs.Entry, entryRelativePath string) error {
		if _, ok := entry.(fs.Directory); ok {
			// skip directories
			return nil
		}

		// regular file
		if entry, ok := entry.(fs.File); ok {
			u.stats.TotalFileCount++
			u.stats.TotalFileSize += entry.Size()
			summ.TotalFileCount++
			summ.TotalFileSize += entry.Size()
			if entry.ModTime().After(summ.MaxModTime) {
				summ.MaxModTime = entry.ModTime()
			}
		}

		// See if we had this name during either of previous passes.
		if cachedEntry := u.maybeIgnoreCachedEntry(findCachedEntry(entry, prevEntries)); cachedEntry != nil {
			u.stats.CachedFiles++
			u.addDirProgress(entry.Size())

			// compute entryResult now, cachedEntry is short-lived
			cachedDirEntry, err := newDirEntry(entry, cachedEntry.(object.HasObjectID).ObjectID())
			if err != nil {
				return errors.Wrap(err, "unable to create dir entry")
			}

			// Avoid hashing by reusing previous object ID.
			result = append(result, &uploadWorkItem{
				entry:             entry,
				entryRelativePath: entryRelativePath,
				uploadFunc: func() entryResult {
					return entryResult{de: cachedDirEntry}
				},
			})
		} else {
			switch entry := entry.(type) {
			case fs.Symlink:
				result = append(result, &uploadWorkItem{
					entry:             entry,
					entryRelativePath: entryRelativePath,
					uploadFunc: func() entryResult {
						return u.uploadSymlinkInternal(ctx, entry)
					},
				})

			case fs.File:
				u.stats.NonCachedFiles++
				result = append(result, &uploadWorkItem{
					entry:             entry,
					entryRelativePath: entryRelativePath,
					uploadFunc: func() entryResult {
						return u.uploadFileInternal(ctx, entry)
					},
				})

			default:
				return errors.Errorf("file type not supported: %v", entry.Mode())
			}
		}
		return nil
	})

	return result, resultErr
}

func toChannel(items []*uploadWorkItem) <-chan *uploadWorkItem {
	ch := make(chan *uploadWorkItem)
	go func() {
		defer close(ch)

		for _, wi := range items {
			ch <- wi
		}
	}()

	return ch
}

func (u *Uploader) launchWorkItems(workItems []*uploadWorkItem, wg *sync.WaitGroup) {
	// allocate result channel for each work item.
	for _, it := range workItems {
		it.resultChan = make(chan entryResult, 1)
	}

	workerCount := u.ParallelUploads
	if workerCount == 0 {
		workerCount = runtime.NumCPU()
	}

	ch := toChannel(workItems)
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			for it := range ch {
				it.resultChan <- it.uploadFunc()
			}
		}()
	}
}

func (u *Uploader) processUploadWorkItems(workItems []*uploadWorkItem, dirManifest *snapshot.DirManifest) error {
	var wg sync.WaitGroup
	u.launchWorkItems(workItems, &wg)

	// Read result channels in order.
	for _, it := range workItems {
		result := <-it.resultChan

		if result.err == errCancelled {
			return errCancelled
		}

		if result.err != nil {
			if u.IgnoreFileErrors {
				u.stats.ReadErrors++
				log.Warningf("unable to hash file %q: %s, ignoring", it.entryRelativePath, result.err)
				continue
			}
			return errors.Errorf("unable to process %q: %s", it.entryRelativePath, result.err)
		}

		dirManifest.Entries = append(dirManifest.Entries, result.de)
	}

	// wait for workers, this is technically not needed, but let's make sure we don't leak goroutines
	wg.Wait()

	return nil
}

func maybeReadDirectoryEntries(ctx context.Context, dir fs.Directory) fs.Entries {
	if dir == nil {
		return nil
	}

	ent, err := dir.Readdir(ctx)
	if err != nil {
		log.Warningf("unable to read previous directory entries: %v", err)
		return nil
	}

	return ent
}

func uniqueDirectories(dirs []fs.Directory) []fs.Directory {
	if len(dirs) <= 1 {
		return dirs
	}

	var unique = map[object.ID]fs.Directory{}
	for _, dir := range dirs {
		unique[dir.(object.HasObjectID).ObjectID()] = dir
	}

	if len(unique) == len(dirs) {
		return dirs
	}

	var result []fs.Directory
	for _, d := range unique {
		result = append(result, d)
	}
	return result
}

func uploadDirInternal(
	ctx context.Context,
	u *Uploader,
	directory fs.Directory,
	previousDirs []fs.Directory,
	dirRelativePath string,
) (object.ID, fs.DirectorySummary, error) {
	u.stats.TotalDirectoryCount++

	var summ fs.DirectorySummary
	summ.TotalDirCount = 1

	defer func() {
		summ.IncompleteReason = u.cancelReason()
	}()

	log.Debugf("reading directory %v", dirRelativePath)
	entries, direrr := directory.Readdir(ctx)
	log.Debugf("finished reading directory %v", dirRelativePath)
	if direrr != nil {
		return "", fs.DirectorySummary{}, direrr
	}

	var prevEntries []fs.Entries
	for _, d := range uniqueDirectories(previousDirs) {
		if ent := maybeReadDirectoryEntries(ctx, d); ent != nil {
			prevEntries = append(prevEntries, ent)
		}
	}
	if len(entries) == 0 {
		summ.MaxModTime = directory.ModTime()
	}

	dirManifest := &snapshot.DirManifest{
		StreamType: directoryStreamType,
	}

	if err := u.processSubdirectories(ctx, dirRelativePath, entries, prevEntries, dirManifest, &summ); err != nil && err != errCancelled {
		return "", fs.DirectorySummary{}, err
	}
	u.prepareProgress(dirRelativePath, entries)

	log.Debugf("preparing work items %v", dirRelativePath)
	workItems, workItemErr := u.prepareWorkItems(ctx, dirRelativePath, entries, prevEntries, &summ)
	log.Debugf("finished preparing work items %v", dirRelativePath)
	if workItemErr != nil && workItemErr != errCancelled {
		return "", fs.DirectorySummary{}, workItemErr
	}
	if err := u.processUploadWorkItems(workItems, dirManifest); err != nil && err != errCancelled {
		return "", fs.DirectorySummary{}, err
	}
	log.Debugf("finished processing uploads %v", dirRelativePath)
	dirManifest.Summary = &summ

	writer := u.repo.Objects.NewWriter(ctx, object.WriterOptions{
		Description: "DIR:" + dirRelativePath,
		Prefix:      "k",
	})

	if err := json.NewEncoder(writer).Encode(&dirManifest); err != nil {
		return "", fs.DirectorySummary{}, errors.Wrap(err, "unable to encode directory JSON")
	}

	oid, err := writer.Result()
	return oid, summ, err
}

// NewUploader creates new Uploader object for a given repository.
func NewUploader(r *repo.Repository) *Uploader {
	return &Uploader{
		repo:             r,
		Progress:         &nullUploadProgress{},
		IgnoreFileErrors: true,
		ParallelUploads:  1,
	}
}

// Cancel requests cancellation of an upload that's in progress. Will typically result in an incomplete snapshot.
func (u *Uploader) Cancel() {
	atomic.StoreInt32(&u.canceled, 1)
}

func (u *Uploader) maybeOpenDirectoryFromManifest(man *snapshot.Manifest) fs.Directory {
	if man == nil {
		return nil
	}

	ent, err := newRepoEntry(u.repo, man.RootEntry)
	if err != nil {
		log.Warningf("invalid previous manifest root entry %v: %v", man.RootEntry, err)
		return nil
	}

	dir, ok := ent.(fs.Directory)
	if !ok {
		log.Debugf("previous manifest root is not a directory (was %T %+v)", ent, man.RootEntry)
		return nil
	}

	return dir
}

// Upload uploads contents of the specified filesystem entry (file or directory) to the repository and returns snapshot.Manifest with statistics.
// Old snapshot manifest, when provided can be used to speed up uploads by utilizing hash cache.
func (u *Uploader) Upload(
	ctx context.Context,
	source fs.Entry,
	sourceInfo snapshot.SourceInfo,
	previousManifests ...*snapshot.Manifest,
) (*snapshot.Manifest, error) {
	log.Debugf("Uploading %v", sourceInfo)
	s := &snapshot.Manifest{
		Source: sourceInfo,
	}

	defer u.Progress.UploadFinished()

	u.stats = snapshot.Stats{}

	var err error

	s.StartTime = time.Now()

	switch entry := source.(type) {
	case fs.Directory:
		var previousDirs []fs.Directory
		for _, m := range previousManifests {
			if d := u.maybeOpenDirectoryFromManifest(m); d != nil {
				previousDirs = append(previousDirs, d)
			}
		}

		entry = ignorefs.New(entry, u.FilesPolicy, ignorefs.ReportIgnoredFiles(func(_ string, md fs.Entry) {
			u.stats.AddExcluded(md)
		}))
		s.RootEntry, err = u.uploadDir(ctx, entry, previousDirs)

	case fs.File:
		s.RootEntry, err = u.uploadFile(ctx, entry)

	default:
		return nil, errors.Errorf("unsupported source: %v", s.Source)
	}

	if err != nil {
		return nil, err
	}

	s.IncompleteReason = u.cancelReason()
	s.EndTime = time.Now()
	s.Stats = u.stats
	s.Stats.Content = u.repo.Content.Stats()

	return s, nil
}
