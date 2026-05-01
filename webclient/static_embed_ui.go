package webclient

import (
	"embed"
	"io/fs"
)

// Embed UI assets under ui/static. Worker-2 owns the actual files.
//
//go:embed all:ui/static
var embeddedUI embed.FS

func embeddedStaticFS() (fs.FS, string) {
	return embeddedUI, "ui/static"
}
