package rpc

import (
	"errors"
	"fmt"
	"strings"

	"github.com/cloudground-it/cloudground/internal/state"
)

// The file manager works inside one site's tree: every RelPath is relative
// to the site's root, and the agent confines it there.

// ListFilesParams lists a directory.
type ListFilesParams struct {
	Site    state.Site `json:"site"`
	RelPath string     `json:"rel_path"`
}

// Sites lists the site browsed.
func (p *ListFilesParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// ListFilesResult is the directory's entries.
type ListFilesResult struct {
	RelPath string      `json:"rel_path"`
	Entries []FileEntry `json:"entries"`
}

// FileEntry is one entry of a directory. Mode is in ls's notation and
// ModTime in RFC 3339.
type FileEntry struct {
	Name    string `json:"name"`
	IsDir   bool   `json:"is_dir"`
	Size    int64  `json:"size"`
	Mode    string `json:"mode"`
	ModTime string `json:"mod_time"`
}

// ReadFileParams opens a file in the editor.
type ReadFileParams struct {
	Site    state.Site `json:"site"`
	RelPath string     `json:"rel_path"`
}

// Sites lists the site read.
func (p *ReadFileParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// ReadFileResult is a file for the editor, or why it is not one: with
// Binary or TooLarge set, Content is empty and the file can only be
// downloaded.
type ReadFileResult struct {
	RelPath string `json:"rel_path"`
	Content string `json:"content"`
	Size    int64  `json:"size"`
	// Binary: a NUL byte in the first 8 KiB, or bytes that are not UTF-8.
	Binary bool `json:"binary"`
	// TooLarge: above the editor's limit (ops/files.MaxEditable).
	TooLarge bool `json:"too_large"`
	// Encoding is "utf-8", or "utf-8-bom" for a file that starts with a
	// byte order mark. The mark is not in Content, and a write keeps it.
	Encoding string `json:"encoding,omitempty"`
}

// WriteFileParams saves the editor's text.
type WriteFileParams struct {
	Site    state.Site `json:"site"`
	RelPath string     `json:"rel_path"`
	Content string     `json:"content"`
}

// Sites lists the site written.
func (p *WriteFileParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// TransferFileParams uploads Data to RelPath (Upload) or downloads RelPath,
// within the agent's size bound.
type TransferFileParams struct {
	Site    state.Site `json:"site"`
	RelPath string     `json:"rel_path"`
	Data    []byte     `json:"data,omitempty"`
	Upload  bool       `json:"upload"`
}

// Sites lists the site transferred to or from.
func (p *TransferFileParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// TransferFileResult is the file's name and size, and its bytes for a
// download.
type TransferFileResult struct {
	Name string `json:"name"`
	Data []byte `json:"data,omitempty"`
	Size int64  `json:"size"`
}

// ManageFileParams makes a directory, deletes, or renames RelPath to
// DestPath.
type ManageFileParams struct {
	Site      state.Site `json:"site"`
	Operation string     `json:"operation"`
	RelPath   string     `json:"rel_path"`
	DestPath  string     `json:"dest_path,omitempty"`
}

// Sites lists the site changed.
func (p *ManageFileParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate requires a known operation, and a destination for a rename.
func (p *ManageFileParams) Validate() error {
	switch p.Operation {
	case "mkdir", "delete":
		return nil
	case "rename":
		if strings.TrimSpace(p.DestPath) == "" {
			return errors.New("destination path required")
		}
		return nil
	}
	return fmt.Errorf("unsupported file operation %q", p.Operation)
}

// SFTPParams turns the site user's SFTP access on (with Password) or off.
type SFTPParams struct {
	Site     state.Site `json:"site"`
	Password string     `json:"password"`
	Enable   bool       `json:"enable"`
}

// Sites lists the site whose access changes.
func (p *SFTPParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// SSHKeyParams lists the site user's keys, adds PublicKey, or deletes the
// key with Fingerprint.
type SSHKeyParams struct {
	Site        state.Site `json:"site"`
	Action      string     `json:"action"`
	PublicKey   string     `json:"public_key,omitempty"`
	Fingerprint string     `json:"fingerprint,omitempty"`
	// Access is the type of a key to add: "sftp" (also "") or "shell"
	// (R-4.5).
	Access string `json:"access,omitempty"`
}

// Sites lists the site whose keys change.
func (p *SSHKeyParams) Sites() []*state.Site { return []*state.Site{&p.Site} }

// Validate requires a known action.
func (p *SSHKeyParams) Validate() error {
	switch p.Action {
	case "list", "add", "delete":
	default:
		return fmt.Errorf("unsupported SSH key action %q", p.Action)
	}
	switch p.Access {
	case "", "sftp", "shell":
		return nil
	}
	return fmt.Errorf("unsupported SSH key access %q", p.Access)
}

// SSHKeysResult is the site user's keys after the action, and the port
// sshd listens on, for the connection details.
type SSHKeysResult struct {
	Keys []SSHKey `json:"keys"`
	Port int      `json:"port"`
	// ShellUser is the site's shell login (R-4.5), whether or not the
	// shell is on.
	ShellUser string `json:"shell_user,omitempty"`
}

// SSHKey is one authorized key.
type SSHKey struct {
	Type        string `json:"type"`
	Fingerprint string `json:"fingerprint"`
	Label       string `json:"label,omitempty"`
	// Access is "sftp" or "shell" (R-4.5).
	Access string `json:"access"`
}

// ShellParams turns a site's SSH shell on or off (R-4.5).
type ShellParams struct {
	Site   state.Site `json:"site"`
	Enable bool       `json:"enable"`
}

// Sites lists the site whose shell changes.
func (p *ShellParams) Sites() []*state.Site { return []*state.Site{&p.Site} }
