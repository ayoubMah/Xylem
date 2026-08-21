package bitcask

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

func tempLog(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "xylem.log")
}

// TestSetGetDelete is the basic contract.
func TestSetGetDelete(t *testing.T) {
	s, err := Open(tempLog(t), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	if err := s.Set([]byte("user:42"), []byte("ayoub")); err != nil {
		t.Fatalf("Set: %v", err)
	}

	got, err := s.Get([]byte("user:42"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "ayoub" {
		t.Errorf("Get = %q, want %q", got, "ayoub")
	}

	if _, err := s.Get([]byte("missing")); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("Get(missing) = %v, want ErrKeyNotFound", err)
	}

	if err := s.Delete([]byte("user:42")); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get([]byte("user:42")); !errors.Is(err, ErrKeyNotFound) {
		t.Errorf("Get after Delete = %v, want ErrKeyNotFound", err)
	}
}

// TestOverwriteKeepsNewest checks that the append-only log resolves updates by
// order: the last record written for a key is the one that wins.
func TestOverwriteKeepsNewest(t *testing.T) {
	path := tempLog(t)
	s, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	for i := 0; i < 10; i++ {
		if err := s.Set([]byte("k"), []byte(strconv.Itoa(i))); err != nil {
			t.Fatalf("Set %d: %v", i, err)
		}
	}
	if s.Len() != 1 {
		t.Errorf("Len = %d, want 1 (ten writes, one key)", s.Len())
	}

	got, _ := s.Get([]byte("k"))
	if string(got) != "9" {
		t.Errorf("Get = %q, want %q", got, "9")
	}
	s.Close()

	// And the same must hold after replay from disk, not just in memory.
	s2, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	got, _ = s2.Get([]byte("k"))
	if string(got) != "9" {
		t.Errorf("after reopen Get = %q, want %q", got, "9")
	}
}

// TestReopenRecoversEverything is the recovery contract: close the store, open
// it again, and the keydir must be rebuilt from the log alone.
func TestReopenRecoversEverything(t *testing.T) {
	path := tempLog(t)

	s, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	const n = 500
	for i := 0; i < n; i++ {
		if err := s.Set([]byte(fmt.Sprintf("key-%03d", i)), []byte(fmt.Sprintf("value-%03d", i))); err != nil {
			t.Fatalf("Set %d: %v", i, err)
		}
	}
	// Delete a slice of them; tombstones must survive the round trip too.
	for i := 0; i < n; i += 10 {
		if err := s.Delete([]byte(fmt.Sprintf("key-%03d", i))); err != nil {
			t.Fatalf("Delete %d: %v", i, err)
		}
	}
	s.Close()

	s2, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	for i := 0; i < n; i++ {
		key := []byte(fmt.Sprintf("key-%03d", i))
		got, err := s2.Get(key)

		if i%10 == 0 {
			if !errors.Is(err, ErrKeyNotFound) {
				t.Errorf("deleted key %d came back: %v", i, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("Get %d after reopen: %v", i, err)
		}
		if want := fmt.Sprintf("value-%03d", i); string(got) != want {
			t.Errorf("key %d = %q, want %q", i, got, want)
		}
	}
}

// TestOpenTruncatesTornTail proves the store survives a half-written record at
// the end of the log and repairs the file so future appends are safe.
func TestOpenTruncatesTornTail(t *testing.T) {
	path := tempLog(t)

	s, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := 0; i < 50; i++ {
		if err := s.Set([]byte(fmt.Sprintf("k%02d", i)), []byte("payload payload")); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}
	goodSize := s.Size()
	s.Close()

	// Append 11 bytes of junk: a partial record that will not parse.
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("open for junk: %v", err)
	}
	if _, err := f.WriteAt([]byte{0xDE, 0xAD, 0xBE, 0xEF, 1, 2, 3, 4, 5, 6, 7}, goodSize); err != nil {
		t.Fatalf("write junk: %v", err)
	}
	f.Close()

	s2, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen over torn tail: %v", err)
	}
	defer s2.Close()

	if s2.Len() != 50 {
		t.Errorf("Len = %d, want 50", s2.Len())
	}
	if s2.Size() != goodSize {
		t.Errorf("Size = %d, want %d (torn tail should be truncated)", s2.Size(), goodSize)
	}

	// The repaired log must accept new writes and survive another restart.
	if err := s2.Set([]byte("after-repair"), []byte("ok")); err != nil {
		t.Fatalf("Set after repair: %v", err)
	}
	if got, err := s2.Get([]byte("after-repair")); err != nil || string(got) != "ok" {
		t.Errorf("Get after repair = %q, %v", got, err)
	}
}

// TestMergeReclaimsSpace measures the whole point of compaction.
func TestMergeReclaimsSpace(t *testing.T) {
	path := tempLog(t)
	s, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// 100 keys, each overwritten 50 times => 5000 records, 100 of them live.
	for round := 0; round < 50; round++ {
		for i := 0; i < 100; i++ {
			key := []byte(fmt.Sprintf("key-%03d", i))
			val := []byte(fmt.Sprintf("round-%02d-%s", round, bytes.Repeat([]byte("x"), 50)))
			if err := s.Set(key, val); err != nil {
				t.Fatalf("Set: %v", err)
			}
		}
	}
	// Delete 20 of them so tombstones are in the mix.
	for i := 0; i < 20; i++ {
		if err := s.Delete([]byte(fmt.Sprintf("key-%03d", i))); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}

	before := s.Size()
	if err := s.Merge(); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	after := s.Size()

	t.Logf("space amplification: %d bytes before merge, %d after (%.1fx reclaimed)",
		before, after, float64(before)/float64(after))

	if after >= before {
		t.Errorf("Merge did not shrink the log: %d -> %d", before, after)
	}
	if s.Len() != 80 {
		t.Errorf("Len after merge = %d, want 80", s.Len())
	}

	// Every surviving key must still read correctly, at its NEW offset.
	for i := 20; i < 100; i++ {
		key := []byte(fmt.Sprintf("key-%03d", i))
		got, err := s.Get(key)
		if err != nil {
			t.Fatalf("Get %d after merge: %v", i, err)
		}
		if !bytes.HasPrefix(got, []byte("round-49")) {
			t.Errorf("key %d after merge = %q, want the round-49 value", i, got[:8])
		}
	}
	// Deleted keys must stay dead.
	for i := 0; i < 20; i++ {
		if _, err := s.Get([]byte(fmt.Sprintf("key-%03d", i))); !errors.Is(err, ErrKeyNotFound) {
			t.Errorf("deleted key %d resurrected by merge", i)
		}
	}
}

// TestMergeSurvivesReopen: compaction is only correct if the compacted file is
// itself a valid log.
func TestMergeSurvivesReopen(t *testing.T) {
	path := tempLog(t)
	s, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for round := 0; round < 5; round++ {
		for i := 0; i < 50; i++ {
			if err := s.Set([]byte(fmt.Sprintf("k%02d", i)), []byte(fmt.Sprintf("v%d-%d", round, i))); err != nil {
				t.Fatalf("Set: %v", err)
			}
		}
	}
	if err := s.Merge(); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	s.Close()

	s2, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen after merge: %v", err)
	}
	defer s2.Close()

	if s2.Len() != 50 {
		t.Errorf("Len = %d, want 50", s2.Len())
	}
	for i := 0; i < 50; i++ {
		got, err := s2.Get([]byte(fmt.Sprintf("k%02d", i)))
		if err != nil {
			t.Fatalf("Get %d: %v", i, err)
		}
		if want := fmt.Sprintf("v4-%d", i); string(got) != want {
			t.Errorf("k%02d = %q, want %q", i, got, want)
		}
	}
	if _, err := os.Stat(path + ".merge"); !os.IsNotExist(err) {
		t.Errorf(".merge side file was left behind")
	}
}

// TestConcurrentAccess is the race-detector target. Run with -race.
func TestConcurrentAccess(t *testing.T) {
	s, err := Open(tempLog(t), nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	for i := 0; i < 100; i++ {
		if err := s.Set([]byte(fmt.Sprintf("k%03d", i)), []byte("initial")); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	var wg sync.WaitGroup
	// 10 readers, 2 writers, hammering the same keys.
	for r := 0; r < 10; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_, _ = s.Get([]byte(fmt.Sprintf("k%03d", i%100)))
			}
		}()
	}
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_ = s.Set([]byte(fmt.Sprintf("k%03d", i%100)), []byte(fmt.Sprintf("w%d-%d", id, i)))
			}
		}(w)
	}
	wg.Wait()

	if s.Len() != 100 {
		t.Errorf("Len = %d, want 100", s.Len())
	}
}

// ---------------------------------------------------------------------------
// THE SHIP CHECKPOINT: kill mid-write, restart, all committed keys retrievable.
// ---------------------------------------------------------------------------
//
// A process cannot test its own death, so the test re-executes the test binary
// as a CHILD process. The child writes records and calls os.Exit(1) at record
// 500 -- no deferred Close, no flush, no cleanup, the same abruptness as a
// kill -9. The parent then reopens the log and audits what survived.

const (
	crashEnvFlag = "XYLEM_BITCASK_CRASH_CHILD"
	crashEnvPath = "XYLEM_BITCASK_CRASH_PATH"
	crashAt      = 500
	crashTotal   = 1000
)

func TestCrashRecovery(t *testing.T) {
	// --- CHILD BRANCH -------------------------------------------------------
	if os.Getenv(crashEnvFlag) == "1" {
		path := os.Getenv(crashEnvPath)
		s, err := Open(path, nil)
		if err != nil {
			os.Exit(3)
		}
		for i := 0; i < crashTotal; i++ {
			// 4 KiB records, as specified in the roadmap.
			val := bytes.Repeat([]byte("v"), 4096)
			if err := s.Set([]byte(fmt.Sprintf("key-%04d", i)), val); err != nil {
				os.Exit(4)
			}
			if i == crashAt {
				// DIE. No Close, no Sync, no defers. The bytes written so far
				// are in the OS page cache; the kernel outlives this process
				// and will write them out.
				os.Exit(1)
			}
		}
		os.Exit(0)
	}

	// --- PARENT BRANCH ------------------------------------------------------
	path := tempLog(t)

	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashRecovery$", "-test.v")
	cmd.Env = append(os.Environ(),
		crashEnvFlag+"=1",
		crashEnvPath+"="+path,
	)
	err := cmd.Run()

	// The child must have died the way we told it to: exit code 1.
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("child did not exit with an error, got %v", err)
	}
	if code := exitErr.ExitCode(); code != 1 {
		t.Fatalf("child exit code = %d, want 1 (3=open failed, 4=set failed)", code)
	}

	// --- THE AUDIT ----------------------------------------------------------
	s, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open after crash: %v", err)
	}
	defer s.Close()

	// Every key the child wrote BEFORE the kill must be retrievable and
	// correct. Record `crashAt` was written and Set returned, so it counts as
	// committed too -- hence crashAt+1 keys.
	const committed = crashAt + 1
	for i := 0; i < committed; i++ {
		key := []byte(fmt.Sprintf("key-%04d", i))
		got, err := s.Get(key)
		if err != nil {
			t.Fatalf("committed key %d lost after crash: %v", i, err)
		}
		if len(got) != 4096 {
			t.Fatalf("key %d: value length %d, want 4096", i, len(got))
		}
	}

	// Nothing after the kill may exist.
	for i := committed; i < crashTotal; i++ {
		if _, err := s.Get([]byte(fmt.Sprintf("key-%04d", i))); !errors.Is(err, ErrKeyNotFound) {
			t.Errorf("key %d survived although it was never written", i)
		}
	}

	if s.Len() != committed {
		t.Errorf("recovered %d keys, want %d", s.Len(), committed)
	}
	t.Logf("crash at record %d: %d/%d committed keys recovered, log is %d bytes",
		crashAt, s.Len(), crashTotal, s.Size())

	// And the recovered store must be writable again.
	if err := s.Set([]byte("post-crash"), []byte("ok")); err != nil {
		t.Fatalf("Set after crash recovery: %v", err)
	}
}
