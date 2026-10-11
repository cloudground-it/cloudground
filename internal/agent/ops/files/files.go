// Package files is the panel's file manager on one site's tree: listing,
// the text editor, uploads and downloads, folders, renames and deletes, all
// through a sitefs.Root. SPEC.md in this directory is the contract.
package files

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/cloudground-it/cloudground/internal/agent/sitefs"
	"github.com/cloudground-it/cloudground/internal/agentrpc"
	"github.com/cloudground-it/cloudground/internal/rpc"
	"github.com/cloudground-it/cloudground/internal/state"
)

// Limits (SPEC §4-§6).
const (
	// MaxEditable is the largest file the editor opens and the most it saves.
	MaxEditable = 2 << 20
	// MaxTransfer is the largest upload or download; SFTP takes the rest.
	MaxTransfer = 16 << 20
)

// newFileMode is a file the file manager creates; newDirMode a folder.
const (
	newFileMode = 0o640
	newDirMode  = 0o750
)

// Managed are the site root's own entries the panel manages (SPEC §1.1):
// the file manager may change what is inside shared/, logs/, tmp/ and src/,
// never the entries themselves.
var Managed = []string{"releases", "current", ".ssh", "shared", "logs", "tmp", "src"}

// Ops holds what the handlers use.
type Ops struct {
	// Open opens a site's tree, creating entries for the site user.
	Open func(s state.Site) (*sitefs.Root, error)
}

// Register wires this package's methods into the agent's RPC server.
func (o *Ops) Register(s *agentrpc.Server) {
	agentrpc.Handle(s, agentrpc.ListFiles, o.List)
	agentrpc.Handle(s, agentrpc.ReadFile, o.Read)
	agentrpc.Handle(s, agentrpc.WriteFile, o.Write)
	agentrpc.Handle(s, agentrpc.TransferFile, o.Transfer)
	agentrpc.Handle(s, agentrpc.ManageFile, o.Manage)
}

// open opens the site's tree as a protected view (SPEC §1.1): no entry of
// Managed, and nothing directly in a folder root owns (.ssh, releases/),
// can be created, replaced, renamed or deleted through it.
func (o *Ops) open(s state.Site) (*sitefs.Root, error) {
	r, err := o.Open(s)
	if err != nil {
		return nil, err
	}
	pr, err := r.Protect(Managed...)
	if err != nil {
		r.Close()
		return nil, err
	}
	return pr, nil
}

// rel turns an API path into a site-relative one, cleaned as if rooted at
// "/": the site root itself is ".".
func rel(p string) string {
	c := strings.TrimPrefix(path.Clean("/"+p), "/")
	if c == "" {
		return "."
	}
	return c
}

// typed classifies a filesystem error for the browser (SPEC §2). The error
// itself, path included, stays as the detail.
func typed(err error) error {
	if err == nil || agentrpc.AsError(err) != nil {
		return err
	}
	var code agentrpc.Code
	var msg string
	switch {
	case errors.Is(err, sitefs.ErrEscape):
		code, msg = agentrpc.CodeInvalid, "path escapes the site root"
	case errors.Is(err, sitefs.ErrProtected):
		code, msg = agentrpc.CodeInvalid, "this is part of the site's layout, which the panel manages; it cannot be changed in the file manager"
	case errors.Is(err, sitefs.ErrSymlink):
		code, msg = agentrpc.CodeInvalid, "the path is a symlink; refusing to follow it"
	case errors.Is(err, sitefs.ErrNotRegular):
		code, msg = agentrpc.CodeInvalid, "not a regular file"
	case errors.Is(err, syscall.ENOTDIR):
		code, msg = agentrpc.CodeInvalid, "part of the path is not a folder"
	case errors.Is(err, syscall.EISDIR):
		code, msg = agentrpc.CodeInvalid, "path is a directory"
	case errors.Is(err, syscall.ENAMETOOLONG):
		code, msg = agentrpc.CodeInvalid, "name too long"
	case errors.Is(err, fs.ErrNotExist):
		code, msg = agentrpc.CodeNotFound, "no such file or folder"
	case errors.Is(err, fs.ErrExist):
		code, msg = agentrpc.CodeConflict, "already exists"
	default:
		return err
	}
	return agentrpc.Wrap(code, msg, err)
}

// List returns a folder's entries, folders first.
func (o *Ops) List(_ context.Context, p rpc.ListFilesParams) (rpc.ListFilesResult, error) {
	r, err := o.open(p.Site)
	if err != nil {
		return rpc.ListFilesResult{}, err
	}
	defer r.Close()
	dir := rel(p.RelPath)
	infos, err := r.ReadDir(dir)
	if err != nil {
		return rpc.ListFilesResult{}, typed(err)
	}
	entries := make([]rpc.FileEntry, 0, len(infos))
	for _, fi := range infos {
		isDir := fi.IsDir()
		if fi.Mode()&fs.ModeSymlink != 0 {
			// Shown as what it leads to when that is a folder in the tree
			// (current -> releases/<id>); anything else stays a file.
			if t, err := r.Stat(path.Join(dir, fi.Name())); err == nil && t.IsDir() {
				isDir = true
			}
		}
		entries = append(entries, rpc.FileEntry{
			Name:    fi.Name(),
			IsDir:   isDir,
			Size:    fi.Size(),
			Mode:    fi.Mode().String(),
			ModTime: fi.ModTime().UTC().Format(time.RFC3339),
		})
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].IsDir != entries[j].IsDir {
			return entries[i].IsDir
		}
		return entries[i].Name < entries[j].Name
	})
	return rpc.ListFilesResult{RelPath: p.RelPath, Entries: entries}, nil
}

// head reads up to limit bytes of f and says whether that is all of it.
func head(f *os.File, limit int) ([]byte, bool, error) {
	buf := make([]byte, limit+1)
	n, err := io.ReadFull(f, buf)
	switch {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return buf[:n], true, nil
	case err != nil:
		return nil, false, err
	}
	return buf[:limit], false, nil
}

// openRegular opens rel for reading (an in-tree link followed) and refuses a
// folder.
func openRegular(r *sitefs.Root, p string) (*os.File, fs.FileInfo, error) {
	f, err := r.Open(p)
	if err != nil {
		return nil, nil, typed(err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if fi.IsDir() {
		f.Close()
		return nil, nil, agentrpc.Invalid("path is a directory")
	}
	return f, fi, nil
}

// Read returns a text file for the editor, or says why it cannot be edited.
func (o *Ops) Read(_ context.Context, p rpc.ReadFileParams) (rpc.ReadFileResult, error) {
	r, err := o.open(p.Site)
	if err != nil {
		return rpc.ReadFileResult{}, err
	}
	defer r.Close()
	f, fi, err := openRegular(r, rel(p.RelPath))
	if err != nil {
		return rpc.ReadFileResult{}, err
	}
	defer f.Close()
	b, whole, err := head(f, MaxEditable)
	if err != nil {
		return rpc.ReadFileResult{}, err
	}
	out := rpc.ReadFileResult{RelPath: p.RelPath, Size: max(fi.Size(), int64(len(b)))}
	kind := classify(b, whole)
	switch {
	case !whole:
		out.TooLarge = true
		out.Binary = kind == binary
	case kind == binary:
		out.Binary = true
	case kind == textBOM:
		out.Encoding, out.Content = encBOM, string(b[len(bom):])
	default:
		out.Encoding, out.Content = encUTF8, string(b)
	}
	return out, nil
}

// Write saves the editor's text: a new file renamed over the name, so a hard
// link is never written through. A binary file is refused, a byte order mark
// kept.
func (o *Ops) Write(_ context.Context, p rpc.WriteFileParams) (map[string]string, error) {
	if len(p.Content) > MaxEditable {
		return nil, agentrpc.Invalid("file too large to edit here")
	}
	r, err := o.open(p.Site)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	name := rel(p.RelPath)
	if name == "." {
		return nil, agentrpc.Invalid("path is a directory")
	}
	data := []byte(p.Content)
	switch f, _, err := openRegular(r, name); {
	case err == nil:
		b, whole, rerr := head(f, MaxEditable)
		f.Close()
		if rerr != nil {
			return nil, rerr
		}
		switch classify(b, whole) {
		case binary:
			return nil, agentrpc.Conflict("this file is binary and cannot be edited as text; download it instead")
		case textBOM:
			if !strings.HasPrefix(p.Content, string(bom)) {
				data = append(append([]byte{}, bom...), data...)
			}
		}
	case agentrpc.CodeOf(err) == agentrpc.CodeNotFound:
		// A new file.
	default:
		return nil, err
	}
	if err := r.WriteFile(name, data, newFileMode); err != nil {
		return nil, typed(err)
	}
	return map[string]string{"written": p.RelPath}, nil
}

// Transfer uploads or downloads a file of at most MaxTransfer bytes.
func (o *Ops) Transfer(_ context.Context, p rpc.TransferFileParams) (rpc.TransferFileResult, error) {
	name := rel(p.RelPath)
	if name == "." {
		return rpc.TransferFileResult{}, agentrpc.Invalid("file path required")
	}
	if p.Upload && len(p.Data) > MaxTransfer {
		return rpc.TransferFileResult{}, agentrpc.Invalid("upload exceeds 16 MB; use SFTP for larger files")
	}
	r, err := o.open(p.Site)
	if err != nil {
		return rpc.TransferFileResult{}, err
	}
	defer r.Close()
	if p.Upload {
		if fi, err := r.Stat(name); err == nil && fi.IsDir() {
			return rpc.TransferFileResult{}, agentrpc.Invalid("destination is a directory")
		}
		if err := r.WriteFile(name, p.Data, newFileMode); err != nil {
			return rpc.TransferFileResult{}, typed(err)
		}
		return rpc.TransferFileResult{Name: path.Base(name), Size: int64(len(p.Data))}, nil
	}
	f, err := r.Open(name)
	if err != nil {
		return rpc.TransferFileResult{}, typed(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return rpc.TransferFileResult{}, err
	}
	if !fi.Mode().IsRegular() {
		return rpc.TransferFileResult{}, agentrpc.Invalid("path is not a regular file")
	}
	tooBig := agentrpc.Invalid("download exceeds 16 MB; use SFTP for larger files")
	if fi.Size() > MaxTransfer {
		return rpc.TransferFileResult{}, tooBig
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxTransfer+1))
	if err != nil {
		return rpc.TransferFileResult{}, err
	}
	if len(data) > MaxTransfer {
		return rpc.TransferFileResult{}, tooBig
	}
	return rpc.TransferFileResult{Name: path.Base(name), Data: data, Size: int64(len(data))}, nil
}

// Manage makes a folder, renames or deletes, acting on the final name itself.
func (o *Ops) Manage(_ context.Context, p rpc.ManageFileParams) (map[string]string, error) {
	name := rel(p.RelPath)
	if name == "." {
		return nil, agentrpc.Invalid("site root cannot be modified")
	}
	var dest string
	switch p.Operation {
	case "mkdir", "delete":
	case "rename":
		if strings.TrimSpace(p.DestPath) == "" {
			return nil, agentrpc.Invalid("destination path required")
		}
		if dest = rel(p.DestPath); dest == "." {
			return nil, agentrpc.Invalid("site root cannot be modified")
		}
	default:
		return nil, agentrpc.Invalid("unsupported file operation %q", p.Operation)
	}
	r, err := o.open(p.Site)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	switch p.Operation {
	case "mkdir":
		if err := r.Mkdir(name, newDirMode); err != nil {
			return nil, typed(err)
		}
		return map[string]string{"created": p.RelPath}, nil
	case "rename":
		err := r.Rename(name, dest, sitefs.NoReplace)
		switch {
		case errors.Is(err, fs.ErrExist):
			return nil, agentrpc.Conflict("destination already exists")
		case errors.Is(err, syscall.EINVAL):
			return nil, agentrpc.Invalid("a folder cannot be moved into itself")
		case err != nil:
			return nil, typed(err)
		}
		return map[string]string{"renamed": p.DestPath}, nil
	default:
		if _, err := r.Lstat(name); err != nil {
			return nil, typed(err)
		}
		if err := r.RemoveAll(name); err != nil {
			return nil, typed(err)
		}
		return map[string]string{"deleted": p.RelPath}, nil
	}
}
