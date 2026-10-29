package admin

import "embed"

// Embed only the shipped UI sources: the shell, stylesheet, HTML partials and
// scripts. The *.cjs Node tests living next to the scripts are not matched.
// Requests never read arbitrary files from disk.
//
//go:embed ui/index.html ui/css ui/partials ui/js/core/*.js ui/js/pages/*/*.js
var adminUIFS embed.FS

// adminUIJSFS is the embed FS served by handleUIJS; script paths are
// "ui/js/<core|pages/<name>>/<file>.js".
var adminUIJSFS = adminUIFS

// adminUIHTML is ui/index.html with every `<!-- @include path -->` line
// expanded (see assembleAdminHTML). It is built once at package init; embedded
// content is fixed at build time, so a broken include panics loudly there.
var adminUIHTML = mustAssembleAdminHTML(adminUIFS, "ui")
