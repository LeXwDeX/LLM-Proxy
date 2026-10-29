package admin

import (
	"fmt"
	"io/fs"
	"regexp"
	"strings"
)

// uiIncludeRe matches a line that holds only an include directive. The whole
// line (including its newline) is replaced by the included file verbatim.
var uiIncludeRe = regexp.MustCompile(`^[ \t]*<!-- @include (\S+) -->[ \t]*$`)

const uiIncludeMarker = "@include"

// assembleAdminHTML expands `<!-- @include rel/path -->` lines of root/index.html.
// Include paths are relative to root, may nest, must exist, must not form a
// cycle, and every included file must end with a newline. Leftover include
// markers are an error.
func assembleAdminHTML(fsys fs.FS, root string) (string, error) {
	return expandAdminHTML(fsys, root, "index.html", nil)
}

func expandAdminHTML(fsys fs.FS, root, rel string, stack []string) (string, error) {
	if !fs.ValidPath(rel) {
		return "", fmt.Errorf("invalid include path %q", rel)
	}
	for _, p := range stack {
		if p == rel {
			return "", fmt.Errorf("include cycle: %s -> %s", strings.Join(stack, " -> "), rel)
		}
	}
	data, err := fs.ReadFile(fsys, root+"/"+rel)
	if err != nil {
		return "", fmt.Errorf("include %q: %w", rel, err)
	}
	src := string(data)
	if len(stack) > 0 && !strings.HasSuffix(src, "\n") {
		return "", fmt.Errorf("included file %q must end with a newline", rel)
	}
	var out strings.Builder
	for _, line := range strings.SplitAfter(src, "\n") {
		if m := uiIncludeRe.FindStringSubmatch(strings.TrimRight(line, "\r\n")); m != nil {
			sub, err := expandAdminHTML(fsys, root, m[1], append(stack, rel))
			if err != nil {
				return "", err
			}
			out.WriteString(sub)
			continue
		}
		if strings.Contains(line, uiIncludeMarker) {
			return "", fmt.Errorf("%s: malformed or leftover include marker: %q", rel, strings.TrimSpace(line))
		}
		out.WriteString(line)
	}
	return out.String(), nil
}

func mustAssembleAdminHTML(fsys fs.FS, root string) string {
	html, err := assembleAdminHTML(fsys, root)
	if err != nil {
		panic("admin ui: " + err.Error())
	}
	return html
}
