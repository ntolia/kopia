package endtoend_test

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kylelemons/godebug/pretty"
)

const repoPassword = "qWQPJ2hiiLgWRRCr" // nolint:gosec

type testenv struct {
	repoDir   string
	configDir string
	dataDir   string
	exe       string

	fixedArgs   []string
	environment []string
}

type sourceInfo struct {
	user      string
	host      string
	path      string
	snapshots []snapshotInfo
}

type snapshotInfo struct {
	objectID string
	time     time.Time
}

func newTestEnv(t *testing.T) *testenv {
	exe := os.Getenv("KOPIA_EXE")
	if exe == "" {
		t.Skip("KOPIA_EXE not set in the environment, skipping test")
	}

	repoDir, err := ioutil.TempDir("", "kopia-repo")
	if err != nil {
		t.Fatalf("can't create temp directory: %v", err)
	}

	configDir, err := ioutil.TempDir("", "kopia-config")
	if err != nil {
		t.Fatalf("can't create temp directory: %v", err)
	}

	dataDir, err := ioutil.TempDir("", "kopia-data")
	if err != nil {
		t.Fatalf("can't create temp directory: %v", err)
	}

	return &testenv{
		repoDir:   repoDir,
		configDir: configDir,
		dataDir:   dataDir,
		exe:       exe,
		fixedArgs: []string{
			// use per-test config file, to avoid clobbering current user's setup.
			"--config-file", filepath.Join(configDir, ".kopia.config"),
		},
		environment: []string{"KOPIA_PASSWORD=" + repoPassword},
	}
}

func (e *testenv) cleanup(t *testing.T) {
	if t.Failed() {
		t.Logf("skipped cleanup for failed test, examine repository: %v", e.repoDir)
		return
	}
	if e.repoDir != "" {
		os.RemoveAll(e.repoDir)
	}
	if e.configDir != "" {
		os.RemoveAll(e.configDir)
	}
	if e.dataDir != "" {
		os.RemoveAll(e.dataDir)
	}
}

func TestEndToEnd(t *testing.T) {
	e := newTestEnv(t)
	defer e.cleanup(t)
	defer e.runAndExpectSuccess(t, "repo", "disconnect")

	e.runAndExpectSuccess(t, "repo", "create", "filesystem", "--path", e.repoDir)

	// make sure we can read policy
	e.runAndExpectSuccess(t, "policy", "show", "--global")

	t.Run("VerifyGlobalPolicy", func(t *testing.T) {
		// verify we created global policy entry
		globalPolicyBlockID := e.runAndVerifyOutputLineCount(t, 1, "content", "ls")[0]
		e.runAndExpectSuccess(t, "content", "show", "-jz", globalPolicyBlockID)

		// make sure the policy is visible in the manifest list
		e.runAndVerifyOutputLineCount(t, 1, "manifest", "list", "--filter=type:policy", "--filter=policyType:global")

		// make sure the policy is visible in the policy list
		e.runAndVerifyOutputLineCount(t, 1, "policy", "list")
	})

	t.Run("Reconnect", func(t *testing.T) {
		e.runAndExpectSuccess(t, "repo", "disconnect")
		e.runAndExpectSuccess(t, "repo", "connect", "filesystem", "--path", e.repoDir)
		e.runAndExpectSuccess(t, "repo", "status")
	})

	t.Run("ReconnectUsingToken", func(t *testing.T) {
		lines := e.runAndExpectSuccess(t, "repo", "status", "-t", "-s")
		prefix := "$ kopia "
		var reconnectArgs []string

		// look for output line containing the prefix - this will be our reconnect command
		for _, l := range lines {
			if strings.HasPrefix(l, prefix) {
				reconnectArgs = strings.Split(strings.TrimPrefix(l, prefix), " ")
			}
		}

		if reconnectArgs == nil {
			t.Fatalf("can't find reonnect command in kopia repo status output")
		}

		e.runAndExpectSuccess(t, "repo", "disconnect")
		e.runAndExpectSuccess(t, reconnectArgs...)
		e.runAndExpectSuccess(t, "repo", "status")
	})

	e.runAndExpectSuccess(t, "snapshot", "create", ".")
	e.runAndExpectSuccess(t, "snapshot", "list", ".")

	dir1 := filepath.Join(e.dataDir, "dir1")
	createDirectory(t, dir1, 3)
	e.runAndExpectSuccess(t, "snapshot", "create", dir1)
	e.runAndExpectSuccess(t, "snapshot", "create", dir1)

	dir2 := filepath.Join(e.dataDir, "dir2")
	createDirectory(t, dir2, 3)
	e.runAndExpectSuccess(t, "snapshot", "create", dir2)
	e.runAndExpectSuccess(t, "snapshot", "create", dir2)
	sources := listSnapshotsAndExpectSuccess(t, e)
	if got, want := len(sources), 3; got != want {
		t.Errorf("unexpected number of sources: %v, want %v in %#v", got, want, sources)
	}

	// expect 5 blobs, each snapshot creation adds one index blob
	e.runAndVerifyOutputLineCount(t, 6, "index", "ls")
	e.runAndExpectSuccess(t, "index", "optimize")
	e.runAndVerifyOutputLineCount(t, 1, "index", "ls")

	e.runAndExpectSuccess(t, "snapshot", "create", ".", dir1, dir2)
	e.runAndVerifyOutputLineCount(t, 2, "index", "ls")

	t.Run("Migrate", func(t *testing.T) {
		dstenv := newTestEnv(t)
		defer dstenv.cleanup(t)
		defer dstenv.runAndExpectSuccess(t, "repo", "disconnect")

		dstenv.runAndExpectSuccess(t, "repo", "create", "filesystem", "--path", dstenv.repoDir)
		dstenv.runAndExpectSuccess(t, "snapshot", "migrate", "--source-config", filepath.Join(e.configDir, ".kopia.config"), "--all")
		// migrate again, which should be a no-op.
		dstenv.runAndExpectSuccess(t, "snapshot", "migrate", "--source-config", filepath.Join(e.configDir, ".kopia.config"), "--all")

		sourceSnapshotCount := len(e.runAndExpectSuccess(t, "snapshot", "list", ".", "-a"))
		dstenv.runAndVerifyOutputLineCount(t, sourceSnapshotCount, "snapshot", "list", ".", "-a")
	})

	t.Run("RepairIndexBlobs", func(t *testing.T) {
		contentsBefore := e.runAndExpectSuccess(t, "content", "ls")

		lines := e.runAndVerifyOutputLineCount(t, 2, "index", "ls")
		for _, l := range lines {
			indexFile := strings.Split(l, " ")[0]
			e.runAndExpectSuccess(t, "blob", "delete", indexFile)
		}

		// there should be no index files at this point
		e.runAndVerifyOutputLineCount(t, 0, "index", "ls", "--no-list-caching")
		// there should be no blocks, since there are no indexesto find them
		e.runAndVerifyOutputLineCount(t, 0, "content", "ls")

		// now recover index from all blocks
		e.runAndExpectSuccess(t, "index", "recover", "--commit")

		// all recovered index entries are added as index file
		e.runAndVerifyOutputLineCount(t, 1, "index", "ls")
		contentsAfter := e.runAndExpectSuccess(t, "content", "ls")
		if diff := pretty.Compare(contentsBefore, contentsAfter); diff != "" {
			t.Errorf("unexpected block diff after recovery: %v", diff)
		}
	})

	t.Run("RepairFormatBlob", func(t *testing.T) {
		// remove kopia.repository
		e.runAndExpectSuccess(t, "blob", "rm", "kopia.repository")
		e.runAndExpectSuccess(t, "repo", "disconnect")

		// this will fail because the format blob in the repository is not found
		e.runAndExpectFailure(t, "repo", "connect", "filesystem", "--path", e.repoDir)

		// now run repair, which will recover the format blob from one of the pack blobs.
		e.runAndExpectSuccess(t, "repo", "repair", "--log-level=debug", "--trace-storage", "filesystem", "--path", e.repoDir)

		// now connect can succeed
		e.runAndExpectSuccess(t, "repo", "connect", "filesystem", "--path", e.repoDir)
	})
}

func TestDiff(t *testing.T) {
	e := newTestEnv(t)
	defer e.cleanup(t)
	defer e.runAndExpectSuccess(t, "repo", "disconnect")

	e.runAndExpectSuccess(t, "repo", "create", "filesystem", "--path", e.repoDir)

	dataDir := filepath.Join(e.dataDir, "dir1")

	// initial snapshot
	assertNoError(t, os.MkdirAll(dataDir, 0777))
	e.runAndExpectSuccess(t, "snapshot", "create", dataDir)

	// create some directories and files
	assertNoError(t, os.MkdirAll(filepath.Join(dataDir, "foo"), 0700))
	assertNoError(t, ioutil.WriteFile(filepath.Join(dataDir, "some-file1"), []byte(`
hello world
how are you
`), 0600))
	assertNoError(t, ioutil.WriteFile(filepath.Join(dataDir, "some-file2"), []byte(`
quick brown
fox jumps
over the lazy
dog
`), 0600))
	e.runAndExpectSuccess(t, "snapshot", "create", dataDir)

	// change some files
	assertNoError(t, ioutil.WriteFile(filepath.Join(dataDir, "some-file2"), []byte(`
quick brown
fox jumps
over the lazy
canary
`), 0600))

	assertNoError(t, os.MkdirAll(filepath.Join(dataDir, "bar"), 0700))
	e.runAndExpectSuccess(t, "snapshot", "create", dataDir)

	// change some files
	os.Remove(filepath.Join(dataDir, "some-file1"))

	assertNoError(t, os.MkdirAll(filepath.Join(dataDir, "bar"), 0700))
	e.runAndExpectSuccess(t, "snapshot", "create", dataDir)

	si := listSnapshotsAndExpectSuccess(t, e, dataDir)
	if got, want := len(si), 1; got != want {
		t.Fatalf("got %v sources, wanted %v", got, want)
	}

	// make sure we can generate between all versions of the directory
	snapshots := si[0].snapshots
	for _, s1 := range snapshots {
		for _, s2 := range snapshots {
			e.runAndExpectSuccess(t, "diff", "-f", s1.objectID, s2.objectID)
		}
	}
}

func (e *testenv) runAndExpectSuccess(t *testing.T, args ...string) []string {
	t.Helper()
	stdout, err := e.run(t, args...)
	if err != nil {
		t.Fatalf("'kopia %v' failed with %v", strings.Join(args, " "), err)
	}
	return stdout
}

func (e *testenv) runAndExpectFailure(t *testing.T, args ...string) []string {
	t.Helper()
	stdout, err := e.run(t, args...)
	if err == nil {
		t.Fatalf("'kopia %v' succeeded, but expected failure", strings.Join(args, " "))
	}
	return stdout
}

func (e *testenv) runAndVerifyOutputLineCount(t *testing.T, wantLines int, args ...string) []string {
	t.Helper()
	lines := e.runAndExpectSuccess(t, args...)
	if len(lines) != wantLines {
		t.Errorf("unexpected list of results of 'kopia %v': %v (%v lines), wanted %v", strings.Join(args, " "), lines, len(lines), wantLines)
	}
	return lines
}

func (e *testenv) run(t *testing.T, args ...string) ([]string, error) {
	t.Helper()
	t.Logf("running 'kopia %v'", strings.Join(args, " "))
	cmdArgs := append(append([]string(nil), e.fixedArgs...), args...)
	c := exec.Command(e.exe, cmdArgs...)
	c.Env = append(os.Environ(), e.environment...)

	stderrPipe, err := c.StderrPipe()
	if err != nil {
		t.Fatalf("can't set up stderr pipe reader")
	}

	var stderr []byte
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()

		stderr, err = ioutil.ReadAll(stderrPipe)
	}()

	o, err := c.Output()
	wg.Wait()

	t.Logf("finished 'kopia %v' with err=%v and output:\n%v\nstderr:\n%v\n", strings.Join(args, " "), err, trimOutput(string(o)), trimOutput(string(stderr)))
	return splitLines(string(o)), err
}

func trimOutput(s string) string {
	lines := splitLines(s)
	if len(lines) <= 100 {
		return s
	}

	lines2 := append([]string(nil), lines[0:50]...)
	lines2 = append(lines2, fmt.Sprintf("/* %v lines removed */", len(lines)-100))
	lines2 = append(lines2, lines[len(lines)-50:]...)

	return strings.Join(lines2, "\n")

}
func listSnapshotsAndExpectSuccess(t *testing.T, e *testenv, targets ...string) []sourceInfo {
	lines := e.runAndExpectSuccess(t, append([]string{"snapshot", "list", "-l"}, targets...)...)
	return mustParseSnapshots(t, lines)
}

func createDirectory(t *testing.T, dirname string, depth int) {
	if err := os.MkdirAll(dirname, 0700); err != nil {
		t.Fatalf("unable to create directory %v: %v", dirname, err)
	}

	if depth > 0 {
		numSubDirs := rand.Intn(10) + 1
		for i := 0; i < numSubDirs; i++ {
			subdirName := randomName()

			createDirectory(t, filepath.Join(dirname, subdirName), depth-1)
		}
	}

	numFiles := rand.Intn(10) + 1
	for i := 0; i < numFiles; i++ {
		fileName := randomName()

		createRandomFile(t, filepath.Join(dirname, fileName))
	}
}

func createRandomFile(t *testing.T, filename string) {
	f, err := os.Create(filename)
	if err != nil {
		t.Fatalf("unable to create random file: %v", err)
	}
	defer f.Close()

	length := rand.Int63n(100000)
	_, err = io.Copy(f, io.LimitReader(rand.New(rand.NewSource(1)), length))
	assertNoError(t, err)
}

func mustParseSnapshots(t *testing.T, lines []string) []sourceInfo {
	var result []sourceInfo

	var currentSource *sourceInfo

	for _, l := range lines {
		if l == "" {
			continue
		}

		if strings.HasPrefix(l, "  ") {
			if currentSource == nil {
				t.Errorf("snapshot without a source: %q", l)
				return nil
			}
			currentSource.snapshots = append(currentSource.snapshots, mustParseSnaphotInfo(t, l[2:]))
			continue
		}

		s := mustParseSourceInfo(t, l)
		result = append(result, s)
		currentSource = &result[len(result)-1]
	}

	return result
}

func randomName() string {
	b := make([]byte, rand.Intn(10)+3)
	cryptorand.Read(b) // nolint:errcheck
	return hex.EncodeToString(b)
}

func mustParseSnaphotInfo(t *testing.T, l string) snapshotInfo {
	parts := strings.Split(l, " ")
	ts, err := time.Parse("2006-01-02 15:04:05 MST", strings.Join(parts[0:3], " "))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	return snapshotInfo{
		time:     ts,
		objectID: parts[3],
	}
}

func mustParseSourceInfo(t *testing.T, l string) sourceInfo {
	p1 := strings.Index(l, "@")
	p2 := strings.Index(l, ":")
	if p1 >= 0 && p2 > p1 {
		return sourceInfo{user: l[0:p1], host: l[p1+1 : p2], path: l[p2+1:]}
	}

	t.Fatalf("can't parse source info: %q", l)
	return sourceInfo{}
}

func splitLines(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}

	var result []string
	for _, l := range strings.Split(s, "\n") {
		result = append(result, strings.TrimRight(l, "\r"))
	}
	return result
}

func assertNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("err: %v", err)
	}
}
