// Package store provides a private, single-daemon registry and immutable,
// content-addressed blob storage. See doc.go for the filesystem trust boundary.
package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/ictusidera/mediatrix/internal/model"
)

const (
	registryVersion = 1
	maxRegistrySize = 16 << 20
	registryName    = "registry.json"
	lockName        = ".daemon-lock"
)

var (
	ErrLocked       = errors.New("store is locked; stop the owning daemon before explicitly removing a stale .daemon-lock directory")
	ErrClosed       = errors.New("store is closed")
	ErrNotFound     = errors.New("file not found")
	ErrTooLarge     = errors.New("file exceeds the per-file size limit")
	ErrStoreFull    = errors.New("store exceeds its total size limit")
	ErrHashMismatch = errors.New("file content does not match the requested SHA-256 key")
	ErrCorrupt      = errors.New("stored file failed integrity verification")
)

type registry struct {
	Version  int              `json:"version"`
	Services []model.Service  `json:"services"`
	Files    []model.FileInfo `json:"files"`
}

// Store owns its directory until Close. Concurrent callers are safe. Uploads are
// serialized so committed data plus upload staging never exceeds maxStore.
// Methods returning values copy ACL slices rather than exposing internal state.
type Store struct {
	mu                       sync.RWMutex
	uploads                  chan struct{}
	dir, blobs               string
	maxFile, maxStore, total int64
	services                 map[string]model.Service
	files                    map[string]model.FileInfo
	closed                   bool
}

// Open opens or initializes dir. The directory must be local to the daemon and
// not writable by untrusted users. Limits apply to blob bytes, not the bounded
// registry or filesystem metadata. A stale lock is never automatically stolen.
func Open(dir string, maxFile, maxStore int64) (_ *Store, err error) {
	if maxFile <= 0 || maxStore <= 0 {
		return nil, errors.New("storage limits must be positive")
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err = privateDir(dir); err != nil {
		return nil, fmt.Errorf("store directory: %w", err)
	}
	lock := filepath.Join(dir, lockName)
	if err = os.Mkdir(lock, 0700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("create store lock: %w", err)
	}
	// Creation, rather than a PID liveness guess, grants exclusive ownership.
	defer func() {
		if err != nil {
			_ = releaseLock(lock)
		}
	}()
	if err = os.WriteFile(filepath.Join(lock, "owner"), []byte("pid="+strconv.Itoa(os.Getpid())+"\n"), 0600); err != nil {
		return nil, err
	}
	s := &Store{uploads: make(chan struct{}, 1), dir: dir, blobs: filepath.Join(dir, "blobs"), maxFile: maxFile, maxStore: maxStore, services: make(map[string]model.Service), files: make(map[string]model.FileInfo)}
	if err = privateDir(s.blobs); err != nil {
		return nil, fmt.Errorf("blob directory: %w", err)
	}
	f, err := openRegular(filepath.Join(dir, registryName))
	if errors.Is(err, os.ErrNotExist) {
		// Never silently create an empty registry over existing committed-looking
		// data: a missing registry could otherwise lose access policy information.
		entries, e := os.ReadDir(s.blobs)
		if e != nil {
			return nil, e
		}
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), ".upload-") {
				return nil, errors.New("registry missing while blob data exists")
			}
		}
		if err = s.persist(s.services, s.files); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, fmt.Errorf("open registry: %w", err)
	} else {
		data, e := io.ReadAll(io.LimitReader(f, maxRegistrySize+1))
		closeErr := f.Close()
		if e != nil {
			return nil, e
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if len(data) > maxRegistrySize {
			return nil, errors.New("registry exceeds size limit")
		}
		var reg registry
		if err = decodeRegistry(data, &reg); err != nil {
			return nil, fmt.Errorf("invalid registry: %w", err)
		}
		for _, service := range reg.Services {
			if err = validateService(service); err != nil {
				return nil, fmt.Errorf("invalid registry service: %w", err)
			}
			if _, exists := s.services[service.Name]; exists {
				return nil, errors.New("duplicate service in registry")
			}
			s.services[service.Name] = copyService(service)
		}
		for _, info := range reg.Files {
			if _, err = model.FileDigest(info.Key); err != nil {
				return nil, fmt.Errorf("invalid registry file: %w", err)
			}
			if err = validatePeers(info.AllowedPeers); err != nil {
				return nil, err
			}
			if info.Size < 0 || info.Size > maxFile {
				return nil, errors.New("registry file exceeds size limit")
			}
			if _, exists := s.files[info.Key]; exists {
				return nil, errors.New("duplicate file in registry")
			}
			if info.Size > maxStore-s.total {
				return nil, ErrStoreFull
			}
			s.files[info.Key] = copyFile(info)
			s.total += info.Size
			blob, e := openRegular(s.blobPath(info.Key))
			if e != nil {
				return nil, fmt.Errorf("registry blob %s: %w", info.Key, e)
			}
			stat, e := blob.Stat()
			_ = blob.Close()
			if e != nil {
				return nil, e
			}
			if stat.Size() != info.Size {
				return nil, fmt.Errorf("%w: size of %s", ErrCorrupt, info.Key)
			}
		}
	}
	if err = s.cleanStaging(); err != nil {
		return nil, err
	}
	return s, nil
}

func privateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return errors.New("must be a real directory, not a symbolic link")
	}
	return os.Chmod(path, 0700)
}

// Close releases the lock after active operations finish. Callers must close any
// OpenFile readers. Repeated Close calls succeed.
func (s *Store) Close() error {
	s.uploads <- struct{}{}
	defer func() { <-s.uploads }()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	if err := releaseLock(filepath.Join(s.dir, lockName)); err != nil {
		return err
	}
	s.closed = true
	return nil
}
func releaseLock(path string) error {
	if err := os.Remove(filepath.Join(path, "owner")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Remove(path)
}
func (s *Store) Services() []model.Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return serviceList(s.services)
}
func (s *Store) GetService(name string) (model.Service, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.services[name]
	return copyService(v), ok
}
func (s *Store) Files() []model.FileInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fileList(s.files)
}
func (s *Store) GetFile(key string) (model.FileInfo, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.files[key]
	return copyFile(v), ok
}
func (s *Store) PutService(service model.Service) error {
	if err := validateService(service); err != nil {
		return err
	}
	service = copyService(service)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	next := make(map[string]model.Service, len(s.services)+1)
	for key, v := range s.services {
		next[key] = v
	}
	next[service.Name] = service
	if err := s.persist(next, s.files); err != nil {
		return err
	}
	s.services = next
	return nil
}
func (s *Store) DeleteService(name string) error {
	if err := model.ValidateService(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if _, ok := s.services[name]; !ok {
		return nil
	}
	next := make(map[string]model.Service, len(s.services))
	for key, v := range s.services {
		if key != name {
			next[key] = v
		}
	}
	if err := s.persist(next, s.files); err != nil {
		return err
	}
	s.services = next
	return nil
}
func (s *Store) SetFileAccess(key string, peers []string) error {
	if _, err := model.FileDigest(key); err != nil {
		return err
	}
	if err := validatePeers(peers); err != nil {
		return err
	}
	peers = append([]string{}, peers...)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	info, ok := s.files[key]
	if !ok {
		return ErrNotFound
	}
	info.AllowedPeers = peers
	next := make(map[string]model.FileInfo, len(s.files))
	for k, v := range s.files {
		next[k] = v
	}
	next[key] = info
	if err := s.persist(s.services, next); err != nil {
		return err
	}
	s.files = next
	return nil
}

// Put reads at most maxFile+1 bytes, verifies wantKey when supplied, and commits
// data before its registry entry. New files deny all peers; duplicates preserve
// their ACL. Cancellation is checked between reads and before commit. A caller
// supplying a potentially blocking Reader must arrange to unblock that Reader
// when its context is canceled (as net/http request bodies and streams can do).
func (s *Store) Put(ctx context.Context, r io.Reader, wantKey string) (model.FileInfo, error) {
	if ctx == nil || r == nil {
		return model.FileInfo{}, errors.New("context and reader are required")
	}
	if wantKey != "" {
		if _, err := model.FileDigest(wantKey); err != nil {
			return model.FileInfo{}, err
		}
	}
	select {
	case s.uploads <- struct{}{}:
	case <-ctx.Done():
		return model.FileInfo{}, ctx.Err()
	}
	defer func() { <-s.uploads }()
	s.mu.RLock()
	if s.closed {
		s.mu.RUnlock()
		return model.FileInfo{}, ErrClosed
	}
	_, known := s.files[wantKey]
	budget := s.maxStore - s.total
	s.mu.RUnlock()
	if err := ctx.Err(); err != nil {
		return model.FileInfo{}, err
	}
	var tmp *os.File
	var err error
	if !known {
		tmp, err = os.CreateTemp(s.blobs, ".upload-")
		if err != nil {
			return model.FileInfo{}, err
		}
		defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }()
	}
	hash := sha256.New()
	buf := make([]byte, 32*1024)
	var size, staged int64
	noProgress := 0
	for {
		if err = ctx.Err(); err != nil {
			return model.FileInfo{}, err
		}
		readSize := len(buf)
		if remaining := s.maxFile - size; remaining < int64(readSize) {
			readSize = int(remaining) + 1
		}
		n, readErr := r.Read(buf[:readSize])
		if n < 0 || n > readSize {
			return model.FileInfo{}, errors.New("invalid reader byte count")
		}
		if err = ctx.Err(); err != nil {
			return model.FileInfo{}, err
		}
		if n > 0 {
			noProgress = 0
			if int64(n) > s.maxFile-size {
				return model.FileInfo{}, ErrTooLarge
			}
			size += int64(n)
			_, _ = hash.Write(buf[:n])
			if tmp != nil && staged < budget {
				writeSize := int64(n)
				if writeSize > budget-staged {
					writeSize = budget - staged
				}
				wrote, e := tmp.Write(buf[:int(writeSize)])
				staged += int64(wrote)
				if e != nil {
					return model.FileInfo{}, e
				}
				if int64(wrote) != writeSize {
					return model.FileInfo{}, io.ErrShortWrite
				}
			}
		} else if readErr == nil {
			noProgress++
			if noProgress >= 100 {
				return model.FileInfo{}, io.ErrNoProgress
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return model.FileInfo{}, readErr
			}
			break
		}
	}
	key := model.FileKey(hex.EncodeToString(hash.Sum(nil)))
	if wantKey != "" && wantKey != key {
		return model.FileInfo{}, ErrHashMismatch
	}
	// Keep registry operations available during slow uploads. Only commit needs
	// exclusive registry access, and ACL changes during the read are preserved.
	s.mu.Lock()
	defer s.mu.Unlock()
	if err = ctx.Err(); err != nil {
		return model.FileInfo{}, err
	}
	if info, exists := s.files[key]; exists {
		f, e := s.openVerified(info)
		if e != nil {
			return model.FileInfo{}, e
		}
		if e = f.Close(); e != nil {
			return model.FileInfo{}, e
		}
		return copyFile(info), nil
	}
	if tmp == nil || staged != size {
		return model.FileInfo{}, ErrStoreFull
	}
	if err = ctx.Err(); err != nil {
		return model.FileInfo{}, err
	}
	if err = tmp.Sync(); err != nil {
		return model.FileInfo{}, err
	}
	if err = tmp.Close(); err != nil {
		return model.FileInfo{}, err
	}
	destination := s.blobPath(key)
	if _, err = os.Lstat(destination); err == nil {
		return model.FileInfo{}, errors.New("unregistered blob already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return model.FileInfo{}, err
	}
	if err = os.Rename(tmp.Name(), destination); err != nil {
		return model.FileInfo{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(destination)
		}
	}()
	if err = syncDirectory(s.blobs); err != nil {
		return model.FileInfo{}, err
	}
	if err = ctx.Err(); err != nil {
		return model.FileInfo{}, err
	}
	info := model.FileInfo{Key: key, Size: size, AllowedPeers: []string{}}
	next := make(map[string]model.FileInfo, len(s.files)+1)
	for k, v := range s.files {
		next[k] = v
	}
	next[key] = info
	if err = s.persist(s.services, next); err != nil {
		return model.FileInfo{}, err
	}
	committed = true
	s.files = next
	s.total += size
	return copyFile(info), nil
}

// OpenFile checks the entire file using constant memory before returning the
// verified descriptor at offset zero. It never serves unverified bytes.
func (s *Store) OpenFile(key string) (io.ReadCloser, model.FileInfo, error) {
	if _, err := model.FileDigest(key); err != nil {
		return nil, model.FileInfo{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.closed {
		return nil, model.FileInfo{}, ErrClosed
	}
	info, ok := s.files[key]
	if !ok {
		return nil, model.FileInfo{}, ErrNotFound
	}
	f, err := s.openVerified(info)
	if err != nil {
		return nil, model.FileInfo{}, err
	}
	return f, copyFile(info), nil
}
func (s *Store) openVerified(info model.FileInfo) (*os.File, error) {
	f, err := openRegular(s.blobPath(info.Key))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	fail := func(e error) (*os.File, error) { _ = f.Close(); return nil, fmt.Errorf("%w: %v", ErrCorrupt, e) }
	fi, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if fi.Size() != info.Size {
		return fail(errors.New("size mismatch"))
	}
	hash := sha256.New()
	n, err := io.Copy(hash, io.LimitReader(f, info.Size))
	if err != nil {
		return fail(err)
	}
	if n != info.Size || model.FileKey(hex.EncodeToString(hash.Sum(nil))) != info.Key {
		return fail(errors.New("SHA-256 mismatch"))
	}
	// An additional byte detects growth while verification was in progress.
	var extra [1]byte
	nExtra, err := f.Read(extra[:])
	if nExtra != 0 || !errors.Is(err, io.EOF) {
		return fail(errors.New("size changed during verification"))
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	return f, nil
}
func (s *Store) blobPath(key string) string {
	digest, _ := model.FileDigest(key) // Only called with validated registry/API keys.
	return filepath.Join(s.blobs, digest)
}

func (s *Store) persist(services map[string]model.Service, files map[string]model.FileInfo) error {
	data, err := json.Marshal(registry{Version: registryVersion, Services: serviceList(services), Files: fileList(files)})
	if err != nil {
		return err
	}
	if len(data) > maxRegistrySize {
		return errors.New("registry exceeds size limit")
	}
	path := filepath.Join(s.dir, registryName)
	if fi, e := os.Lstat(path); e == nil {
		if !fi.Mode().IsRegular() {
			return errors.New("registry must be a regular file")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	tmp, err := os.CreateTemp(s.dir, ".registry-")
	if err != nil {
		return err
	}
	defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	// Rename is the commit point. Do not report a failed mutation after it: that
	// would let callers believe an ACL change was rolled back when it was not.
	// Blob directory sync happens before this point. A directory sync error here
	// affects power-loss durability, not the committed in-process registry state.
	_ = syncDirectory(s.dir)
	return nil
}
func (s *Store) cleanStaging() error {
	entries, err := os.ReadDir(s.blobs)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if _, ok := s.files[model.FileKey(name)]; ok {
			continue
		}
		_, digestErr := model.FileDigest(model.FileKey(name))
		if !strings.HasPrefix(name, ".upload-") && digestErr != nil {
			return fmt.Errorf("unexpected blob directory entry %q", name)
		}
		path := filepath.Join(s.blobs, name)
		fi, e := os.Lstat(path)
		if e != nil {
			return e
		}
		if !fi.Mode().IsRegular() {
			return errors.New("uncommitted blob is not a regular file")
		}
		if e = os.Remove(path); e != nil {
			return e
		}
	}
	entries, err = os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".registry-") {
			continue
		}
		path := filepath.Join(s.dir, entry.Name())
		fi, e := os.Lstat(path)
		if e != nil {
			return e
		}
		if !fi.Mode().IsRegular() {
			return errors.New("registry staging entry is not a regular file")
		}
		if e = os.Remove(path); e != nil {
			return e
		}
	}
	return nil
}

// Check both pathname and opened descriptor. This blocks symlinks and ordinary
// path replacement races, but is not a sandbox against a malicious same-UID
// process capable of modifying private files after validation.
func openRegular(path string) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, errors.New("expected a regular file, not a symlink or special file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	after, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		_ = f.Close()
		return nil, errors.New("file changed while opening")
	}
	return f, nil
}
func copyService(v model.Service) model.Service {
	v.AllowedPeers = append([]string{}, v.AllowedPeers...)
	return v
}
func copyFile(v model.FileInfo) model.FileInfo {
	v.AllowedPeers = append([]string{}, v.AllowedPeers...)
	return v
}
func serviceList(m map[string]model.Service) []model.Service {
	out := make([]model.Service, 0, len(m))
	for _, v := range m {
		out = append(out, copyService(v))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
func fileList(m map[string]model.FileInfo) []model.FileInfo {
	out := make([]model.FileInfo, 0, len(m))
	for _, v := range m {
		out = append(out, copyFile(v))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}
func validateService(service model.Service) error {
	if err := model.ValidateService(service.Name); err != nil {
		return err
	}
	u, err := url.Parse(service.URL)
	if err != nil || (u.Scheme != "http") || u.Host == "" || u.Opaque != "" {
		return errors.New("service URL must be an absolute HTTP URL")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return errors.New("service URL must use a numeric loopback IP address")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return errors.New("service URL must not contain credentials, a query string, or a fragment")
	}
	return validatePeers(service.AllowedPeers)
}
func validatePeers(peers []string) error {
	if len(peers) > 4096 {
		return errors.New("too many allowed peers")
	}
	for _, p := range peers {
		if len(p) == 0 || len(p) > 1024 || strings.TrimSpace(p) != p || strings.ContainsAny(p, "\x00\r\n\t") {
			return errors.New("invalid allowed peer")
		}
	}
	return nil
}

func decodeRegistry(data []byte, reg *registry) error {
	// encoding/json otherwise silently accepts duplicate members, which is unsafe
	// for a durable access-control registry.
	tokens := json.NewDecoder(bytes.NewReader(data))
	if err := uniqueJSON(tokens, 0); err != nil {
		return err
	}
	if _, err := tokens.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing registry data")
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(reg); err != nil {
		return err
	}
	if reg.Version != registryVersion {
		return errors.New("unsupported registry version")
	}
	if reg.Services == nil || reg.Files == nil {
		return errors.New("registry services and files arrays are required")
	}
	return nil
}
func uniqueJSON(dec *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("registry nesting is too deep")
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for dec.More() {
			tok, e := dec.Token()
			if e != nil {
				return e
			}
			name, ok := tok.(string)
			if !ok {
				return errors.New("invalid object member")
			}
			if seen[name] {
				return fmt.Errorf("duplicate JSON member %q", name)
			}
			seen[name] = true
			if e = uniqueJSON(dec, depth+1); e != nil {
				return e
			}
		}
		tok, err = dec.Token()
		if err != nil {
			return err
		}
		if tok != json.Delim('}') {
			return errors.New("invalid JSON object")
		}
	case '[':
		for dec.More() {
			if err = uniqueJSON(dec, depth+1); err != nil {
				return err
			}
		}
		tok, err = dec.Token()
		if err != nil {
			return err
		}
		if tok != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}
