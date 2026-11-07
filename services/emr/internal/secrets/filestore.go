package secrets

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"

	"github.com/Measured-Health/mhai-multiagent/services/gokit/seal"
)

// FileStore keeps secrets under a directory: <dir>/<path>/<version>, each
// version a 0400 file written whole (a temp file, then rename), each
// directory 0700 and owned by this process's user.
type FileStore struct {
	dir string
	env string
	// Now dates versions; nil is time.Now.
	Now func() time.Time
}

// NewFileStore opens the store at dir (absolute, existing, 0700, owned by
// this user) for appEnv, which must be development or staging.
func NewFileStore(dir, appEnv string) (*FileStore, error) {
	if appEnv != "development" && appEnv != "staging" {
		return nil, ErrRefused
	}
	if !filepath.IsAbs(dir) {
		return nil, ErrStore
	}
	if err := checkDir(dir); err != nil {
		return nil, err
	}
	return &FileStore{dir: filepath.Clean(dir), env: appEnv}, nil
}

// checkDir: a real directory (not a link), mode 0700, owned by this user.
func checkDir(dir string) error {
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm() != 0o700 {
		return ErrStore
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); !ok || int(sys.Uid) != os.Getuid() {
		return ErrStore
	}
	return nil
}

func (s *FileStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// at is p's directory; p must be valid and of this store's environment.
func (s *FileStore) at(p Path) (string, error) {
	if !p.valid() || p.Env != s.env {
		return "", ErrPath
	}
	return filepath.Join(s.dir, filepath.FromSlash(p.String())), nil
}

// mkdirs makes every directory from the store's root down to d, 0700.
func (s *FileStore) mkdirs(d string) error {
	rel, err := filepath.Rel(s.dir, d)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ErrPath
	}
	cur := s.dir
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		if err := os.Mkdir(cur, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
			return ErrStore
		}
		if err := checkDir(cur); err != nil {
			return err
		}
	}
	return nil
}

func newVersion(t time.Time) (string, error) {
	var b [4]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return "", ErrStore
	}
	return fmt.Sprintf("v%019d-%s", t.UnixNano(), hex.EncodeToString(b[:])), nil
}

// Put writes v as a new version of p, then drops versions older than Keep.
func (s *FileStore) Put(ctx context.Context, p Path, v seal.Secret) (string, error) {
	d, err := s.at(p)
	if err != nil {
		return "", err
	}
	if v.Len() == 0 {
		return "", ErrStore
	}
	if err := s.mkdirs(d); err != nil {
		return "", err
	}
	ver, err := newVersion(s.now())
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(d, ".tmp-")
	if err != nil {
		return "", ErrStore
	}
	tmp := f.Name()
	_, werr := f.Write(v.Bytes())
	serr := f.Sync()
	cerr := f.Close()
	if werr != nil || serr != nil || cerr != nil || os.Chmod(tmp, 0o400) != nil ||
		os.Rename(tmp, filepath.Join(d, ver)) != nil {
		_ = os.Remove(tmp)
		return "", ErrStore
	}
	_ = s.DeleteVersionsBefore(ctx, p, Keep)
	return ver, nil
}

// versions are p's versions, oldest first.
func (s *FileStore) versions(d string) ([]string, error) {
	es, err := os.ReadDir(d)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, ErrStore
	}
	var out []string
	for _, e := range es {
		if versionK.MatchString(e.Name()) && e.Type().IsRegular() {
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	return out, nil
}

// Get reads p's current version.
func (s *FileStore) Get(ctx context.Context, p Path) (seal.Secret, string, error) {
	d, err := s.at(p)
	if err != nil {
		return seal.Secret{}, "", err
	}
	vs, err := s.versions(d)
	if err != nil {
		return seal.Secret{}, "", err
	}
	if len(vs) == 0 {
		return seal.Secret{}, "", ErrNotFound
	}
	cur := vs[len(vs)-1]
	v, err := s.GetVersion(ctx, p, cur)
	return v, cur, err
}

// GetVersion reads one version of p.
func (s *FileStore) GetVersion(_ context.Context, p Path, version string) (seal.Secret, error) {
	d, err := s.at(p)
	if err != nil {
		return seal.Secret{}, err
	}
	if !versionK.MatchString(version) {
		return seal.Secret{}, ErrPath
	}
	f, err := os.OpenFile(filepath.Join(d, version), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return seal.Secret{}, ErrNotFound
	}
	if err != nil {
		return seal.Secret{}, ErrStore
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, seal.MaxValue+1))
	if err != nil || len(b) == 0 || len(b) > seal.MaxValue {
		clear(b)
		return seal.Secret{}, ErrStore
	}
	sec := seal.NewSecret(b)
	clear(b)
	return sec, nil
}

// DeleteVersion drops one version of p; a missing one is fine.
func (s *FileStore) DeleteVersion(_ context.Context, p Path, version string) error {
	d, err := s.at(p)
	if err != nil {
		return err
	}
	if !versionK.MatchString(version) {
		return ErrPath
	}
	if err := os.Remove(filepath.Join(d, version)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return ErrStore
	}
	return nil
}

// DeleteAll removes the connection's directory: every field, every version.
func (s *FileStore) DeleteAll(_ context.Context, hospital, connection uuid.UUID) error {
	if hospital == uuid.Nil || connection == uuid.Nil {
		return ErrPath
	}
	d := filepath.Join(s.dir, "mhai", s.env, "emr", hospital.String(), connection.String())
	if err := os.RemoveAll(d); err != nil {
		return ErrStore
	}
	return nil
}

// DeleteVersionsBefore drops p's versions, other than the current one,
// written more than keep ago.
func (s *FileStore) DeleteVersionsBefore(ctx context.Context, p Path, keep time.Duration) error {
	d, err := s.at(p)
	if err != nil {
		return err
	}
	vs, err := s.versions(d)
	if err != nil || len(vs) < 2 {
		return err
	}
	cutoff, _ := newVersion(s.now().Add(-keep))
	for _, v := range vs[:len(vs)-1] {
		if v < cutoff {
			if err := s.DeleteVersion(ctx, p, v); err != nil {
				return err
			}
		}
	}
	return nil
}

// Ready checks the root is still the directory it was.
func (s *FileStore) Ready(context.Context) error { return checkDir(s.dir) }
