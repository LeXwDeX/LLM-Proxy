package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/auth"
	"github.com/LeXwDeX/LLM-Proxy/internal/config"
	"github.com/coder/websocket"
)

// Pool maintenance timings are variables so tests can shorten them.
var (
	responsesWSReapInterval = 15 * time.Second
	// Idle lanes are pinged so a silently dropped path is retired before reuse.
	responsesWSPingAfter   = 30 * time.Second
	responsesWSPingTimeout = 10 * time.Second
	// A target that rejected the upgrade is sent over HTTP for this long.
	responsesWSUnsupportedTTL = 10 * time.Minute
	// Fresh spare lanes are dialed in the background after reusable turns.
	responsesWSPrewarm = true
	// A failed background dial suppresses further spares for that identity.
	responsesWSPrewarmBackoff = time.Minute
	// A fresh request waits at most this long for its identity's spare lane
	// that is still being dialed, then dials in the foreground itself.
	responsesWSSpareWait = 200 * time.Millisecond
)

// responsesWSCloseHook lets tests simulate a close that stalls.
var responsesWSCloseHook atomic.Pointer[func(*websocket.Conn)]

// responsesWSCloseConn closes one pooled socket. A TLS close_notify to a peer
// that stopped reading may block it for seconds, so the pool never calls it
// while holding p.mu.
func responsesWSCloseConn(c *websocket.Conn) {
	if hook := responsesWSCloseHook.Load(); hook != nil {
		(*hook)(c)
		return
	}
	_ = c.CloseNow()
}

const (
	responsesWSIdle      = 5 * time.Minute
	responsesWSAge       = 55 * time.Minute
	responsesWSMax       = 128
	responsesWSPerClient = 4
	// A lane stops reading when its consumer leaves a frame unread this long.
	responsesWSFrameBacklog = 30 * time.Second
	// Upper bound of one upstream WebSocket handshake.
	responsesWSDialTimeout = 5 * time.Second
)

type responsesWSFrame struct {
	data []byte
	err  error
}

// Only a transport failure before a handshake response is safe to fail over:
// no response.create frame could have reached the upstream yet.
type responsesWSHandshakeError struct{ err error }

func (e *responsesWSHandshakeError) Error() string { return e.err.Error() }
func (e *responsesWSHandshakeError) Unwrap() error { return e.err }

// Dial closes failed handshake bodies and retains only their first 1024 bytes.
// Keep the original non-upgrade body out of Dial's ownership so the proxy can
// stream the complete HTTP error without buffering it or issuing another call.
type responsesWSHandshakeTransport struct {
	owned    *http.Transport
	base     http.RoundTripper
	response *http.Response
	body     io.ReadCloser
}

func (t *responsesWSHandshakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// A followed redirect has finished with its previous response. Its real
	// body was hidden from http.Client too, so release it before the next call.
	if t.body != nil {
		_ = t.body.Close()
		t.body, t.response = nil, nil
	}
	automatic := automaticGzipNegotiation(t.base, t.owned, req)
	if automatic {
		req.Header.Set("Accept-Encoding", "gzip")
	}
	resp, err := t.base.RoundTrip(req)
	if automatic && err == nil {
		normalizeAutomaticGzip(resp, req.Context())
	}
	if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols && resp.Body != nil {
		t.response, t.body = resp, resp.Body
		resp.Body = io.NopCloser(bytes.NewReader(nil))
	}
	return resp, err
}

func (t *responsesWSHandshakeTransport) restore(resp *http.Response) {
	if t.body == nil {
		return
	}
	if resp == t.response {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
		resp.Body = t.body
	} else {
		_ = t.body.Close()
	}
	t.body, t.response = nil, nil
}

type responsesWSConn struct {
	conn          *websocket.Conn
	frames        chan responsesWSFrame
	owner         [32]byte
	key           [32]byte
	created, used time.Time
	busy, retired bool
	pinging       bool
	pinged        time.Time
	responseID    string
	// dial is how another lane with this identity is opened in the background.
	dial *responsesWSDialSpec
	// prewarming marks a busy placeholder whose background dial is in flight;
	// ready closes when that dial ends. prewarmed marks an unused spare lane.
	prewarming, prewarmed bool
	ready                 chan struct{}
	// cancelDial aborts a placeholder's background dial when it is dropped;
	// dialCtx is that dial's context until the dial starts.
	cancelDial context.CancelFunc
	dialCtx    context.Context
}

// responsesWSDialSpec repeats a lane's handshake. Headers are the identity
// headers only: per-request tracing headers are not replayed.
type responsesWSDialSpec struct {
	client  *http.Client
	url     string
	host    string
	headers http.Header
}

type responsesWSPool struct {
	automaticGzipTransport *http.Transport
	mu                     sync.Mutex
	entries                map[*responsesWSConn]struct{}
	ctx                    context.Context
	cancel                 context.CancelFunc
	closed                 bool
	generation             uint64
	once                   sync.Once
	// unsupported remembers handshake identities whose upgrade was rejected.
	unsupported map[[32]byte]time.Time
	// Timings are read once at construction so tests can vary them safely.
	reapInterval, pingAfter, pingTimeout, unsupportedTTL time.Duration
	prewarm                                              bool
	prewarmBackoffTTL, spareWait                         time.Duration
	// Native session upstream sockets are pinged between turns with these.
	nativePingAfter, nativePingTimeout time.Duration
	// prewarmBackoff suppresses spares for identities whose background dial failed.
	prewarmBackoff map[[32]byte]time.Time
	stats          responsesWSPoolStats
}

func newResponsesWSPool() *responsesWSPool {
	ctx, cancel := context.WithCancel(context.Background())
	return &responsesWSPool{entries: make(map[*responsesWSConn]struct{}), ctx: ctx, cancel: cancel,
		reapInterval: responsesWSReapInterval, pingAfter: responsesWSPingAfter,
		pingTimeout: responsesWSPingTimeout, unsupportedTTL: responsesWSUnsupportedTTL,
		prewarm: responsesWSPrewarm, prewarmBackoffTTL: responsesWSPrewarmBackoff, spareWait: responsesWSSpareWait,
		nativePingAfter: responsesWebsocketUpstreamPingAfter, nativePingTimeout: responsesWebsocketUpstreamPingTimeout}
}

// closeResponsesWSConns closes sockets already removed from the pool. Callers
// release p.mu first; async keeps request and configuration paths off a close
// that stalls on an unresponsive peer.
func closeResponsesWSConns(conns []*websocket.Conn, async bool) {
	if len(conns) == 0 {
		return
	}
	if async {
		go closeResponsesWSConns(conns, false)
		return
	}
	for _, c := range conns {
		responsesWSCloseConn(c)
	}
}

// removeLocked retires e and drops it from the pool, queueing its socket to be
// closed once the caller has released p.mu.
func (p *responsesWSPool) removeLocked(e *responsesWSConn, closing []*websocket.Conn) []*websocket.Conn {
	e.retired = true
	delete(p.entries, e)
	if e.prewarming && e.cancelDial != nil {
		e.cancelDial() // its dial then discards whatever it may still open
	}
	if e.conn != nil {
		closing = append(closing, e.conn)
	}
	return closing
}

func (p *responsesWSPool) invalidate(closeActive bool) {
	p.mu.Lock()
	p.generation++
	p.unsupported = nil
	p.prewarmBackoff = nil
	var closing []*websocket.Conn
	for e := range p.entries {
		e.retired = true
		// A background placeholder has no request to wait for: drop it now,
		// which cancels its dial before it reaches the upstream again.
		if !e.busy || closeActive || e.prewarming {
			closing = p.removeLocked(e, closing)
		}
	}
	p.mu.Unlock()
	// Shutdown waits for its closes; a configuration change must not stall
	// behind them (it runs while the service holds its target lock).
	closeResponsesWSConns(closing, !closeActive)
}

func (p *responsesWSPool) close() {
	p.mu.Lock()
	p.closed = true
	p.cancel()
	p.mu.Unlock()
	p.invalidate(true)
}

func (p *responsesWSPool) reap() {
	ticker := time.NewTicker(p.reapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case now := <-ticker.C:
			p.reapAt(now)
		}
	}
}

func (p *responsesWSPool) reapAt(now time.Time) {
	var closing []*websocket.Conn
	p.mu.Lock()
	defer func() {
		p.mu.Unlock()
		closeResponsesWSConns(closing, false)
	}()
	// Expired memories are otherwise dropped only when looked up again.
	for identity, until := range p.prewarmBackoff {
		if !now.Before(until) {
			delete(p.prewarmBackoff, identity)
		}
	}
	for identity, until := range p.unsupported {
		if now.After(until) {
			delete(p.unsupported, identity)
		}
	}
	for e := range p.entries {
		if now.Sub(e.created) >= responsesWSAge || (!e.busy && now.Sub(e.used) >= responsesWSIdle) {
			e.retired = true
		}
		if !e.busy && e.retired {
			closing = p.removeLocked(e, closing)
			continue
		}
		if !e.busy && !e.retired && !e.pinging && e.conn != nil &&
			now.Sub(e.used) >= p.pingAfter && now.Sub(e.pinged) >= p.pingAfter {
			e.pinging = true
			go p.ping(e)
		}
	}
}

// ping checks an idle lane; the lane's reader goroutine processes the pong.
func (p *responsesWSPool) ping(e *responsesWSConn) {
	ctx, cancel := context.WithTimeout(p.ctx, p.pingTimeout)
	err := e.conn.Ping(ctx)
	cancel()
	var closing []*websocket.Conn
	p.mu.Lock()
	e.pinging = false
	e.pinged = time.Now()
	if err != nil {
		e.retired = true
		if !e.busy {
			closing = p.removeLocked(e, closing)
		}
	}
	p.mu.Unlock()
	closeResponsesWSConns(closing, false)
}

// responsesWSUpgradeUnsupported reports a handshake status that means the
// upstream does not offer WebSocket here. 400 is too broad to remember: it may
// come from one client's headers.
func responsesWSUpgradeUnsupported(status int) bool {
	return status == 404 || status == 405 || status == 426 || status == 501
}

// markUnsupportedLocked remembers a rejected handshake identity unless the
// configuration changed while the handshake was in flight. The caller holds p.mu.
func (p *responsesWSPool) markUnsupportedLocked(identity [32]byte, gen uint64) {
	if p.generation != gen {
		return
	}
	if p.unsupported == nil {
		p.unsupported = make(map[[32]byte]time.Time)
	}
	p.unsupported[identity] = time.Now().Add(p.unsupportedTTL)
}

// knownUnsupportedLocked is the read side; the caller holds p.mu.
func (p *responsesWSPool) knownUnsupportedLocked(identity [32]byte) bool {
	until, ok := p.unsupported[identity]
	if ok && time.Now().After(until) {
		delete(p.unsupported, identity)
		return false
	}
	return ok
}

func (p *responsesWSPool) knownUnsupported(identity [32]byte) (bool, uint64) {
	if p == nil {
		return false, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.knownUnsupportedLocked(identity), p.generation
}

func (p *responsesWSPool) markUnsupported(identity [32]byte, gen uint64) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.markUnsupportedLocked(identity, gen)
}

// discardStaleFrames empties frames left over from a finished turn. It reports
// false when the lane's reader has ended (the socket is gone).
func (e *responsesWSConn) discardStaleFrames() bool {
	for {
		select {
		case frame, ok := <-e.frames:
			if !ok || frame.err != nil {
				return false
			}
		default:
			return true
		}
	}
}

// freeSlotLocked makes room for a new foreground lane (of owner, or anywhere
// when owner is nil). It gives up, in order: an idle unused spare, a spare
// still being dialed (its dial is canceled) and then the least recently used
// idle lane. A lane busy with a request stays exclusively owned by it. The
// caller holds p.mu.
func (p *responsesWSPool) freeSlotLocked(owner *[32]byte, closing *[]*websocket.Conn) bool {
	var oldest, dialing *responsesWSConn
	for e := range p.entries {
		if owner != nil && e.owner != *owner {
			continue
		}
		if e.prewarming {
			dialing = e
			continue
		}
		if e.busy {
			continue
		}
		if oldest == nil || (e.prewarmed && !oldest.prewarmed) || (e.prewarmed == oldest.prewarmed && e.used.Before(oldest.used)) {
			oldest = e
		}
	}
	switch {
	case oldest != nil && oldest.prewarmed:
	case dialing != nil:
		oldest = dialing
	case oldest == nil:
		return false
	}
	*closing = p.removeLocked(oldest, *closing)
	return true
}

func (p *responsesWSPool) release(e *responsesWSConn, reusable bool) {
	var closing []*websocket.Conn
	var spare *responsesWSConn
	p.mu.Lock()
	e.busy = false
	e.used = time.Now()
	if !reusable || e.retired || p.closed {
		closing = p.removeLocked(e, closing)
	} else {
		spare, closing = p.reserveSpareLocked(e, closing)
	}
	var spareCtx context.Context
	if spare != nil {
		spareCtx = spare.dialCtx
		spare.dialCtx = nil
	}
	gen := p.generation
	p.mu.Unlock()
	// The request that owned the lane still releases its socket before it
	// finishes; only the pool lock is no longer held during the close.
	closeResponsesWSConns(closing, false)
	if spare != nil {
		go p.prewarmDial(spare, spareCtx, gen)
	}
}

// reserveSpareLocked places a busy placeholder for a fresh spare lane when the
// released lane's client has no idle fresh lane for this identity, so the
// client's next new request need not wait for a cold handshake. It respects
// the same per-client and global caps as foreground lanes, and keeps at most
// one spare per client: an unused spare of the client's other identity is
// replaced by one for the identity just used. The caller holds p.mu.
func (p *responsesWSPool) reserveSpareLocked(e *responsesWSConn, closing []*websocket.Conn) (*responsesWSConn, []*websocket.Conn) {
	if !p.prewarm || p.closed || e.dial == nil || e.retired {
		return nil, closing
	}
	count := 0
	var replaced *responsesWSConn
	for x := range p.entries {
		if x.owner != e.owner {
			continue
		}
		count++
		if x.retired {
			continue
		}
		if x.prewarming || (x.key == e.key && !x.busy && x.responseID == "") {
			return nil, closing // a dial is in flight, or a fresh lane is ready
		}
		if x.prewarmed && !x.busy {
			replaced = x
		}
	}
	now := time.Now()
	if until, ok := p.prewarmBackoff[e.key]; ok {
		if now.Before(until) {
			return nil, closing
		}
		delete(p.prewarmBackoff, e.key)
	}
	if p.knownUnsupportedLocked(e.key) {
		return nil, closing
	}
	if replaced != nil {
		closing = p.removeLocked(replaced, closing)
		count--
	}
	if count >= responsesWSPerClient || len(p.entries) >= responsesWSMax {
		return nil, closing
	}
	spare := &responsesWSConn{owner: e.owner, key: e.key, created: now, used: now, busy: true,
		prewarming: true, ready: make(chan struct{}), dial: e.dial, frames: make(chan responsesWSFrame, 8)}
	// The dial is bounded like a foreground one, and dropping the placeholder
	// (configuration change, blocked key, capacity) cancels it.
	timeout := responsesWSDialTimeout
	if t := e.dial.client.Timeout; t > 0 && t < timeout {
		timeout = t
	}
	spare.dialCtx, spare.cancelDial = context.WithTimeout(p.ctx, timeout)
	p.entries[spare] = struct{}{}
	return spare, closing
}

// retireSparesForCredentials retires spare lanes, idle or still dialing, whose
// handshake carried one of these credentials, once the key pool has taken
// them out of rotation. Lanes holding a conversation keep their own lifecycle.
func (p *responsesWSPool) retireSparesForCredentials(keys []string) {
	if p == nil || len(keys) == 0 {
		return
	}
	var closing []*websocket.Conn
	p.mu.Lock()
	for e := range p.entries {
		if (e.prewarming || (e.prewarmed && !e.busy)) && e.dial != nil && headersCarryCredential(e.dial.headers, keys) {
			closing = p.removeLocked(e, closing)
		}
	}
	p.mu.Unlock()
	closeResponsesWSConns(closing, true)
}

// headersCarryCredential reports whether a handshake authenticated with one
// of keys, compared exactly in the headers the provider auth strategies set:
// Authorization "Bearer <key>", api-key (Azure), x-api-key (Anthropic) and
// x-goog-api-key (Gemini). A key that is a substring of another key, or of
// any other header value, never matches.
func headersCarryCredential(headers http.Header, keys []string) bool {
	for _, key := range keys {
		if key == "" {
			continue
		}
		for _, value := range headers.Values("Authorization") {
			if value == "Bearer "+key {
				return true
			}
		}
		for _, name := range []string{"Api-Key", "X-Api-Key", "X-Goog-Api-Key"} {
			for _, value := range headers.Values(name) {
				if value == key {
					return true
				}
			}
		}
	}
	return false
}

// prewarmDial opens a reserved spare lane. It is not a client request: no
// outcome reaches the key pool, target health, request metrics or the
// unsupported memory. A failure only backs off further spares for the
// identity; a configuration change or shutdown discards the socket.
func (p *responsesWSPool) prewarmDial(e *responsesWSConn, ctx context.Context, gen uint64) {
	defer close(e.ready)
	defer e.cancelDial()
	spec := e.dial
	dialClient := *spec.client
	baseTransport := spec.client.Transport
	if baseTransport == nil {
		baseTransport = http.DefaultTransport
	}
	transport := &responsesWSHandshakeTransport{base: baseTransport, owned: p.automaticGzipTransport}
	dialClient.Transport = transport
	dialClient.Timeout = 0
	ctx, observed := p.stats.prewarm.trace(ctx)
	c, handshake, err := websocket.Dial(ctx, spec.url, &websocket.DialOptions{HTTPClient: &dialClient, HTTPHeader: spec.headers.Clone(), Host: spec.host})
	observed()
	transport.restore(handshake)
	if handshake != nil && handshake.Body != nil {
		_ = handshake.Body.Close()
	}
	var closing []*websocket.Conn
	p.mu.Lock()
	_, present := p.entries[e]
	current := present && !e.retired && !p.closed && p.generation == gen
	switch {
	case !current:
		if c != nil {
			closing = append(closing, c)
		}
		p.stats.prewarmDiscarded.Add(1)
	case err != nil:
		if p.prewarmBackoff == nil {
			p.prewarmBackoff = make(map[[32]byte]time.Time)
		}
		p.prewarmBackoff[e.key] = time.Now().Add(p.prewarmBackoffTTL)
		p.stats.prewarmDialFail.Add(1)
	default:
		c.SetReadLimit(maxRequestBodyBytes)
		now := time.Now()
		e.conn, e.created, e.used = c, now, now
		e.busy, e.prewarming, e.prewarmed = false, false, true
		p.stats.prewarmDialOK.Add(1)
	}
	if !current || err != nil {
		e.retired = true
		delete(p.entries, e)
	}
	p.mu.Unlock()
	closeResponsesWSConns(closing, false)
	if current && err == nil {
		go p.read(e)
	}
}

func (p *responsesWSPool) read(e *responsesWSConn) {
	defer func() {
		p.mu.Lock()
		e.retired = true
		if !e.busy {
			delete(p.entries, e)
		}
		p.mu.Unlock()
	}()
	defer close(e.frames)
	defer e.conn.CloseNow()
	// One timer for the lane's lifetime instead of one allocation per frame.
	backlog := time.NewTimer(responsesWSFrameBacklog)
	defer backlog.Stop()
	for {
		typ, data, err := e.conn.Read(p.ctx)
		if err == nil && typ != websocket.MessageText {
			err = errors.New("non-text Responses WebSocket event")
		}
		// Reset without Stop/drain relies on Go 1.23+ timer semantics: Reset
		// discards any stale expiry still buffered in backlog.C. Running with
		// GODEBUG=asynctimerchan=1 restores the old asynchronous channel and is
		// not supported here, because a stale tick could then end a healthy
		// lane immediately.
		// 依赖 Go 1.23+ 计时器语义（Reset 会丢弃 C 中残留的过期值）；
		// 不支持 GODEBUG=asynctimerchan=1。
		backlog.Reset(responsesWSFrameBacklog)
		select {
		case e.frames <- responsesWSFrame{data, err}:
		case <-p.ctx.Done():
			return
		case <-backlog.C:
			// A slow downstream must not accumulate an unbounded event backlog.
			return
		}
		if err != nil {
			return
		}
	}
}

// acquire reserves one exclusive request lane before any generation is sent.
func (p *responsesWSPool) acquire(ctx context.Context, client *http.Client, req *http.Request, owner [32]byte, previous string) (*responsesWSConn, *http.Response, bool, error) {
	headers := req.Header.Clone()
	for _, h := range []string{"Content-Type", "Content-Length", "Accept", "Accept-Encoding", "X-Request-Id", "User-Agent", "Sec-Websocket-Key", "Sec-Websocket-Version", "Sec-Websocket-Extensions", "Sec-Websocket-Protocol"} {
		headers.Del(h)
	}
	// Per-request tracing and hop metadata still reach the handshake, but must
	// not split one client's connection (and its continuation) per request.
	identityHeaders := headers.Clone()
	for name := range identityHeaders {
		if responsesWSVolatileHeader(name) {
			delete(identityHeaders, name)
		}
	}
	identity, _ := json.Marshal(struct {
		URL     string
		Headers http.Header
	}{req.URL.String(), identityHeaders})
	key := sha256.Sum256(identity)
	if client == nil {
		client = http.DefaultClient
	}
	// Sockets retired while selecting are closed only after unlocking.
	var closing []*websocket.Conn
	unlock := func() {
		p.mu.Unlock()
		closeResponsesWSConns(closing, true)
		closing = nil
	}
	p.mu.Lock()
	if p.closed {
		unlock()
		return nil, nil, false, errors.New("WebSocket pool closed")
	}
	p.once.Do(func() { go p.reap() })
	now := time.Now()
	waited := false
selectLane:
	count := 0
	var chosen, cached, pending *responsesWSConn
	for e := range p.entries {
		if !e.busy && (e.retired || now.Sub(e.created) >= responsesWSAge || now.Sub(e.used) >= responsesWSIdle) {
			closing = p.removeLocked(e, closing)
			continue
		}
		if e.owner == owner {
			count++
		}
		if e.owner != owner || e.key != key || e.retired {
			continue
		}
		if e.prewarming {
			pending = e // a spare for this identity is being dialed
			continue
		}
		if previous != "" {
			if e.responseID == previous {
				if e.busy {
					unlock()
					return nil, nil, false, errors.New("previous response is currently in use")
				}
				chosen = e
				break
			}
		} else if !e.busy {
			if e.responseID == "" {
				chosen = e
			} else if cached == nil || e.used.Before(cached.used) {
				cached = e
			}
		}
	}
	if previous != "" && chosen == nil {
		unlock()
		return nil, wsErrorResponse(req, 400, "previous_response_not_found", "Previous response is not available for this client on an active connection."), false, nil
	}
	// A spare already being dialed is usually further along than a new
	// handshake, and keeps another conversation's lane from being reclaimed.
	// Wait for it once and only briefly: a background dial that hangs must not
	// delay the request, which then dials in the foreground itself.
	if chosen == nil && pending != nil && !waited {
		ready := pending.ready
		unlock()
		wait := time.NewTimer(p.spareWait)
		select {
		case <-ready:
		case <-wait.C:
		case <-ctx.Done():
			wait.Stop()
			return nil, nil, false, ctx.Err()
		}
		wait.Stop()
		waited = true
		p.mu.Lock()
		if p.closed {
			unlock()
			return nil, nil, false, errors.New("WebSocket pool closed")
		}
		now = time.Now()
		goto selectLane
	}
	// Reuse a lane holding another conversation's latest response only when no
	// new lane fits; that conversation then gets a deterministic not-found.
	reclaimed := false
	if chosen == nil && cached != nil && (count >= responsesWSPerClient || len(p.entries) >= responsesWSMax) {
		chosen = cached
		chosen.responseID = ""
		reclaimed = true
	}
	if chosen != nil {
		// Late frames of the previous turn must not become this turn's events.
		if !chosen.discardStaleFrames() {
			closing = p.removeLocked(chosen, closing)
			goto selectLane
		}
		chosen.busy = true
		switch {
		case previous != "":
			p.stats.hitContinuation.Add(1)
		case reclaimed:
			p.stats.hitReuseCached.Add(1)
		default:
			p.stats.hitFresh.Add(1)
		}
		if chosen.prewarmed {
			chosen.prewarmed = false
			p.stats.prewarmUsed.Add(1)
		}
		unlock()
		return chosen, nil, false, nil
	}
	// Only a new handshake consults the unsupported cache; existing lanes and
	// their continuations stay usable.
	if p.knownUnsupportedLocked(key) {
		p.stats.fallbackUnsupported.Add(1)
		unlock()
		return nil, nil, true, nil
	}
	if count >= responsesWSPerClient && p.freeSlotLocked(&owner, &closing) {
		count--
	}
	if count < responsesWSPerClient && len(p.entries) >= responsesWSMax {
		p.freeSlotLocked(nil, &closing)
	}
	if count >= responsesWSPerClient || len(p.entries) >= responsesWSMax {
		// Nothing was sent yet: ordinary HTTP is a safe transport for this request.
		p.stats.fallbackCap.Add(1)
		unlock()
		return nil, nil, true, nil
	}
	e := &responsesWSConn{owner: owner, key: key, created: now, used: now, busy: true, frames: make(chan responsesWSFrame, 8),
		dial: &responsesWSDialSpec{client: client, url: req.URL.String(), host: req.Host, headers: identityHeaders}}
	p.entries[e] = struct{}{}
	gen := p.generation
	unlock()
	p.stats.newHandshake.Add(1)
	p.stats.sampleHandshakeHeaders(owner, identityHeaders)
	dialClient := *client
	baseTransport := client.Transport
	if baseTransport == nil {
		baseTransport = http.DefaultTransport
	}
	transport := &responsesWSHandshakeTransport{base: baseTransport, owned: p.automaticGzipTransport}
	dialClient.Transport = transport
	timeout := responsesWSDialTimeout
	if client.Timeout > 0 && client.Timeout < timeout {
		timeout = client.Timeout
	}
	// Own the timeout here: Dial's client-timeout context would otherwise be
	// canceled when Dial returns, before the retained HTTP body is consumed.
	dialClient.Timeout = 0
	dialCtx, cancel := context.WithCancel(ctx)
	dialTimer := time.AfterFunc(timeout, cancel)
	cancelHandedOff := false
	defer func() {
		if !cancelHandedOff {
			cancel()
		}
	}()
	traceCtx, observed := p.stats.foreground.trace(dialCtx)
	c, handshake, err := websocket.Dial(traceCtx, req.URL.String(), &websocket.DialOptions{HTTPClient: &dialClient, HTTPHeader: headers, Host: req.Host})
	observed()
	handshakeTimedOut := !dialTimer.Stop()
	if handshakeTimedOut {
		cancel()
	}
	hadHandshake := transport.response != nil
	transport.restore(handshake)
	if handshakeTimedOut && ctx.Err() == nil {
		if c != nil {
			_ = c.CloseNow()
		}
		if handshake != nil && handshake.Body != nil {
			_ = handshake.Body.Close()
		}
		p.release(e, false)
		return nil, nil, false, context.DeadlineExceeded
	}
	if err != nil {
		p.release(e, false)
		if ctx.Err() != nil {
			if handshake != nil && handshake.Body != nil {
				_ = handshake.Body.Close()
			}
			return nil, nil, false, ctx.Err()
		}
		if dialCtx.Err() != nil {
			if handshake != nil && handshake.Body != nil {
				_ = handshake.Body.Close()
			}
			return nil, nil, false, context.DeadlineExceeded
		}
		if handshake != nil {
			if handshake.StatusCode == 400 || handshake.StatusCode == 404 || handshake.StatusCode == 405 || handshake.StatusCode == 426 || handshake.StatusCode == 501 {
				_ = handshake.Body.Close()
				if responsesWSUpgradeUnsupported(handshake.StatusCode) {
					p.markUnsupported(key, gen)
				}
				p.stats.fallbackUnsupported.Add(1)
				return nil, nil, true, nil
			}
			// A rejected upgrade cannot represent a successful generation.
			if handshake.StatusCode < http.StatusBadRequest {
				handshake.StatusCode = http.StatusBadGateway
				handshake.Status = fmt.Sprintf("%d %s", http.StatusBadGateway, http.StatusText(http.StatusBadGateway))
			}
			handshake.Request = req
			wsBody := &responsesWSBody{ReadCloser: &releaseOnCloseBody{ReadCloser: handshake.Body, release: cancel}}
			wsBody.setOutcome(true, handshake.StatusCode, nil)
			wsBody.noMute.Store(!isUpstreamFailureStatus(handshake.StatusCode))
			handshake.Body = wsBody
			cancelHandedOff = true
			return nil, handshake, false, nil
		}
		if hadHandshake {
			return nil, nil, false, err
		}
		return nil, nil, false, &responsesWSHandshakeError{err}
	}
	c.SetReadLimit(maxRequestBodyBytes)
	p.mu.Lock()
	if p.closed || p.generation != gen {
		p.mu.Unlock()
		_ = c.CloseNow()
		p.release(e, false)
		return nil, nil, false, errors.New("target configuration changed during WebSocket handshake")
	}
	e.conn = c
	p.mu.Unlock()
	if handshake.Body != nil {
		_ = handshake.Body.Close()
	}
	go p.read(e)
	return e, nil, false, nil
}

type responsesWSBody struct {
	io.ReadCloser
	generationSent bool
	local          bool // proxy-generated; the upstream was not contacted
	failed         atomic.Bool
	noMute         atomic.Bool
	outcomeMu      sync.Mutex
	status         int
	errorBody      []byte
}

func (b *responsesWSBody) setOutcome(failed bool, status int, body []byte) {
	b.outcomeMu.Lock()
	b.status, b.errorBody = status, bytes.Clone(body)
	b.outcomeMu.Unlock()
	b.failed.Store(failed)
}

func (b *responsesWSBody) outcome() (int, []byte) {
	b.outcomeMu.Lock()
	defer b.outcomeMu.Unlock()
	return b.status, bytes.Clone(b.errorBody)
}

// The generation may have reached upstream. Stage is diagnostic metadata only;
// it must not change cancellation classification or grant replay permission.
type responsesWSSentError struct {
	err   error
	stage string
}

func (e *responsesWSSentError) Error() string { return e.err.Error() }
func (e *responsesWSSentError) Unwrap() error { return e.err }

func wsErrorResponse(req *http.Request, status int, code, message string) *http.Response {
	data, _ := json.Marshal(map[string]any{"error": map[string]any{"type": "invalid_request_error", "code": code, "message": message}})
	b := &responsesWSBody{ReadCloser: io.NopCloser(bytes.NewReader(data)), local: true}
	b.noMute.Store(true)
	b.setOutcome(true, status, data)
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: b, Request: req}
}

func (s *Service) tryResponsesWS(r, req *http.Request, target *Target, body []byte) (*http.Response, bool, error) {
	if !target.PreferWebSocket || target.EndpointType != config.EndpointTypeOpenAI || r.Method != http.MethodPost || r.URL.Path != "/v1/responses" {
		return nil, false, nil
	}
	payload, ok := scanJSONObject(nil, body)
	if !ok {
		return nil, false, nil
	}
	if string(jsonLast(payload, "background")) == "true" {
		return nil, false, nil
	}
	stream := string(jsonLast(payload, "stream")) == "true"
	var previous string
	if raw := jsonLast(payload, "previous_response_id"); raw != nil && string(raw) != "null" {
		if previous, ok = jsonString(raw); !ok {
			return nil, false, nil
		}
	}
	// Internal WebSocket envelope fields must not be supplied through the HTTP
	// body. Only these members are edited; the rest keeps its original bytes.
	// WebSocket mode rejects the HTTP shorthand of a plain-string input; send
	// the equivalent single user message, reusing the string token as is.
	input := jsonLast(payload, "input")
	var inputMember []byte
	if len(input) > 0 && input[0] == '"' {
		inputMember = append(append([]byte(`"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":`), input...), "}]}]"...)
	}
	wire := jsonRebuild(payload, []byte(`"type":"response.create"`), func(m jsonMember) ([]byte, bool) {
		switch {
		case m.named("stream"), m.named("background"), m.named("stream_id"), m.named("type"):
			return nil, false
		case inputMember != nil && m.named("input"):
			// The body was normalized at request entry: input is unique.
			return inputMember, true
		}
		return m.span, true
	}, nil)
	principal, _ := auth.PrincipalFromContext(r.Context())
	owner := sha256.Sum256([]byte(principal.Name + "\x00" + principal.AccessKey + "\x00" + target.Name))
	e, resp, fallback, err := s.responsesWS.acquire(req.Context(), s.httpClient, req, owner, previous)
	if fallback {
		return nil, false, nil
	}
	if err != nil || resp != nil {
		return resp, true, err
	}
	// Nothing is written yet: a request already canceled is not a sent generation.
	if err = req.Context().Err(); err != nil {
		s.responsesWS.release(e, true)
		return nil, true, err
	}
	// Any write error is ambiguous: never replay this generation over HTTP or another target.
	if err = e.conn.Write(req.Context(), websocket.MessageText, wire); err != nil {
		s.responsesWS.release(e, false)
		return nil, true, &responsesWSSentError{err: err, stage: "send"}
	}
	var first responsesWSFrame
	select {
	case first = <-e.frames:
	case <-req.Context().Done():
		first.err = req.Context().Err()
	}
	if first.err != nil {
		s.responsesWS.release(e, false)
		return nil, true, &responsesWSSentError{err: first.err, stage: "wait_first_event"}
	}
	var event struct {
		Type   string          `json:"type"`
		Status int             `json:"status"`
		Error  json.RawMessage `json:"error"`
	}
	if json.Unmarshal(first.data, &event) != nil {
		s.responsesWS.release(e, false)
		return nil, true, &responsesWSSentError{err: errors.New("invalid Responses WebSocket event"), stage: "decode_first_event"}
	}
	if event.Type == "error" {
		s.responsesWS.release(e, false)
		_, status, errorBody := responsesWebsocketOutcome(first.data)
		if status < 400 || status > 599 {
			status = 502
		}
		data, _ := json.Marshal(map[string]json.RawMessage{"error": event.Error})
		b := &responsesWSBody{ReadCloser: io.NopCloser(bytes.NewReader(data)), generationSent: true}
		b.setOutcome(true, status, errorBody)
		b.noMute.Store(!isUpstreamFailureStatus(status))
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: b, Request: req}, true, nil
	}
	reader, writer := io.Pipe()
	b := &responsesWSBody{ReadCloser: reader, generationSent: true}
	ct := "application/json"
	var ready chan error
	if stream {
		ct = "text/event-stream"
	} else {
		ready = make(chan error, 1)
	}
	go func() {
		reusable := false
		terminalReady := false
		var responseErr error
		defer func() {
			s.responsesWS.release(e, reusable)
			_ = writer.Close()
			if !stream && !terminalReady {
				ready <- responseErr
			}
		}()
		frame := first
		var members []jsonMember // reused per event; frame.data owns the bytes
		for {
			if frame.err != nil {
				responseErr = frame.err
				b.failed.Store(true)
				_ = writer.CloseWithError(frame.err)
				return
			}
			var ok bool
			if members, ok = scanJSONObject(members[:0], frame.data); !ok {
				responseErr = errors.New("invalid Responses WebSocket event")
				b.failed.Store(true)
				_ = writer.CloseWithError(responseErr)
				return
			}
			typ, _ := jsonString(jsonLast(members, "type"))
			if typ == "" || strings.ContainsAny(typ, "\r\n") {
				responseErr = errors.New("invalid Responses event type")
				b.failed.Store(true)
				_ = writer.CloseWithError(responseErr)
				return
			}
			terminal := typ == "response.completed" || typ == "response.failed" || typ == "response.incomplete" || typ == "error"
			if terminal {
				failed, status, errorBody := responsesWebsocketOutcome(frame.data)
				// Health follows the classified status: an unclassified error
				// (e.g. context_window_exceeded) is not evidence of an upstream fault.
				b.noMute.Store(!isUpstreamFailureStatus(status))
				if typ == "error" && (status < 400 || status > 599) {
					status = http.StatusBadGateway // never report an error event as 200
				}
				b.setOutcome(failed, status, errorBody)
				s.responsesWS.mu.Lock()
				e.responseID = ""
				if typ == "response.completed" {
					var response struct {
						ID string `json:"id"`
					}
					_ = json.Unmarshal(jsonLast(members, "response"), &response)
					e.responseID = response.ID
				}
				s.responsesWS.mu.Unlock()
			}
			if stream {
				// Forward the event bytes as received; only the internal
				// stream_id member is removed, and SSE needs a single line.
				event := frame.data
				if jsonHas(members, "stream_id") {
					event = jsonRebuild(members, nil, func(m jsonMember) ([]byte, bool) { return m.span, !m.named("stream_id") }, nil)
				}
				event = jsonSingleLine(event)
				sse := make([]byte, 0, len(typ)+len(event)+16)
				sse = append(append(append(append(append(sse, "event: "...), typ...), "\ndata: "...), event...), "\n\n"...)
				if _, err := writer.Write(sse); err != nil {
					b.failed.Store(true)
					return
				}
			} else if terminal {
				output := jsonLast(members, "response")
				if typ == "error" {
					errorValue := jsonLast(members, "error")
					if errorValue == nil {
						errorValue = []byte("null")
					}
					output = append(append([]byte(`{"error":`), errorValue...), '}')
				}
				// A buffered HTTP response must be a response object. RawMessage
				// also accepts null and scalars; preserve valid objects byte for byte
				// while rejecting unusable snapshots without replaying the generation.
				if trimmed := bytes.TrimSpace(output); len(trimmed) == 0 || trimmed[0] != '{' {
					responseErr = errors.New("invalid terminal response object")
					b.failed.Store(true)
					b.noMute.Store(false)
					_ = writer.CloseWithError(responseErr)
					return
				}
				terminalReady = true
				ready <- nil
				if _, err := writer.Write(output); err != nil {
					b.failed.Store(true)
					return
				}
			}
			if terminal {
				reusable = typ == "response.completed"
				return
			}
			select {
			case frame = <-e.frames:
			case <-req.Context().Done():
				frame = responsesWSFrame{err: req.Context().Err()}
			}
		}
	}()
	status := http.StatusOK
	if !stream {
		if err := <-ready; err != nil {
			_ = reader.Close()
			return nil, true, &responsesWSSentError{err: err, stage: "wait_terminal_event"}
		}
		if terminalStatus, _ := b.outcome(); terminalStatus >= 400 && terminalStatus <= 599 {
			status = terminalStatus
		}
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{ct}}, Body: b, Request: req}, true, nil
}

func responsesWSVolatileHeader(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Traceparent", "Tracestate", "Baggage", "Sentry-Trace", "X-Amzn-Trace-Id", "X-Cloud-Trace-Context",
		"Cf-Ray", "Cf-Connecting-Ip", "X-Forwarded-For", "X-Real-Ip", "X-Request-Start",
		"X-Stainless-Retry-Count", "X-Stainless-Timeout":
		return true
	}
	return strings.HasPrefix(http.CanonicalHeaderKey(name), "X-B3-")
}

func responseWSBody(resp *http.Response) *responsesWSBody {
	if resp == nil {
		return nil
	}
	if b, ok := resp.Body.(*responsesWSBody); ok {
		return b
	}
	if b, ok := resp.Body.(*releaseOnCloseBody); ok {
		if ws, ok := b.ReadCloser.(*responsesWSBody); ok {
			return ws
		}
	}
	return nil
}

// ResponsesWSMetrics summarizes upstream Responses WebSocket handshakes and
// lane reuse for /admin/metrics. Counters are cumulative since start.
type ResponsesWSMetrics struct {
	HitFresh            int64 `json:"hit_fresh"`            // fresh request took an idle lane without a held response
	HitReuseCached      int64 `json:"hit_reuse_cached"`     // fresh request reclaimed a lane holding another conversation
	HitContinuation     int64 `json:"hit_continuation"`     // previous_response_id request took its own lane
	NewHandshake        int64 `json:"new_handshake"`        // pooled foreground handshakes started
	NativeHandshake     int64 `json:"native_handshake"`     // native session handshakes started
	FallbackCap         int64 `json:"fallback_cap"`         // pool full: request sent over HTTP
	FallbackUnsupported int64 `json:"fallback_unsupported"` // upgrade rejected (or remembered as such): HTTP
	PrewarmDialOK       int64 `json:"prewarm_dial_ok"`      // background spare lanes opened
	PrewarmDialFail     int64 `json:"prewarm_dial_fail"`    // background dials that failed (identity backed off)
	PrewarmUsed         int64 `json:"prewarm_used"`         // spare lanes taken by a request
	PrewarmDiscarded    int64 `json:"prewarm_discarded"`    // background dials dropped by a config change or shutdown
	NativePingFail      int64 `json:"native_ping_fail"`     // idle native upstream sockets closed after a failed ping
	// Handshake phase durations of every upstream WebSocket dial (pooled,
	// background and native), in milliseconds.
	HandshakeDNS     DurationHistogram `json:"handshake_dns_ms"`
	HandshakeTCP     DurationHistogram `json:"handshake_tcp_ms"`
	HandshakeTLS     DurationHistogram `json:"handshake_tls_ms"`
	HandshakeUpgrade DurationHistogram `json:"handshake_upgrade_ms"`
	// The same phases for background spare-lane dials only.
	PrewarmHandshakeDNS     DurationHistogram `json:"prewarm_handshake_dns_ms"`
	PrewarmHandshakeTCP     DurationHistogram `json:"prewarm_handshake_tcp_ms"`
	PrewarmHandshakeTLS     DurationHistogram `json:"prewarm_handshake_tls_ms"`
	PrewarmHandshakeUpgrade DurationHistogram `json:"prewarm_handshake_upgrade_ms"`
	// HeaderChanges counts identity header names whose values differed from the
	// same client's previous pooled handshake. Values are never kept.
	HeaderChangeSamples int64            `json:"header_change_samples"`
	HeaderChanges       map[string]int64 `json:"header_changes"`
}

// DurationHistogram has non-cumulative buckets; Overflow counts samples above
// the last bound.
type DurationHistogram struct {
	Count    int64            `json:"count"`
	SumMs    float64          `json:"sum_ms"`
	Buckets  []DurationBucket `json:"buckets"`
	Overflow int64            `json:"overflow"`
}

// DurationBucket counts samples above the previous bound and up to LeMs.
type DurationBucket struct {
	LeMs  float64 `json:"le_ms"`
	Count int64   `json:"count"`
}

var responsesWSHistogramBoundsMs = [...]float64{1, 2, 5, 10, 25, 50, 100, 250, 500, 1000, 2500, 5000}

type responsesWSHistogram struct {
	counts [len(responsesWSHistogramBoundsMs) + 1]atomic.Int64
	sumNs  atomic.Int64
}

func (h *responsesWSHistogram) observe(d time.Duration) {
	ms := float64(d) / float64(time.Millisecond)
	i := 0
	for i < len(responsesWSHistogramBoundsMs) && ms > responsesWSHistogramBoundsMs[i] {
		i++
	}
	h.counts[i].Add(1)
	h.sumNs.Add(int64(d))
}

func (h *responsesWSHistogram) snapshot() DurationHistogram {
	out := DurationHistogram{Buckets: make([]DurationBucket, len(responsesWSHistogramBoundsMs))}
	for i, bound := range responsesWSHistogramBoundsMs {
		out.Buckets[i] = DurationBucket{LeMs: bound, Count: h.counts[i].Load()}
		out.Count += out.Buckets[i].Count
	}
	out.Overflow = h.counts[len(responsesWSHistogramBoundsMs)].Load()
	out.Count += out.Overflow
	out.SumMs = float64(h.sumNs.Load()) / float64(time.Millisecond)
	return out
}

const (
	responsesWSHeaderOwners = 256 // clients whose previous handshake headers are remembered
	responsesWSHeaderNames  = 32  // distinct changed names kept; the rest count as "(other)"
)

// responsesWSHeaderSalt keys the in-memory value fingerprints, which are only
// compared for equality and never exported.
var responsesWSHeaderSalt = func() (salt [16]byte) {
	_, _ = rand.Read(salt[:])
	return salt
}()

type responsesWSPoolStats struct {
	hitFresh, hitReuseCached, hitContinuation, newHandshake, nativeHandshake atomic.Int64
	fallbackCap, fallbackUnsupported                                         atomic.Int64
	prewarmDialOK, prewarmDialFail, prewarmUsed, prewarmDiscarded            atomic.Int64
	nativePingFail                                                           atomic.Int64
	// Foreground (pooled and native) and background spare handshakes.
	foreground, prewarm responsesWSHandshakeHistograms

	headerMu      sync.Mutex
	headerLast    map[[32]byte]map[string]uint64
	headerChanges map[string]int64
	headerSamples int64
}

// sampleHandshakeHeaders records which identity header names changed since the
// owner's previous handshake. Only salted fingerprints of values are held, in
// memory, for the comparison; names are bounded and values never leave.
func (st *responsesWSPoolStats) sampleHandshakeHeaders(owner [32]byte, headers http.Header) {
	fingerprints := make(map[string]uint64, len(headers))
	for name, values := range headers {
		h := fnv.New64a()
		_, _ = h.Write(responsesWSHeaderSalt[:])
		for _, value := range values {
			_, _ = h.Write([]byte(value))
			_, _ = h.Write([]byte{0})
		}
		fingerprints[http.CanonicalHeaderKey(name)] = h.Sum64()
	}
	st.headerMu.Lock()
	defer st.headerMu.Unlock()
	previous, seen := st.headerLast[owner]
	if st.headerLast == nil || (!seen && len(st.headerLast) >= responsesWSHeaderOwners) {
		st.headerLast = make(map[[32]byte]map[string]uint64)
	}
	st.headerLast[owner] = fingerprints
	if !seen {
		return
	}
	st.headerSamples++
	changed := func(name string) {
		if st.headerChanges == nil {
			st.headerChanges = make(map[string]int64)
		}
		if _, ok := st.headerChanges[name]; !ok && len(st.headerChanges) >= responsesWSHeaderNames {
			name = "(other)"
		}
		st.headerChanges[name]++
	}
	for name, value := range fingerprints {
		if old, ok := previous[name]; !ok || old != value {
			changed(name)
		}
	}
	for name := range previous {
		if _, ok := fingerprints[name]; !ok {
			changed(name)
		}
	}
}

// responsesWSHandshakeHistograms holds the phase durations of one kind of dial.
type responsesWSHandshakeHistograms struct {
	dns, tcp, tls, upgrade responsesWSHistogram
}

// trace times DNS, TCP connect, TLS and the upgrade round trip of one dial.
// Call the returned function once the dial returns.
func (st *responsesWSHandshakeHistograms) trace(ctx context.Context) (context.Context, func()) {
	var mu sync.Mutex
	var dnsStart, connectStart, tlsStart, wrote time.Time
	var dns, tcp, tlsDone, upgrade time.Duration
	trace := &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { mu.Lock(); dnsStart = time.Now(); mu.Unlock() },
		DNSDone: func(info httptrace.DNSDoneInfo) {
			mu.Lock()
			if info.Err == nil && dns == 0 && !dnsStart.IsZero() {
				dns = max(time.Since(dnsStart), 1)
			}
			mu.Unlock()
		},
		ConnectStart: func(string, string) {
			mu.Lock()
			if connectStart.IsZero() {
				connectStart = time.Now()
			}
			mu.Unlock()
		},
		ConnectDone: func(_, _ string, err error) {
			mu.Lock()
			if err == nil && tcp == 0 && !connectStart.IsZero() {
				tcp = max(time.Since(connectStart), 1)
			}
			mu.Unlock()
		},
		TLSHandshakeStart: func() { mu.Lock(); tlsStart = time.Now(); mu.Unlock() },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			mu.Lock()
			if err == nil && tlsDone == 0 && !tlsStart.IsZero() {
				tlsDone = max(time.Since(tlsStart), 1)
			}
			mu.Unlock()
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			mu.Lock()
			if info.Err == nil && wrote.IsZero() {
				wrote = time.Now()
			}
			mu.Unlock()
		},
		GotFirstResponseByte: func() {
			mu.Lock()
			if upgrade == 0 && !wrote.IsZero() {
				upgrade = max(time.Since(wrote), 1)
			}
			mu.Unlock()
		},
	}
	return httptrace.WithClientTrace(ctx, trace), func() {
		mu.Lock()
		defer mu.Unlock()
		for _, phase := range []struct {
			d time.Duration
			h *responsesWSHistogram
		}{{dns, &st.dns}, {tcp, &st.tcp}, {tlsDone, &st.tls}, {upgrade, &st.upgrade}} {
			if phase.d > 0 {
				phase.h.observe(phase.d)
			}
		}
	}
}

// observeNativeHandshake counts and times one native session handshake.
func (p *responsesWSPool) observeNativeHandshake(ctx context.Context) (context.Context, func()) {
	if p == nil {
		return ctx, func() {}
	}
	p.stats.nativeHandshake.Add(1)
	return p.stats.foreground.trace(ctx)
}

func (p *responsesWSPool) metricsSnapshot() ResponsesWSMetrics {
	if p == nil {
		return ResponsesWSMetrics{}
	}
	st := &p.stats
	m := ResponsesWSMetrics{
		HitFresh: st.hitFresh.Load(), HitReuseCached: st.hitReuseCached.Load(), HitContinuation: st.hitContinuation.Load(),
		NewHandshake: st.newHandshake.Load(), NativeHandshake: st.nativeHandshake.Load(),
		FallbackCap: st.fallbackCap.Load(), FallbackUnsupported: st.fallbackUnsupported.Load(),
		PrewarmDialOK: st.prewarmDialOK.Load(), PrewarmDialFail: st.prewarmDialFail.Load(),
		PrewarmUsed: st.prewarmUsed.Load(), PrewarmDiscarded: st.prewarmDiscarded.Load(),
		NativePingFail: st.nativePingFail.Load(),
		HandshakeDNS:   st.foreground.dns.snapshot(), HandshakeTCP: st.foreground.tcp.snapshot(),
		HandshakeTLS: st.foreground.tls.snapshot(), HandshakeUpgrade: st.foreground.upgrade.snapshot(),
		PrewarmHandshakeDNS: st.prewarm.dns.snapshot(), PrewarmHandshakeTCP: st.prewarm.tcp.snapshot(),
		PrewarmHandshakeTLS: st.prewarm.tls.snapshot(), PrewarmHandshakeUpgrade: st.prewarm.upgrade.snapshot(),
		HeaderChanges: map[string]int64{},
	}
	st.headerMu.Lock()
	m.HeaderChangeSamples = st.headerSamples
	for name, count := range st.headerChanges {
		m.HeaderChanges[name] = count
	}
	st.headerMu.Unlock()
	return m
}
