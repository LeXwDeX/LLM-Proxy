package fakellm

import (
	"net/http"
	"regexp"
	"strings"
	"time"
)

// The fake matches operations by path suffix so one instance can stand in for
// any provider base. A real provider does not: a path the proxy assembled
// wrongly (an endpoint configured as a full operation URL with the client path
// appended, or two version segments stacked) gets a 404 there. Answering such
// paths would hide proxy path bugs behind a 200, so they are rejected here too.

var pathVersionSegmentRe = regexp.MustCompile(`^v\d+[a-z]*$`)

// operationEndSegments close an API operation path (…/chat/completions,
// …/responses, …/images/generations).
var operationEndSegments = map[string]bool{
	"completions": true, "responses": true, "embeddings": true, "messages": true,
	"generations": true, "edits": true, "variations": true,
}

// operationStartSegments open one. Sub-resources that legitimately follow an
// operation (…/responses/{id}, …/messages/count_tokens, …/messages/batches,
// …/chat/completions/{id}/messages) are not in this set.
var operationStartSegments = map[string]bool{
	"chat": true, "completions": true, "responses": true, "messages": true,
	"embeddings": true, "images": true,
}

// malformedUpstreamPath reports why no real provider would serve path, or ""
// when the path is well formed. It looks only at the shape of the path.
func malformedUpstreamPath(path string) string {
	segments := strings.FieldsFunc(strings.ToLower(path), func(r rune) bool { return r == '/' })
	for i := 0; i+1 < len(segments); i++ {
		current, next := segments[i], segments[i+1]
		nextIsVersion := pathVersionSegmentRe.MatchString(next)
		if pathVersionSegmentRe.MatchString(current) && nextIsVersion {
			return "stacked version segments /" + current + "/" + next
		}
		if operationEndSegments[current] && (nextIsVersion || operationStartSegments[next]) {
			return "path continues with /" + next + " after the API operation /" + current
		}
	}
	return ""
}

// rejectsMalformedPaths is false only for the legacy mock contract, which
// answered any path.
func (s *Server) rejectsMalformedPaths() bool { return !s.opts.LegacyMock }

// rejectMalformedPath answers like a provider gateway that has no such route.
// The request is recorded and counted, but consumes no scenario: it never
// reached an operation.
func (s *Server) rejectMalformedPath(w http.ResponseWriter, r *http.Request, bodyBytes int) {
	scheme, key := authOf(r)
	rec := Record{Seq: s.seq.Add(1), Time: time.Now(), Method: r.Method, Path: r.URL.Path, Query: redactQuery(r),
		Auth: scheme, Key: KeyFingerprint(key), BodyBytes: bodyBytes, Status: http.StatusNotFound, Outcome: "error"}
	s.mu.Lock()
	s.stats.Total++
	s.stats.Paths[r.URL.Path]++
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte("404 page not found\n"))
	s.record(rec)
}
