// Package web holds the Zex UI so the desktop app can serve it from inside its binary.
package web

import "embed"

//go:embed index.html favicon.svg fonts
var FS embed.FS
