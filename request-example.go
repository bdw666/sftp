package sftp

// This serves as an example of how to implement the request server handler as
// well as a dummy backend for testing. It implements an in-memory backend that
// works as a very simple filesystem with simple flat key-value lookup system.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// InMemHandler returns a Hanlders object with the test handlers.
func InMemHandler() Handlers {
	root := &root{
		files: make(map[string]*memFile),
	}
	root.memFile = newMemFile("/", true)
	return Handlers{root, root, root, root}
}

// Example Handlers
func (fs *root) Fileread(r *Request) (io.ReaderAt, error) {
	if fs.mockErr != nil {
		return nil, fs.mockErr
	}
	_ = r.WithContext(r.Context()) // initialize context for deadlock testing
	fs.filesLock.Lock()
	defer fs.filesLock.Unlock()
	file, err := fs.fetch(r.Filepath)
	if err != nil {
		return nil, err
	}
	return file.ReaderAt()
}

func (fs *root) getFileForWrite(r *Request) (*memFile, error) {
	if fs.mockErr != nil {
		return nil, fs.mockErr
	}
	_ = r.WithContext(r.Context()) // initialize context for deadlock testing
	fs.filesLock.Lock()
	defer fs.filesLock.Unlock()
	file, err := fs.fetch(r.Filepath)
	if err == os.ErrNotExist {
		dir, err := fs.fetch(path.Dir(r.Filepath))
		if err != nil {
			return nil, err
		}
		if !dir.isdir {
			return nil, os.ErrInvalid
		}
		file = newMemFile(r.Filepath, false)
		fs.files[r.Filepath] = file
	}
	return file, nil
}

func (fs *root) Filewrite(r *Request) (io.WriterAt, error) {
	file, err := fs.getFileForWrite(r)
	if err != nil {
		return nil, err
	}
	return file.WriterAt()
}

func (fs *root) OpenFile(r *Request) (WriterAtReaderAt, error) {
	return fs.getFileForWrite(r)
}

func (fs *root) Filecmd(r *Request) error {
	if fs.mockErr != nil {
		return fs.mockErr
	}
	_ = r.WithContext(r.Context()) // initialize context for deadlock testing
	fs.filesLock.Lock()
	defer fs.filesLock.Unlock()
	switch r.Method {
	case "Setstat":
		file, err := fs.fetch(r.Filepath)
		if err != nil {
			return err
		}
		if r.AttrFlags().Size {
			return file.Truncate(int64(r.Attributes().Size))
		}
		return nil
	case "Rename":
		file, err := fs.fetch(r.Filepath)
		if err != nil {
			return err
		}
		if _, ok := fs.files[r.Target]; ok {
			return &os.LinkError{Op: "rename", Old: r.Filepath, New: r.Target,
				Err: fmt.Errorf("dest file exists")}
		}
		file.name = r.Target
		fs.files[r.Target] = file
		delete(fs.files, r.Filepath)

		if file.IsDir() {
			for path, file := range fs.files {
				if strings.HasPrefix(path, r.Filepath+"/") {
					file.name = r.Target + path[len(r.Filepath):]
					fs.files[r.Target+path[len(r.Filepath):]] = file
					delete(fs.files, path)
				}
			}
		}
	case "Rmdir", "Remove":
		file, err := fs.fetch(path.Dir(r.Filepath))
		if err != nil {
			return err
		}

		if file.IsDir() {
			for path := range fs.files {
				if strings.HasPrefix(path, r.Filepath+"/") {
					return &os.PathError{
						Op:   "remove",
						Path: r.Filepath + "/",
						Err:  fmt.Errorf("directory is not empty"),
					}
				}
			}
		}

		delete(fs.files, r.Filepath)

	case "Mkdir":
		_, err := fs.fetch(path.Dir(r.Filepath))
		if err != nil {
			return err
		}
		fs.files[r.Filepath] = newMemFile(r.Filepath, true)
	case "Link":
		file, err := fs.fetch(r.Filepath)
		if err != nil {
			return err
		}
		if file.IsDir() {
			return fmt.Errorf("hard link not allowed for directory")
		}
		fs.files[r.Target] = file
	case "Symlink":
		_, err := fs.fetch(r.Filepath)
		if err != nil {
			return err
		}
		link := newMemFile(r.Target, false)
		link.symlink = r.Filepath
		fs.files[r.Target] = link
	}
	return nil
}

type listerat []os.FileInfo

// Modeled after strings.Reader's ReadAt() implementation
func (f listerat) ListAt(ls []os.FileInfo, offset int64) (int, error) {
	var n int
	if offset >= int64(len(f)) {
		return 0, io.EOF
	}
	n = copy(ls, f[offset:])
	if n < len(ls) {
		return n, io.EOF
	}
	return n, nil
}

func (fs *root) Filelist(r *Request) (ListerAt, error) {
	if fs.mockErr != nil {
		return nil, fs.mockErr
	}
	_ = r.WithContext(r.Context()) // initialize context for deadlock testing
	fs.filesLock.Lock()
	defer fs.filesLock.Unlock()

	file, err := fs.fetch(r.Filepath)
	if err != nil {
		return nil, err
	}

	switch r.Method {
	case "List":
		if !file.IsDir() {
			return nil, syscall.ENOTDIR
		}
		orderedNames := []string{}
		for fn := range fs.files {
			if path.Dir(fn) == r.Filepath {
				orderedNames = append(orderedNames, fn)
			}
		}
		sort.Strings(orderedNames)
		list := make([]os.FileInfo, len(orderedNames))
		for i, fn := range orderedNames {
			list[i] = fs.files[fn]
		}
		return listerat(list), nil
	case "Stat":
		return listerat([]os.FileInfo{file}), nil
	case "Readlink":
		return listerat([]os.FileInfo{file}), nil
	}
	return nil, nil
}

// implements LstatFileLister interface
func (fs *root) Lstat(r *Request) (ListerAt, error) {
	if fs.mockErr != nil {
		return nil, fs.mockErr
	}
	_ = r.WithContext(r.Context()) // initialize context for deadlock testing
	fs.filesLock.Lock()
	defer fs.filesLock.Unlock()

	file, err := fs.lfetch(r.Filepath)
	if err != nil {
		return nil, err
	}
	return listerat([]os.FileInfo{file}), nil
}

// In memory file-system-y thing that the Hanlders live on
type root struct {
	*memFile
	files     map[string]*memFile
	filesLock sync.Mutex
	mockErr   error
}

// Set a mocked error that the next handler call will return.
// Set to nil to reset for no error.
func (fs *root) returnErr(err error) {
	fs.mockErr = err
}

func (fs *root) lfetch(path string) (*memFile, error) {
	if path == "/" {
		return fs.memFile, nil
	}

	file, ok := fs.files[path]
	if file == nil {
		if ok {
			delete(fs.files, path)
		}

		return nil, os.ErrNotExist
	}

	return file, nil
}

func (fs *root) fetch(path string) (*memFile, error) {
	file, err := fs.lfetch(path)
	if err != nil {
		return nil, err
	}

	for file.symlink != "" {
		file, err = fs.lfetch(file.symlink)
		if err != nil {
			return nil, err
		}
	}

	return file, nil
}

// Implements os.FileInfo, Reader and Writer interfaces.
// These are the 3 interfaces necessary for the Handlers.
// Implements the optional interface TransferError.
type memFile struct {
	name          string
	modtime       time.Time
	symlink       string
	isdir         bool
	transferError error

	mu      sync.RWMutex
	content []byte
}

// factory to make sure modtime is set
func newMemFile(name string, isdir bool) *memFile {
	return &memFile{
		name:    name,
		modtime: time.Now(),
		isdir:   isdir,
	}
}

// These are helper functions, they must be called while holding the memFile.mu mutex
func (f *memFile) size() int64  { return int64(len(f.content)) }
func (f *memFile) grow(n int64) { f.content = append(f.content, make([]byte, n)...) }

// Have memFile fulfill os.FileInfo interface
func (f *memFile) Name() string { return path.Base(f.name) }
func (f *memFile) Size() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.size()
}
func (f *memFile) Mode() os.FileMode {
	if f.isdir {
		return os.FileMode(0755) | os.ModeDir
	}
	if f.symlink != "" {
		return os.FileMode(0777) | os.ModeSymlink
	}
	return os.FileMode(0644)
}
func (f *memFile) ModTime() time.Time { return f.modtime }
func (f *memFile) IsDir() bool        { return f.isdir }
func (f *memFile) Sys() interface{} {
	return fakeFileInfoSys()
}

// Read/Write
func (f *memFile) ReaderAt() (io.ReaderAt, error) {
	if f.isdir {
		return nil, os.ErrInvalid
	}

	return f, nil
}
func (f *memFile) ReadAt(b []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if off < 0 {
		return 0, errors.New("memFile.ReadAt: negative offset")
	}

	if off >= f.size() {
		return 0, io.EOF
	}

	n := copy(b, f.content[off:])
	if n < len(b) {
		return n, io.EOF
	}

	return n, nil
}

func (f *memFile) WriterAt() (io.WriterAt, error) {
	if f.isdir {
		return nil, os.ErrInvalid
	}

	return f, nil
}
func (f *memFile) WriteAt(b []byte, off int64) (int, error) {
	// fmt.Println(string(p), off)
	// mimic write delays, should be optional
	time.Sleep(time.Microsecond * time.Duration(len(b)))

	f.mu.Lock()
	defer f.mu.Unlock()

	grow := int64(len(b)) + off - f.size()
	if grow > 0 {
		f.grow(grow)
	}

	return copy(f.content[off:], b), nil
}

func (f *memFile) Truncate(size int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	grow := size - f.size()
	if grow <= 0 {
		f.content = f.content[:size]
	} else {
		f.grow(grow)
	}

	return nil
}

func (f *memFile) TransferError(err error) {
	f.transferError = err
}
