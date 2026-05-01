//go:build webclient_ui

package webclient

import (
	"embed"
	"io/fs"
)

// When built with -tags webclient_ui, embed UI assets under ui/static/.
// Worker-2 owns the actual files under webclient/ui/static.
//
//go:embed ui/static/*
var embeddedUI embed.FS

func embeddedStaticFS() (fs.FS, string) {
	return embeddedUI, "ui/static"
}

