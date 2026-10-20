// urlencoded_normalize.go — application/x-www-form-urlencoded 请求体的受保护字段规范化。
package proxy

import (
	"bytes"
	"net/url"
)

// normalizeURLEncodedMembers applies the JSON and multipart normalization rule
// to a urlencoded form. The proxy uses the last pair whose decoded key equals a
// protected field name exactly (extractModel); every other pair whose decoded
// key folds to that name, exact duplicates and case variants alike, is
// removed, and with only variants all are removed. Only the removed "k=v"
// segments go: the other segments keep their raw bytes, percent-encoding and
// order, empty segments included. A body needing no change is returned as is.
// Forms url.ParseQuery rejects are unchanged: extractModel reads no model
// from them.
func normalizeURLEncodedMembers(body []byte) ([]byte, []memberNormalization) {
	if len(body) == 0 {
		return body, nil
	}
	if _, err := url.ParseQuery(string(body)); err != nil {
		return body, nil
	}
	segments := bytes.Split(body, []byte("&"))
	protected := make([]int, len(segments))
	keep := make([]int, len(protectedFormFields))
	for i := range keep {
		keep[i] = -1
	}
	counts := make([]int, len(protectedFormFields))
	needed := false
	for i, segment := range segments {
		protected[i] = -1
		if len(segment) == 0 {
			continue // url.ParseQuery skips empty segments.
		}
		rawKey, _, _ := bytes.Cut(segment, []byte("="))
		key, err := url.QueryUnescape(string(rawKey))
		if err != nil {
			return body, nil // unreachable after ParseQuery succeeded
		}
		p := protectedFormFieldIndex(key)
		if p < 0 {
			continue
		}
		protected[i] = p
		counts[p]++
		if key == protectedFormFields[p] {
			keep[p] = i
		}
		if counts[p] > 1 || key != protectedFormFields[p] {
			needed = true
		}
	}
	if !needed {
		return body, nil
	}
	removed := make([]int, len(protectedFormFields))
	out := make([]byte, 0, len(body))
	first := true
	for i, segment := range segments {
		if p := protected[i]; p >= 0 && keep[p] != i {
			removed[p]++
			continue
		}
		if !first {
			out = append(out, '&')
		}
		out = append(out, segment...)
		first = false
	}
	var report []memberNormalization
	for p, n := range removed {
		if n > 0 {
			report = append(report, memberNormalization{Member: protectedFormFields[p], Removed: n})
		}
	}
	return out, report
}
