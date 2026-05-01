//go:build !webclient_ui

package webclient

import "io/fs"

// embeddedStaticFS returns the embedded UI filesystem (if any) and its root
// directory within the FS.
//
// This stub returns nil so the server falls back to a minimal placeholder page.
// Another file may provide an embedded implementation (e.g. via go:embed) in the
// same package.
func embeddedStaticFS() (fs.FS, string) {
	return nil, ""
}
