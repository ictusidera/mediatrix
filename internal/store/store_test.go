package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ictusidera/mediatrix/internal/model"
)

func newStore(t *testing.T, maxFile, maxStore int64) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), maxFile, maxStore)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func put(t *testing.T, s *Store, data string) model.FileInfo {
	t.Helper()
	info, err := s.Put(context.Background(), strings.NewReader(data), "")
	if err != nil {
		t.Fatal(err)
	}
	return info
}
func service(name string) model.Service {
	return model.Service{Name: "service:" + name, URL: "http://127.0.0.1:8000/rpc", AllowedPeers: []string{"peer-a"}}
}
func assertNoStaging(t *testing.T, s *Store) {
	t.Helper()
	entries, err := os.ReadDir(s.blobs)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".upload-") {
			t.Errorf("upload staging left behind: %s", e.Name())
		}
	}
	entries, err = os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".registry-") {
			t.Errorf("registry staging left behind: %s", e.Name())
		}
	}
}
func registryBytes(t *testing.T, s *Store) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.dir, registryName))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestRestartRegistryAndCopies(t *testing.T) {
	s := newStore(t, 1024, 2048)
	input := service("echo")
	if err := s.PutService(input); err != nil {
		t.Fatal(err)
	}
	input.AllowedPeers[0] = "changed"
	stored, _ := s.GetService(input.Name)
	if stored.AllowedPeers[0] != "peer-a" {
		t.Fatal("service input not copied")
	}
	stored.AllowedPeers[0] = "changed"
	listed := s.Services()
	listed[0].AllowedPeers[0] = "changed"
	info := put(t, s, "hello")
	sum := sha256.Sum256([]byte("hello"))
	if info.Key != model.FileKey(hex.EncodeToString(sum[:])) || info.Size != 5 {
		t.Fatal(info)
	}
	if len(info.AllowedPeers) != 0 {
		t.Fatal("new files must be private")
	}
	peers := []string{"peer-b"}
	if err := s.SetFileAccess(info.Key, peers); err != nil {
		t.Fatal(err)
	}
	peers[0] = "changed"
	fi, _ := s.GetFile(info.Key)
	fi.AllowedPeers[0] = "changed"
	files := s.Files()
	files[0].AllowedPeers[0] = "changed"
	dir := s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, 1024, 2048)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, ok := reopened.GetService(input.Name)
	if !ok || restored.AllowedPeers[0] != "peer-a" {
		t.Fatal(restored)
	}
	r, restoredFile, err := reopened.OpenFile(info.Key)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" || restoredFile.AllowedPeers[0] != "peer-b" {
		t.Fatalf("got %q, %+v", data, restoredFile)
	}
	if err := reopened.DeleteService(input.Name); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(dir, 1024, 2048)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if len(again.Services()) != 0 {
		t.Fatal("deleted service reappeared")
	}
}

func TestDuplicatePreservesACLAtQuota(t *testing.T) {
	s := newStore(t, 8, 4)
	info := put(t, s, "data")
	if err := s.SetFileAccess(info.Key, []string{"peer"}); err != nil {
		t.Fatal(err)
	}
	before := registryBytes(t, s)
	for _, key := range []string{"", info.Key} {
		same, err := s.Put(context.Background(), strings.NewReader("data"), key)
		if err != nil {
			t.Fatal(err)
		}
		if len(same.AllowedPeers) != 1 || same.AllowedPeers[0] != "peer" {
			t.Fatal("duplicate changed ACL")
		}
	}
	if !bytes.Equal(before, registryBytes(t, s)) {
		t.Fatal("duplicate rewrote registry")
	}
	if len(s.Files()) != 1 || s.total != 4 {
		t.Fatal("duplicate consumes quota")
	}
	assertNoStaging(t, s)
}

func TestQuotasAndFailedUploadPreserveRegistry(t *testing.T) {
	s := newStore(t, 5, 7)
	put(t, s, "first")
	before := registryBytes(t, s)
	cases := []struct {
		data string
		want error
	}{
		{"second", ErrTooLarge},
		{"more", ErrStoreFull},
	}
	for _, tc := range cases {
		_, err := s.Put(context.Background(), strings.NewReader(tc.data), "")
		if !errors.Is(err, tc.want) {
			t.Errorf("%q: got %v want %v", tc.data, err, tc.want)
		}
		if !bytes.Equal(before, registryBytes(t, s)) {
			t.Fatal("failed upload changed registry")
		}
		assertNoStaging(t, s)
	}
	got := put(t, s, "ok")
	if got.Size != 2 {
		t.Fatal(got)
	}
	empty := put(t, s, "")
	if empty.Size != 0 || len(s.Files()) != 3 {
		t.Fatal(empty)
	}
	var total int64
	entries, _ := os.ReadDir(s.blobs)
	for _, e := range entries {
		info, _ := e.Info()
		total += info.Size()
	}
	if total != 7 {
		t.Fatalf("disk blob bytes = %d", total)
	}
}

type cancelReader struct{ cancel context.CancelFunc }

func (r cancelReader) Read(p []byte) (int, error) { r.cancel(); return copy(p, "cancelled"), io.EOF }

type brokenReader struct{}

func (brokenReader) Read(p []byte) (int, error) { return copy(p, "partial"), errors.New("read failed") }

type stalledReader struct{}

func (stalledReader) Read([]byte) (int, error) { return 0, nil }
func TestCanceledAndBrokenUploadsCleanup(t *testing.T) {
	s := newStore(t, 100, 100)
	before := registryBytes(t, s)
	ctx, cancel := context.WithCancel(context.Background())
	_, err := s.Put(ctx, cancelReader{cancel}, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_, err = s.Put(ctx, strings.NewReader("unused"), "")
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err = s.Put(context.Background(), brokenReader{}, ""); err == nil {
		t.Fatal("reader error accepted")
	}
	if _, err = s.Put(context.Background(), stalledReader{}, ""); !errors.Is(err, io.ErrNoProgress) {
		t.Fatal(err)
	}
	if len(s.Files()) != 0 {
		t.Fatal("failed file registered")
	}
	if !bytes.Equal(before, registryBytes(t, s)) {
		t.Fatal("failure changed registry")
	}
	assertNoStaging(t, s)
	entries, _ := os.ReadDir(s.blobs)
	if len(entries) != 0 {
		t.Fatal("failed upload left blob")
	}
}
func TestRequestedKeyMismatchAndInvalidKeys(t *testing.T) {
	s := newStore(t, 100, 100)
	info := put(t, s, "known")
	for _, key := range []string{"", info.Key} {
		if key == "" {
			key = model.FileKey(strings.Repeat("f", 64))
		}
		_, err := s.Put(context.Background(), strings.NewReader("different"), key)
		if !errors.Is(err, ErrHashMismatch) {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"../registry.json", "file:sha256:../escape", strings.Repeat("a", 64), "file:sha256:" + strings.Repeat("A", 64)} {
		if _, _, err := s.OpenFile(key); err == nil {
			t.Errorf("opened invalid key %q", key)
		}
		if _, err := s.Put(context.Background(), strings.NewReader("data"), key); err == nil {
			t.Errorf("accepted invalid key %q", key)
		}
		if err := s.SetFileAccess(key, nil); err == nil {
			t.Errorf("ACL accepted invalid key %q", key)
		}
	}
	assertNoStaging(t, s)
}
func TestCorruptionDetectedBeforeServe(t *testing.T) {
	for _, content := range []string{"evil!", "x", "toolong"} {
		t.Run(content, func(t *testing.T) {
			s := newStore(t, 100, 100)
			info := put(t, s, "hello")
			if err := os.WriteFile(s.blobPath(info.Key), []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			r, _, err := s.OpenFile(info.Key)
			if !errors.Is(err, ErrCorrupt) || r != nil {
				t.Fatalf("corrupt file exposed: %v %v", r, err)
			}
			if _, err := s.Put(context.Background(), strings.NewReader("hello"), info.Key); !errors.Is(err, ErrCorrupt) {
				t.Fatal("duplicate accepted corrupt blob", err)
			}
		})
	}
}
func TestRegistryStrictDecoding(t *testing.T) {
	cases := []string{
		`{`, `{}`, `null`, `{"version":2,"services":[],"files":[]}`,
		`{"version":1,"services":null,"files":[]}`,
		`{"version":1,"services":[],"files":[],"secret":"no"}`,
		`{"version":1,"version":1,"services":[],"files":[]}`,
		`{"version":1,"services":[],"files":[]} {}`,
		`{"version":1,"services":[{"name":"service:x","url":"http://127.0.0.1","allowed_peers":[],"extra":true}],"files":[]}`,
		`{"version":1,"services":[{"name":"service:x","url":"http://127.0.0.1","url":"http://127.0.0.2","allowed_peers":[]}],"files":[]}`,
		`{"version":1,"services":[],"files":[{"key":"../../escape","size":1,"allowed_peers":[]}]}`,
		`{"version":1,"services":[{"name":"service:x","url":"http://127.0.0.1","allowed_peers":[]},{"name":"service:x","url":"http://127.0.0.1","allowed_peers":[]}],"files":[]}`,
	}
	for i, data := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, registryName), []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if s, err := Open(dir, 100, 100); err == nil {
				s.Close()
				t.Fatal("invalid registry accepted")
			}
			actual, _ := os.ReadFile(filepath.Join(dir, registryName))
			if string(actual) != data {
				t.Fatal("invalid registry modified")
			}
			if _, err := os.Stat(filepath.Join(dir, lockName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("lock not released after failed open: %v", err)
			}
		})
	}
}
func TestLockConflictAndExplicitStaleCleanup(t *testing.T) {
	s := newStore(t, 100, 100)
	if other, err := Open(s.dir, 100, 100); !errors.Is(err, ErrLocked) {
		if other != nil {
			other.Close()
		}
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(s.dir, lockName)
	if err := os.Mkdir(lock, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lock, "owner"), []byte("pid=999999999\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if other, err := Open(s.dir, 100, 100); !errors.Is(err, ErrLocked) {
		if other != nil {
			other.Close()
		}
		t.Fatal("stale lock stolen", err)
	}
	if err := os.Remove(filepath.Join(lock, "owner")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	again, err := Open(s.dir, 100, 100)
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
}
func TestConcurrentMutationsAndReads(t *testing.T) {
	s := newStore(t, 100, 10000)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			data := fmt.Sprintf("blob-%d", i%8)
			info, err := s.Put(context.Background(), strings.NewReader(data), "")
			if err != nil {
				t.Error(err)
				return
			}
			if err = s.SetFileAccess(info.Key, []string{fmt.Sprintf("peer-%d", i)}); err != nil {
				t.Error(err)
			}
			if err = s.PutService(service(fmt.Sprintf("service-%d", i))); err != nil {
				t.Error(err)
			}
			_, _ = s.GetService("service:service-0")
			_ = s.Services()
			_ = s.Files()
			_, _ = s.GetFile(info.Key)
			f, _, err := s.OpenFile(info.Key)
			if err != nil {
				t.Error(err)
				return
			}
			content, err := io.ReadAll(f)
			_ = f.Close()
			if err != nil || string(content) != data {
				t.Errorf("bad content %q: %v", content, err)
			}
		}(i)
	}
	wg.Wait()
	if len(s.Files()) != 8 || len(s.Services()) != 24 {
		t.Fatalf("got %d files, %d services", len(s.Files()), len(s.Services()))
	}
	assertNoStaging(t, s)
}
func TestOrphanRecoveryAndMissingRegistryFailClosed(t *testing.T) {
	s := newStore(t, 100, 100)
	good := put(t, s, "good")
	dir := s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, "blobs", strings.Repeat("a", 64))
	staging := filepath.Join(dir, "blobs", ".upload-interrupted")
	registryStaging := filepath.Join(dir, ".registry-interrupted")
	for _, path := range []string{orphan, staging, registryStaging} {
		if err := os.WriteFile(path, []byte("unfinished"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	again, err := Open(dir, 100, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{orphan, staging, registryStaging} {
		if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("orphan retained %s", path)
		}
	}
	if _, ok := again.GetFile(good.Key); !ok {
		t.Fatal("lost committed blob")
	}
	again.Close()
	if err = os.Remove(filepath.Join(dir, registryName)); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(dir, 100, 100); err == nil {
		s.Close()
		t.Fatal("silently recreated missing registry")
	}
}
func TestFailedRegistryMutationKeepsMemoryAndBlobState(t *testing.T) {
	s := newStore(t, 100, 100)
	original := service("original")
	if err := s.PutService(original); err != nil {
		t.Fatal(err)
	}
	info := put(t, s, "original")
	originalRegistry := registryBytes(t, s)
	path := filepath.Join(s.dir, registryName)
	backup := path + ".backup"
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := s.PutService(service("new")); err == nil {
		t.Fatal("mutation accepted invalid registry target")
	}
	if err := s.DeleteService(original.Name); err == nil {
		t.Fatal("deletion accepted invalid registry target")
	}
	if err := s.SetFileAccess(info.Key, []string{"new"}); err == nil {
		t.Fatal("ACL mutation accepted invalid registry target")
	}
	if _, err := s.Put(context.Background(), strings.NewReader("new"), ""); err == nil {
		t.Fatal("blob commit accepted invalid registry target")
	}
	if len(s.Services()) != 1 || len(s.Files()) != 1 {
		t.Fatal("failed mutation changed memory")
	}
	unchanged, _ := s.GetFile(info.Key)
	if len(unchanged.AllowedPeers) != 0 {
		t.Fatal("failed ACL mutation changed memory")
	}
	entries, _ := os.ReadDir(s.blobs)
	if len(entries) != 1 {
		t.Fatal("failed commit left blob")
	}
	actual, _ := os.ReadFile(backup)
	if !bytes.Equal(actual, originalRegistry) {
		t.Fatal("previous registry modified")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(backup, path); err != nil {
		t.Fatal(err)
	}
	assertNoStaging(t, s)
}
func TestRejectSymlinks(t *testing.T) {
	t.Run("root", func(t *testing.T) {
		dir := t.TempDir()
		target := t.TempDir()
		link := filepath.Join(dir, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Skip(err)
		}
		if s, err := Open(link, 100, 100); err == nil {
			s.Close()
			t.Fatal("accepted root symlink")
		}
	})
	t.Run("blobs", func(t *testing.T) {
		dir := t.TempDir()
		target := t.TempDir()
		if err := os.Symlink(target, filepath.Join(dir, "blobs")); err != nil {
			t.Skip(err)
		}
		if s, err := Open(dir, 100, 100); err == nil {
			s.Close()
			t.Fatal("accepted blobs symlink")
		}
	})
	t.Run("registry", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(t.TempDir(), "target")
		data := []byte(`{"version":1,"services":[],"files":[]}`)
		if err := os.WriteFile(target, data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(dir, registryName)); err != nil {
			t.Skip(err)
		}
		if s, err := Open(dir, 100, 100); err == nil {
			s.Close()
			t.Fatal("accepted registry symlink")
		}
	})
	t.Run("blob", func(t *testing.T) {
		s := newStore(t, 100, 100)
		info := put(t, s, "blob")
		path := s.blobPath(info.Key)
		target := filepath.Join(t.TempDir(), "target")
		if err := os.WriteFile(target, []byte("blob"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Skip(err)
		}
		if f, _, err := s.OpenFile(info.Key); err == nil {
			f.Close()
			t.Fatal("served symlink blob")
		}
	})
}
func TestServiceValidationAndClosedStore(t *testing.T) {
	s := newStore(t, 100, 100)
	for _, url := range []string{"http://user:password@127.0.0.1", "http://127.0.0.1?token=secret", "http://127.0.0.1#secret", "http://example.com", "http://localhost", "http://192.168.1.1", "https://127.0.0.1", "file:///tmp/secret"} {
		v := service("test")
		v.URL = url
		if err := s.PutService(v); err == nil {
			t.Errorf("accepted URL %q", url)
		}
	}
	v := service("ipv6")
	v.URL = "http://[::1]:8000/rpc"
	if err := s.PutService(v); err != nil {
		t.Fatal(err)
	}
	info := put(t, s, "blob")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.PutService(service("closed")); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err := s.DeleteService(v.Name); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if err := s.SetFileAccess(info.Key, nil); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if _, err := s.Put(context.Background(), strings.NewReader("new"), ""); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if _, _, err := s.OpenFile(info.Key); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

type waitingReader struct {
	started chan struct{}
	release chan struct{}
}

func (r waitingReader) Read([]byte) (int, error) { close(r.started); <-r.release; return 0, io.EOF }
func TestQueuedUploadCancellationAndRegistryAvailability(t *testing.T) {
	s := newStore(t, 100, 100)
	started := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() { _, err := s.Put(context.Background(), waitingReader{started, release}, ""); firstDone <- err }()
	<-started
	// Registry access must remain available while a network reader is slow.
	registryDone := make(chan error, 1)
	go func() { registryDone <- s.PutService(service("responsive")) }()
	select {
	case err := <-registryDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(release)
		<-firstDone
		t.Fatal("slow upload blocked registry")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { _, err := s.Put(ctx, strings.NewReader("waiting"), ""); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(release)
		<-firstDone
		t.Fatal("queued upload did not cancel")
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}
