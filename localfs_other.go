//go:build !windows
// +build !windows

package desync

import (
	"io"
	"os"
	"path"
	"path/filepath"
	"syscall"
	"time"

	"github.com/pkg/errors"
	"github.com/pkg/xattr"
)

// NewLocalFS initializes a new instance of a local filesystem that
// can be used for tar/untar operations.
func NewLocalFS(root string, opts LocalFSOptions) *LocalFS {
	return &LocalFS{
		Root:    root,
		opts:    opts,
		entries: make(chan walkEntry),
	}
}

func (fs *LocalFS) SetDirPermissions(n NodeDirectory) error {
	dst := filepath.Join(fs.Root, n.Name)

	// The dir exists now, fix the UID/GID if needed
	if !fs.opts.NoSameOwner {
		if err := os.Chown(dst, n.UID, n.GID); err != nil {
			return err
		}

		if n.Xattrs != nil {
			for key, value := range n.Xattrs {
				if err := xattr.LSet(dst, key, []byte(value)); err != nil {
					return err
				}
			}
		}
	}
	if !fs.opts.NoSamePermissions {
		if err := syscall.Chmod(dst, FilemodeToStatMode(n.Mode)); err != nil {
			return err
		}
	}

	return nil
}

func (fs *LocalFS) SetFilePermissions(n NodeFile) error {
	dst := filepath.Join(fs.Root, n.Name)

	if !fs.opts.NoSameOwner {
		if err := os.Chown(dst, n.UID, n.GID); err != nil {
			return err
		}

		if n.Xattrs != nil {
			for key, value := range n.Xattrs {
				if err := xattr.LSet(dst, key, []byte(value)); err != nil {
					return err
				}
			}
		}
	}
	if !fs.opts.NoSamePermissions {
		if err := syscall.Chmod(dst, FilemodeToStatMode(n.Mode)); err != nil {
			return err
		}
	}

	return nil
}

func (fs *LocalFS) SetSymlinkPermissions(n NodeSymlink) error {
	dst := filepath.Join(fs.Root, n.Name)

	// TODO: On Linux, the permissions of the link don't matter so we don't
	// set them here. But they do matter somewhat on Mac, so should probably
	// add some Mac-specific logic for that here.
	// fchmodat() with flag AT_SYMLINK_NOFOLLOW
	if !fs.opts.NoSameOwner {
		if err := os.Lchown(dst, n.UID, n.GID); err != nil {
			return err
		}

		if n.Xattrs != nil {
			for key, value := range n.Xattrs {
				if err := xattr.LSet(dst, key, []byte(value)); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (fs *LocalFS) CreateDevice(n NodeDevice) error {
	dst := filepath.Join(fs.Root, n.Name)

	if err := syscall.Unlink(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := syscall.Mknod(dst, FilemodeToStatMode(n.Mode)|0666, int(mkdev(n.Major, n.Minor))); err != nil {
		return errors.Wrapf(err, "mknod %s", dst)
	}
	if !fs.opts.NoSameOwner {
		if err := os.Chown(dst, n.UID, n.GID); err != nil {
			return err
		}

		if n.Xattrs != nil {
			for key, value := range n.Xattrs {
				if err := xattr.LSet(dst, key, []byte(value)); err != nil {
					return err
				}
			}
		}
	}
	if !fs.opts.NoSamePermissions {
		if err := syscall.Chmod(dst, FilemodeToStatMode(n.Mode)); err != nil {
			return errors.Wrapf(err, "chmod %s", dst)
		}
	}
	if n.MTime == time.Unix(0, 0) {
		return nil
	}
	return os.Chtimes(dst, n.MTime, n.MTime)
}

func mkdev(major, minor uint64) uint64 {
	dev := (major & 0x00000fff) << 8
	dev |= (major & 0xfffff000) << 32
	dev |= (minor & 0x000000ff) << 0
	dev |= (minor & 0xffffff00) << 12
	return dev
}

// Next returns the next filesystem entry or io.EOF when done. The caller is responsible
// for closing the returned File object.
func (fs *LocalFS) Next() (*File, error) {
	fs.once.Do(func() {
		fs.initForReading()
	})

	entry, ok := <-fs.entries
	if !ok {
		return nil, fs.sErr
	}
	if entry.err != nil {
		return nil, entry.err
	}

	var (
		uid, gid     int
		major, minor uint64
	)
	switch sys := entry.info.Sys().(type) {
	case *syscall.Stat_t:
		uid = int(sys.Uid)
		gid = int(sys.Gid)
		major = uint64((sys.Rdev >> 8) & 0xfff)
		minor = (uint64(sys.Rdev) % 256) | ((uint64(sys.Rdev) & 0xfff00000) >> 12)
	default:
		panic("unsupported platform")
	}

	// Extract the Xattrs if any
	xa := make(map[string]string)
	keys, err := xattr.LList(entry.path)
	if err != nil {
		return nil, err
	}
	for _, key := range keys {
		value, err := xattr.LGet(entry.path, key)
		if err != nil {
			return nil, err
		}
		xa[key] = string(value)
	}

	// If it's a file, open it and return a ReadCloser
	var r io.ReadCloser
	if entry.info.Mode().IsRegular() {
		data, err := os.Open(entry.path)
		if err != nil {
			return nil, err
		}
		r = data
	}

	// If this is a symlink we need to get the link target
	var linkTarget string
	if entry.info.Mode()&os.ModeSymlink != 0 {
		linkTarget, err = os.Readlink(entry.path)
		if err != nil {
			return nil, err
		}
	}

	mtime := entry.info.ModTime()
	if fs.opts.NoTime {
		mtime = time.Unix(0, 0)
	}

	f := &File{
		Name:       entry.info.Name(),
		Path:       path.Clean(entry.path),
		Mode:       entry.info.Mode(),
		ModTime:    mtime,
		Size:       uint64(entry.info.Size()),
		LinkTarget: linkTarget,
		Uid:        uid,
		Gid:        gid,
		Xattrs:     xa,
		DevMajor:   major,
		DevMinor:   minor,
		Data:       r,
	}

	return f, nil
}

func (fs *LocalFS) initForReading() {
	if fs.opts.OneFileSystem {
		info, err := os.Lstat(fs.Root)
		if err == nil {
			st, ok := info.Sys().(*syscall.Stat_t)
			if ok {
				// Dev (and Rdev) elements of syscall.Stat_t are uint64 on Linux,
				// but int32 on MacOS. Cast it to uint64 everywhere.
				fs.dev = uint64(st.Dev)
			}
		}
	}
	fs.startSerializer()
}

func (fs *LocalFS) startSerializer() {
	go func() {
		err := filepath.Walk(fs.Root, func(path string, info os.FileInfo, err error) error {
			if fs.dev != 0 && info.IsDir() {
				// one-file-system is set, skip other filesystems
				st, ok := info.Sys().(*syscall.Stat_t)
				if ok && uint64(st.Dev) != fs.dev {
					// this directory is another filesystem, so skip the whole sub-tree
					return filepath.SkipDir
				}
			}
			fs.entries <- walkEntry{path, info, err}
			return nil
		})
		fs.sErr = err
		if err == nil {
			fs.sErr = io.EOF
		}
		close(fs.entries)
	}()
}
