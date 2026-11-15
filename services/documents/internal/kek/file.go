package kek

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"

	"github.com/google/uuid"
)

// The file provider (documents design §6.2): a keyring file of
// per-hospital keys on a volume only the documents container mounts, mode
// 0400. For the pilot and development only: the service refuses it in
// production unless DOCUMENTS_ALLOW_FILE_KEYRING=pilot, and logs a warning
// at every start (config, serve). A new hospital's key is written to a
// temporary file in the same directory and renamed over the keyring, under
// an exclusive lock on <keyring>.lock with the keyring re-read inside it,
// so processes sharing the keyring never overwrite each other's keys.
//
// Format (JSON, unknown fields refused):
//
//	{"format": "mhai.documents.keyring/1",
//	 "hospitals": {"<hospital uuid>": {"current": 1,
//	   "keys": {"1": {"key": "<base64, 32 bytes>", "check": "<base64>"}}}}}
//
// check is a fixed value sealed under the key (bound to the hospital and
// version); a key whose check doesn't open is refused (the §6.2 "test
// unwrap").

const keyringFormat = "mhai.documents.keyring/1"

// maxKeyringBytes bounds the file read: thousands of hospitals fit.
const maxKeyringBytes = 4 << 20

var (
	// ErrKeyringMissing: no keyring file.
	ErrKeyringMissing = fmt.Errorf("%w: no keyring file", ErrNotReady)
	// ErrKeyringMode: not a regular file, or readable by group or others.
	ErrKeyringMode = fmt.Errorf("%w: the keyring must be a regular file readable by its owner only", ErrNotReady)
	// ErrKeyringInvalid: the file isn't a valid keyring.
	ErrKeyringInvalid = fmt.Errorf("%w: the keyring is not valid", ErrNotReady)
	// ErrKeyringPlaceholder: a key that isn't random (a repeated byte or
	// typed text).
	ErrKeyringPlaceholder = fmt.Errorf("%w: the keyring holds a placeholder key", ErrNotReady)
	// ErrKeyringCheck: a key's check value doesn't open.
	ErrKeyringCheck = fmt.Errorf("%w: a key fails its test unwrap", ErrNotReady)
	// ErrExists: InitFile found a keyring already there.
	ErrExists = errors.New("kek: a keyring is already there")
)

type fileKey struct {
	Key   string `json:"key"`
	Check string `json:"check"`
}

type fileHospital struct {
	Current int                `json:"current"`
	Keys    map[string]fileKey `json:"keys"`
}

type fileRing struct {
	Format    string                  `json:"format"`
	Hospitals map[string]fileHospital `json:"hospitals"`
}

// File is the file provider.
type File struct {
	path string

	mu   sync.Mutex
	keys map[uuid.UUID]map[int][]byte // nil until loaded
	cur  map[uuid.UUID]int
	// raw is the loaded file, rewritten with a new hospital's key.
	raw fileRing
}

// NewFile is the provider for the keyring at path. Nothing is read until
// Check (or the first Wrap or Unwrap).
func NewFile(path string) *File { return &File{path: path} }

// Name is "file".
func (f *File) Name() string { return "file" }

// Check loads the keyring if it isn't loaded and reports whether it is
// usable. A keyring fixed on disk is picked up by the next Check.
func (f *File) Check(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loadLocked()
}

func (f *File) loadLocked() error {
	if f.keys != nil {
		return nil
	}
	raw, err := readKeyring(f.path)
	if err != nil {
		return err
	}
	keys, cur, err := parseKeyring(raw)
	if err != nil {
		return err
	}
	f.keys, f.cur, f.raw = keys, cur, raw
	return nil
}

// reloadLocked reads the keyring again; on failure the loaded keys stay.
func (f *File) reloadLocked() error {
	raw, err := readKeyring(f.path)
	if err != nil {
		return err
	}
	keys, cur, err := parseKeyring(raw)
	if err != nil {
		return err
	}
	f.keys, f.cur, f.raw = keys, cur, raw
	return nil
}

// lockFile takes an exclusive advisory lock on path (made 0600 if absent)
// and returns its release.
func lockFile(path string) (func(), error) {
	fh, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(fh.Fd()), syscall.LOCK_EX); err != nil {
		_ = fh.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(fh.Fd()), syscall.LOCK_UN)
		_ = fh.Close()
	}, nil
}

// readKeyring reads and decodes the file, refusing a symlink or a mode
// that lets anyone but the owner read it.
func readKeyring(path string) (fileRing, error) {
	st, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fileRing{}, ErrKeyringMissing
	case err != nil:
		return fileRing{}, ErrKeyringInvalid
	case !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0:
		return fileRing{}, ErrKeyringMode
	case st.Size() > maxKeyringBytes:
		return fileRing{}, ErrKeyringInvalid
	}
	fh, err := os.Open(path)
	if err != nil {
		return fileRing{}, ErrKeyringInvalid
	}
	defer func() { _ = fh.Close() }()
	b, err := io.ReadAll(io.LimitReader(fh, maxKeyringBytes+1))
	if err != nil || len(b) > maxKeyringBytes {
		return fileRing{}, ErrKeyringInvalid
	}
	var r fileRing
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil || dec.More() || r.Format != keyringFormat || r.Hospitals == nil {
		return fileRing{}, ErrKeyringInvalid
	}
	return r, nil
}

// parseKeyring checks every entry: a canonical hospital id, positive
// versions, the current version present, 32 random-looking bytes per key,
// and each check value opening under its key.
func parseKeyring(r fileRing) (map[uuid.UUID]map[int][]byte, map[uuid.UUID]int, error) {
	keys := map[uuid.UUID]map[int][]byte{}
	cur := map[uuid.UUID]int{}
	for hs, h := range r.Hospitals {
		hid, err := uuid.Parse(hs)
		if err != nil || hid == uuid.Nil || hid.String() != hs || len(h.Keys) == 0 {
			return nil, nil, ErrKeyringInvalid
		}
		keys[hid] = map[int][]byte{}
		for vs, k := range h.Keys {
			v, err := strconv.Atoi(vs)
			if err != nil || v < 1 || strconv.Itoa(v) != vs {
				return nil, nil, ErrKeyringInvalid
			}
			key, err := base64.StdEncoding.DecodeString(k.Key)
			if err != nil || len(key) != DEKSize {
				return nil, nil, ErrKeyringInvalid
			}
			if placeholderKey(key) {
				return nil, nil, ErrKeyringPlaceholder
			}
			check, err := base64.StdEncoding.DecodeString(k.Check)
			if err != nil || !checkOpens(key, hid, v, check) {
				return nil, nil, ErrKeyringCheck
			}
			keys[hid][v] = key
		}
		if _, ok := keys[hid][h.Current]; !ok {
			return nil, nil, ErrKeyringInvalid
		}
		cur[hid] = h.Current
	}
	return keys, cur, nil
}

// placeholderKey reports a key that isn't random: one repeated byte, or
// all printable ASCII (typed text; 32 random bytes are that by chance about
// once in 10^13).
func placeholderKey(k []byte) bool {
	repeated := true
	for _, c := range k {
		repeated = repeated && c == k[0]
	}
	if repeated {
		return true
	}
	for _, c := range k {
		if c < 0x20 || c > 0x7e {
			return false
		}
	}
	return true
}

const checkPlain = "mhai.documents/kek-check/1"

func checkAAD(hospital uuid.UUID, version int) []byte {
	return []byte(checkPlain + "|" + hospital.String() + "|" + strconv.Itoa(version))
}

// makeCheck seals the check value under key.
func makeCheck(key []byte, hospital uuid.UUID, version int) ([]byte, error) {
	return seal(key, []byte(checkPlain), checkAAD(hospital, version))
}

func checkOpens(key []byte, hospital uuid.UUID, version int, check []byte) bool {
	p, err := open(key, check, checkAAD(hospital, version))
	return err == nil && string(p) == checkPlain
}

// seal is AES-256-GCM with a random nonce: nonce || ciphertext || tag.
func seal(key, plain, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, g.NonceSize(), g.NonceSize()+len(plain)+g.Overhead())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return g.Seal(nonce, nonce, plain, aad), nil
}

func open(key, sealed, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(sealed) < g.NonceSize()+g.Overhead() {
		return nil, ErrUnwrap
	}
	return g.Open(nil, sealed[:g.NonceSize()], sealed[g.NonceSize():], aad)
}

// Wrap wraps dek under the hospital's current KEK, making and storing one
// first if the hospital has none.
func (f *File) Wrap(_ context.Context, hospital uuid.UUID, dek, aad []byte) ([]byte, int, error) {
	if err := checkInput(hospital, aad); err != nil || len(dek) != DEKSize {
		return nil, 0, ErrInput
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.loadLocked(); err != nil {
		return nil, 0, err
	}
	v, ok := f.cur[hospital]
	if !ok {
		var err error
		if v, err = f.addHospitalLocked(hospital); err != nil {
			return nil, 0, err
		}
	}
	w, err := seal(f.keys[hospital][v], dek, wrapAAD(hospital, v, aad))
	if err != nil {
		return nil, 0, ErrNotReady
	}
	return w, v, nil
}

// Unwrap opens a wrapped DEK.
func (f *File) Unwrap(_ context.Context, hospital uuid.UUID, version int, wrapped, aad []byte) ([]byte, error) {
	if err := checkInput(hospital, aad); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.loadLocked(); err != nil {
		return nil, err
	}
	key, ok := f.keys[hospital][version]
	if !ok {
		// Another process may have added it since this one loaded: read
		// the file once more before saying there is no such key.
		if err := f.reloadLocked(); err != nil {
			return nil, err
		}
		if key, ok = f.keys[hospital][version]; !ok {
			return nil, ErrNoKey
		}
	}
	dek, err := open(key, wrapped, wrapAAD(hospital, version, aad))
	if err != nil || len(dek) != DEKSize {
		return nil, ErrUnwrap
	}
	return dek, nil
}

// addHospitalLocked makes version 1 of a hospital's KEK and writes the
// keyring before the key is used. If the write fails the key is dropped.
// It holds an exclusive lock on <keyring>.lock and re-reads the keyring
// under it, so a second process adding a key at the same moment (two
// replicas by mistake) never overwrites the other's: whoever comes second
// adopts the key already there.
func (f *File) addHospitalLocked(hospital uuid.UUID) (int, error) {
	unlock, err := lockFile(f.path + ".lock")
	if err != nil {
		return 0, ErrNotReady
	}
	defer unlock()
	raw, err := readKeyring(f.path)
	if err != nil {
		return 0, err
	}
	keys, cur, err := parseKeyring(raw)
	if err != nil {
		return 0, err
	}
	f.keys, f.cur, f.raw = keys, cur, raw
	if v, ok := f.cur[hospital]; ok {
		return v, nil
	}
	key := make([]byte, DEKSize)
	if _, err := rand.Read(key); err != nil {
		return 0, ErrNotReady
	}
	check, err := makeCheck(key, hospital, 1)
	if err != nil {
		return 0, ErrNotReady
	}
	next := fileRing{Format: keyringFormat, Hospitals: make(map[string]fileHospital, len(f.raw.Hospitals)+1)}
	for k, v := range f.raw.Hospitals {
		next.Hospitals[k] = v
	}
	next.Hospitals[hospital.String()] = fileHospital{Current: 1, Keys: map[string]fileKey{
		"1": {Key: base64.StdEncoding.EncodeToString(key), Check: base64.StdEncoding.EncodeToString(check)}}}
	if err := writeKeyring(f.path, next, true); err != nil {
		return 0, ErrNotReady
	}
	f.raw = next
	f.keys[hospital] = map[int][]byte{1: key}
	f.cur[hospital] = 1
	return 1, nil
}

// writeKeyring writes r to a temporary file in path's directory (0400,
// synced) and renames it over path (replace) or links it there only if
// nothing is (init), then syncs the directory.
func writeKeyring(path string, r fileRing, replace bool) (err error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".keyring-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(name) // after the rename or link: a no-op or the spare name
	}()
	if err = tmp.Chmod(0o400); err != nil {
		return err
	}
	if _, err = tmp.Write(b); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if replace {
		err = os.Rename(name, path)
	} else {
		// A hard link fails if path exists: init never overwrites.
		err = os.Link(name, path)
		if errors.Is(err, fs.ErrExist) {
			return ErrExists
		}
	}
	if err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// InitFile makes an empty keyring at path (mode 0400) for "documents
// keyring init". It never overwrites one: that would destroy every KEK.
func InitFile(path string) error {
	if !filepath.IsAbs(path) {
		path, _ = filepath.Abs(path)
	}
	if _, err := os.Lstat(path); err == nil {
		return ErrExists
	}
	if err := writeKeyring(path, fileRing{Format: keyringFormat, Hospitals: map[string]fileHospital{}}, false); err != nil {
		if errors.Is(err, ErrExists) {
			return err
		}
		return errors.New("kek: cannot write the keyring")
	}
	return nil
}
