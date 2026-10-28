package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// backup move (SPEC.md §6.6, internal/api SPEC §5.9, #192).

const backupMoveArgs = "<repository> --copy|--already-copied [--repository-password-file <file>] [--credentials-file <json-file>]"

// moveCredentials are the keys a --credentials-file may hold: the new
// repository's object-store credentials.
var moveCredentials = map[string]bool{
	"s3_access_key": true, "s3_secret_key": true, "s3_region": true, "b2_account_id": true, "b2_account_key": true,
}

// maxSecretFile bounds a file of secrets cgctl reads.
const maxSecretFile = 64 << 10

// buildBackupMove is `backup move`: the new repository, the mode (the panel
// copies, or the files are already there), and the new repository's
// password and credentials from files, so they never show in ps.
func buildBackupMove(a []string) (request, error) {
	body := map[string]any{}
	var repo []string
	for i := 0; i < len(a); i++ {
		name, val, hasVal := strings.Cut(a[i], "=")
		value := func() (string, error) {
			if hasVal {
				return val, nil
			}
			if i+1 >= len(a) {
				return "", fmt.Errorf("%s needs a value", name)
			}
			i++
			return a[i], nil
		}
		switch {
		case (name == "--copy" || name == "--already-copied") && !hasVal:
			if _, twice := body["mode"]; twice {
				return request{}, errors.New("give one of --copy and --already-copied")
			}
			body["mode"] = map[string]string{"--copy": "copy", "--already-copied": "moved"}[name]
		case name == "--repository-password-file":
			f, err := value()
			if err != nil {
				return request{}, err
			}
			pw, err := readSecretFile(f)
			if err != nil {
				return request{}, fmt.Errorf("--repository-password-file: %w", err)
			}
			body["password"] = strings.TrimRight(pw, "\r\n")
		case name == "--credentials-file":
			f, err := value()
			if err != nil {
				return request{}, err
			}
			raw, err := readSecretFile(f)
			if err != nil {
				return request{}, fmt.Errorf("--credentials-file: %w", err)
			}
			var creds map[string]string
			if err := json.Unmarshal([]byte(raw), &creds); err != nil {
				return request{}, fmt.Errorf("--credentials-file: not a JSON object of strings: %w", err)
			}
			for k, v := range creds {
				if !moveCredentials[k] {
					return request{}, fmt.Errorf("--credentials-file: unknown key %q (s3_access_key, s3_secret_key, s3_region, b2_account_id, b2_account_key)", k)
				}
				body[k] = v
			}
		case strings.HasPrefix(a[i], "-"):
			return request{}, fmt.Errorf("unknown flag %s", name)
		default:
			repo = append(repo, a[i])
		}
	}
	if len(repo) != 1 || body["mode"] == nil {
		return request{}, errors.New("usage: cgctl backup move " + backupMoveArgs)
	}
	body["repository"] = repo[0]
	return request{method: "POST", path: "/api/backups/repository/move", body: body}, nil
}

// readSecretFile reads a small file of secrets.
func readSecretFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxSecretFile+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxSecretFile {
		return "", errors.New("larger than 64 KiB")
	}
	if strings.TrimSpace(string(b)) == "" {
		return "", errors.New("empty")
	}
	return string(b), nil
}

// backup import (SPEC.md §6.7, internal/api SPEC §5.10, #193).

const backupImportArgs = "<snapshot> <domain> [--type <type>] [--php <version>]"

// buildBackupImport is `backup import`: a site restored from another
// server's backup, its domain typed to confirm; the type and PHP version
// for a backup without a description.
func buildBackupImport(a []string) (request, error) {
	body := map[string]any{}
	var pos []string
	for i := 0; i < len(a); i++ {
		name, val, hasVal := strings.Cut(a[i], "=")
		switch {
		case name == "--type" || name == "--php":
			if !hasVal {
				if i+1 >= len(a) {
					return request{}, fmt.Errorf("%s needs a value", name)
				}
				i++
				val = a[i]
			}
			if val == "" {
				return request{}, fmt.Errorf("%s needs a value", name)
			}
			body[map[string]string{"--type": "type", "--php": "php_version"}[name]] = val
		case strings.HasPrefix(a[i], "-"):
			return request{}, fmt.Errorf("unknown flag %s", name)
		default:
			pos = append(pos, a[i])
		}
	}
	if len(pos) != 2 {
		return request{}, errors.New("usage: cgctl backup import " + backupImportArgs)
	}
	body["snapshot"], body["confirm"] = pos[0], pos[1]
	return request{method: "POST", path: "/api/backups/foreign/restore", body: body}, nil
}
