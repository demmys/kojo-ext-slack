// Package uploadpath centralizes the on-disk location and filename
// sanitization for attachments so the Slack file-download path writes them
// exactly where, and named exactly how, kojo's own upload handler would.
package uploadpath

import (
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// EnvDir is the environment variable kojo sets for an extension process to
// its upload directory.
const EnvDir = "KOJO_UPLOAD_DIR"

// Dir returns the directory where attachments are stored. kojo accepts an
// ext-chat attachment only if its path is inside its upload directory, which
// it passes as KOJO_UPLOAD_DIR. Without the variable (the binary was not
// started by kojo) this falls back to kojo's default location,
// {os.TempDir()}/kojo/upload. Callers create it on demand.
func Dir() string {
	if dir := os.Getenv(EnvDir); dir != "" {
		return dir
	}
	return filepath.Join(os.TempDir(), "kojo", "upload")
}

// SanitizeName reduces an arbitrary user-supplied filename to a single
// benign path component. The unified policy is:
//
//  1. filepath.Base strips any directory components.
//  2. "/", "\\" and NUL are replaced with "_".
//  3. Any other control character (unicode.IsControl) is replaced with
//     "_" — this blocks newline/escape-sequence injection through crafted
//     names that later get echoed into prompts or logs.
//  4. If the result would be "", "." or ".." (values that are unsafe as a
//     bare path element), it is replaced with "_".
//
// The output is safe to use directly as a filename component under Dir()
// and safe to join with a prefix ({unixnano}_{name}).
func SanitizeName(name string) string {
	name = filepath.Base(name) // strip any directory components
	name = strings.Map(func(r rune) rune {
		switch {
		case r == '/', r == '\\', r == '\x00':
			return '_'
		case unicode.IsControl(r):
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." || name == ".." {
		return "_"
	}
	return name
}
