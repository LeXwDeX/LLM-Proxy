package admin

import "embed"

//go:embed ui/index.html
var adminUIHTML string

// Embed only the scripts shipped with the application. Requests never read
// arbitrary files from disk.
//
//go:embed ui/js/*.js
var adminUIJSFS embed.FS
