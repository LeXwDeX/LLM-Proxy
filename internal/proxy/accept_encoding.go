// accept_encoding.go — 观察用量的上游请求不协商 br，客户端不能借 br 观察预算耗尽规避计费。
package proxy

import "strings"

// usageObservationEnabled reports whether a successful response would be
// observed for usage, billing traces or request statistics (writeResponse).
func (s *Service) usageObservationEnabled() bool {
	return s.currentUsageRecorder() != nil || s.traceStore != nil || s.currentRequestStatsRecorder() != nil
}

// withoutBrotliAcceptEncoding removes br from merged Accept-Encoding field
// values. br observation shares a bounded decode budget (512 MiB by default);
// a client offering br over many long streams could exhaust it and have its
// later streams settled with zero tokens (observation_failed). The other
// codings keep their spelling and q-values. A wildcard would still admit br,
// so br;q=0 is added beside it. When nothing remains the result is identity:
// an absent header admits any coding (RFC 9110), and it would also make the
// proxy add its own transparently decoded gzip, changing the forwarded bytes.
// changed is false when the values already exclude br; the header is then
// left untouched.
func withoutBrotliAcceptEncoding(values []string) (string, bool) {
	var kept []string
	changed, wildcard := false, false
	for _, value := range values {
		for _, member := range strings.Split(value, ",") {
			member = strings.TrimSpace(member)
			if member == "" {
				continue
			}
			coding, _, _ := strings.Cut(member, ";")
			coding = strings.TrimSpace(coding)
			if strings.EqualFold(coding, "br") {
				changed = true
				continue
			}
			if coding == "*" {
				wildcard = true
			}
			kept = append(kept, member)
		}
	}
	if wildcard {
		kept = append(kept, "br;q=0")
		changed = true
	}
	if !changed {
		return "", false
	}
	if len(kept) == 0 {
		return "identity", true
	}
	return strings.Join(kept, ", "), true
}
