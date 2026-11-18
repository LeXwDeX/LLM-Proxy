package proxy

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/LLM-Proxy/internal/config"
)

// observeWire feeds a completed SSE/JSON wire into a usageStream and returns it.
func observeWire(t *testing.T, contentType string, parser usageParser, wire string) *usageStream {
	t.Helper()
	u := newUsageStream(contentType, "", parser)
	if _, err := io.WriteString(u, wire); err != nil {
		t.Fatalf("write observation: %v", err)
	}
	u.Close()
	return u
}

// Content events are non-empty output deltas; role-only, empty and usage-only
// chunks are not content. Gemini detection is structural (protocol may be
// inferred only later from usageMetadata).
func TestUsageStreamContentEventRecognition(t *testing.T) {
	cases := []struct {
		name   string
		parser usageParser
		wire   string
		want   bool
	}{
		{
			name:   "chat role-only empty content",
			parser: usageParser{protocol: config.ProtocolOpenAIChat},
			wire:   "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\ndata: [DONE]\n\n",
		},
		{
			name:   "chat content",
			parser: usageParser{protocol: config.ProtocolOpenAIChat},
			wire:   "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: [DONE]\n\n",
			want:   true,
		},
		{
			name:   "chat reasoning_content",
			parser: usageParser{protocol: config.ProtocolOpenAIChat},
			wire:   "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"}}]}\n\ndata: [DONE]\n\n",
			want:   true,
		},
		{
			name:   "chat reasoning",
			parser: usageParser{protocol: config.ProtocolOpenAIChat},
			wire:   "data: {\"choices\":[{\"delta\":{\"reasoning\":\"think\"}}]}\n\ndata: [DONE]\n\n",
			want:   true,
		},
		{
			name:   "chat tool_calls arguments",
			parser: usageParser{protocol: config.ProtocolOpenAIChat},
			wire:   "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"a\\\":\"}}]}}]}\n\ndata: [DONE]\n\n",
			want:   true,
		},
		{
			name:   "chat tool_calls name only",
			parser: usageParser{protocol: config.ProtocolOpenAIChat},
			wire:   "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":\"lookup\"}}]}}]}\n\ndata: [DONE]\n\n",
			want:   true,
		},
		{
			name:   "chat empty delta",
			parser: usageParser{protocol: config.ProtocolOpenAIChat},
			wire:   "data: {\"choices\":[{\"delta\":{}}]}\n\ndata: [DONE]\n\n",
		},
		{
			name:   "chat usage-only chunk",
			parser: usageParser{protocol: config.ProtocolOpenAIChat},
			wire:   "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\ndata: [DONE]\n\n",
		},
		{
			name:   "responses output_text delta",
			parser: usageParser{protocol: config.ProtocolOpenAIResponses},
			wire:   "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hi\"}\n\n",
			want:   true,
		},
		{
			name:   "responses reasoning summary delta",
			parser: usageParser{protocol: config.ProtocolOpenAIResponses},
			wire:   "event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"why\"}\n\n",
			want:   true,
		},
		{
			name:   "responses function call arguments delta",
			parser: usageParser{protocol: config.ProtocolOpenAIResponses},
			wire:   "event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{\\\"a\\\":\"}\n\n",
			want:   true,
		},
		{
			name:   "responses empty delta",
			parser: usageParser{protocol: config.ProtocolOpenAIResponses},
			wire:   "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"\"}\n\n",
		},
		{
			name:   "anthropic text delta",
			parser: usageParser{protocol: config.ProtocolAnthropicMessages},
			wire:   "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n",
			want:   true,
		},
		{
			name:   "anthropic thinking delta",
			parser: usageParser{protocol: config.ProtocolAnthropicMessages},
			wire:   "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"why\"}}\n\n",
			want:   true,
		},
		{
			name:   "anthropic input_json_delta",
			parser: usageParser{protocol: config.ProtocolAnthropicMessages},
			wire:   "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"a\\\":\"}}\n\n",
			want:   true,
		},
		{
			name:   "anthropic empty text delta",
			parser: usageParser{protocol: config.ProtocolAnthropicMessages},
			wire:   "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"\"}}\n\n",
		},
		{
			name: "gemini text part",
			wire: "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"Hi\"}]}}]}\n\n",
			want: true,
		},
		{
			name: "gemini empty text part",
			wire: "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"\"}]}}]}\n\n",
		},
		{
			name: "gemini usage-only chunk",
			wire: "data: {\"usageMetadata\":{\"promptTokenCount\":1}}\n\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			u := observeWire(t, "text/event-stream", tc.parser, tc.wire)
			if u.contentSeen != tc.want {
				t.Fatalf("contentSeen = %v, want %v", u.contentSeen, tc.want)
			}
			if tc.want {
				if u.firstContentAt.IsZero() || u.lastContentAt.Before(u.firstContentAt) {
					t.Fatalf("content timestamps not recorded: first=%v last=%v", u.firstContentAt, u.lastContentAt)
				}
			}
		})
	}
}

// Only the first and last content events stamp the clock: intermediate chunks
// leave the timestamps untouched.
func TestUsageStreamContentTimestampsTrackFirstAndLast(t *testing.T) {
	wire := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n" +
		"data: [DONE]\n\n"
	u := observeWire(t, "text/event-stream", usageParser{protocol: config.ProtocolOpenAIChat}, wire)
	if !u.contentSeen || u.firstContentAt.IsZero() || u.lastContentAt.IsZero() {
		t.Fatalf("content not recorded: %+v", u)
	}
	if u.lastContentAt.Before(u.firstContentAt) {
		t.Fatalf("last content before first: %v < %v", u.lastContentAt, u.firstContentAt)
	}
}

func TestPerformanceSampleFromStreaming(t *testing.T) {
	base := time.Unix(1700000000, 0)
	streaming := func(first, last time.Duration, tokens int64, final bool) perfObservation {
		return perfObservation{
			Streaming:      true,
			Status:         http.StatusOK,
			StartedAt:      base,
			FinishedAt:     base.Add(5 * time.Second),
			ContentSeen:    true,
			FirstContentAt: base.Add(first),
			LastContentAt:  base.Add(last),
			FinalUsage:     final,
			OutputTokens:   tokens,
		}
	}

	t.Run("streaming TTFT and TPS", func(t *testing.T) {
		got, ok := performanceSampleFrom(streaming(500*time.Millisecond, 1500*time.Millisecond, 64, true))
		if !ok {
			t.Fatal("a content-bearing stream must produce a sample")
		}
		if !got.ttft || got.ttftMs != 500 {
			t.Fatalf("ttft = %v %v, want 500ms", got.ttft, got.ttftMs)
		}
		if !got.tps || math.Abs(got.tpsValue-64) > 1e-9 {
			t.Fatalf("tps = %v %v, want 64 tok/s over a one-second window", got.tps, got.tpsValue)
		}
		if got.nonStream {
			t.Fatal("a stream must not feed the non-stream totals")
		}
	})

	t.Run("below output threshold has TTFT only", func(t *testing.T) {
		got, ok := performanceSampleFrom(streaming(500*time.Millisecond, 1500*time.Millisecond, 10, true))
		if !ok || !got.ttft || got.tps {
			t.Fatalf("sample = %+v ok=%v, want TTFT without TPS", got, ok)
		}
	})

	t.Run("no final usage has TTFT only", func(t *testing.T) {
		got, ok := performanceSampleFrom(streaming(500*time.Millisecond, 1500*time.Millisecond, 64, false))
		if !ok || !got.ttft || got.tps {
			t.Fatalf("sample = %+v ok=%v, want TTFT without TPS", got, ok)
		}
	})

	t.Run("zero content window has TTFT only", func(t *testing.T) {
		got, ok := performanceSampleFrom(streaming(500*time.Millisecond, 500*time.Millisecond, 64, true))
		if !ok || !got.ttft || got.tps {
			t.Fatalf("sample = %+v ok=%v, want TTFT without TPS", got, ok)
		}
	})

	t.Run("non-streaming records duration and tokens only", func(t *testing.T) {
		got, ok := performanceSampleFrom(perfObservation{
			Status:       http.StatusOK,
			StartedAt:    base,
			FinishedAt:   base.Add(2 * time.Second),
			FinalUsage:   true,
			OutputTokens: 100,
		})
		if !ok || !got.nonStream || got.nonStreamMs != 2000 || got.outputTokens != 100 {
			t.Fatalf("sample = %+v ok=%v, want a non-stream sample", got, ok)
		}
		if got.ttft || got.tps || got.penalty {
			t.Fatalf("non-stream sample leaked into streaming fields: %+v", got)
		}
	})

	t.Run("non-2xx non-streaming is not a sample", func(t *testing.T) {
		if _, ok := performanceSampleFrom(perfObservation{
			Status: http.StatusBadRequest, StartedAt: base, FinishedAt: base.Add(time.Second), FinalUsage: true, OutputTokens: 3,
		}); ok {
			t.Fatal("a passed-through 4xx must not feed performance estimates")
		}
	})

	t.Run("local response is not a sample", func(t *testing.T) {
		obs := streaming(500*time.Millisecond, 1500*time.Millisecond, 64, true)
		obs.LocalResponse = true
		if _, ok := performanceSampleFrom(obs); ok {
			t.Fatal("a locally generated response must not feed performance estimates")
		}
	})
}

func TestPerformanceCancelPenalty(t *testing.T) {
	base := time.Unix(1700000000, 0)
	canceled := func(waited time.Duration, cause string) perfObservation {
		return perfObservation{
			Streaming:    true,
			Status:       http.StatusOK,
			StartedAt:    base,
			FinishedAt:   base.Add(waited),
			CopyErr:      context.Canceled,
			NoUsageCause: cause,
		}
	}

	t.Run("fast cancel is counted separately", func(t *testing.T) {
		got, ok := performanceSampleFrom(canceled(time.Second, noUsageCauseClientGone))
		if !ok || got.penalty || !got.fastCancel || got.ttft {
			t.Fatalf("sample = %+v ok=%v, want fast cancel only", got, ok)
		}
	})

	t.Run("post-threshold cancel is a penalty lower bound", func(t *testing.T) {
		got, ok := performanceSampleFrom(canceled(4*time.Second, noUsageCauseClientGone))
		if !ok || !got.penalty || got.penaltyMs != 4000 || got.fastCancel || got.ttft {
			t.Fatalf("sample = %+v ok=%v, want a 4s penalty sample", got, ok)
		}
	})

	t.Run("timeout cause also penalizes", func(t *testing.T) {
		obs := canceled(4*time.Second, noUsageCauseTimeout)
		obs.CopyErr = context.DeadlineExceeded
		got, ok := performanceSampleFrom(obs)
		if !ok || !got.penalty {
			t.Fatalf("sample = %+v ok=%v, want a penalty sample", got, ok)
		}
	})

	t.Run("clean empty stream is not a sample", func(t *testing.T) {
		if _, ok := performanceSampleFrom(perfObservation{
			Streaming: true, Status: http.StatusOK, StartedAt: base, FinishedAt: base.Add(5 * time.Second),
		}); ok {
			t.Fatal("an empty clean stream proves nothing about latency")
		}
	})
}

func perfTestSample(model string, ttftMs, tps float64, at time.Time) performanceSample {
	return performanceSample{model: model, ttft: true, ttftMs: ttftMs, tps: true, tpsValue: tps, at: at}
}

// Five-minute half-life: effective samples halve every half-life, drop below
// the sufficiency threshold and the estimate falls back to the neutral prior.
func TestPerformanceDecayAndInsufficient(t *testing.T) {
	p := newTargetPerformance()
	base := time.Unix(1700000000, 0)
	for i := 0; i < 5; i++ {
		p.observe(perfTestSample("m", 800, 100, base))
	}

	est, ok := p.estimate("m", base, "t")
	if !ok {
		t.Fatal("observed model missing")
	}
	if est.TTFT.Samples != 5 || est.TTFT.Insufficient {
		t.Fatalf("ttft samples = %d insufficient = %v, want 5 and sufficient", est.TTFT.Samples, est.TTFT.Insufficient)
	}
	if est.TPS.Samples != 5 || est.TPS.Insufficient {
		t.Fatalf("tps samples = %d insufficient = %v, want 5 and sufficient", est.TPS.Samples, est.TPS.Insufficient)
	}
	if est.TTFT.P50 < 600 || est.TTFT.P50 > 800 {
		t.Fatalf("ttft p50 = %v, want the 800ms bucket", est.TTFT.P50)
	}
	if est.TTFT.P90 < est.TTFT.P50 || est.TPS.P90 < est.TPS.P50 {
		t.Fatalf("percentiles out of order: %+v", est)
	}

	// One half-life halves the effective counts; the threshold is no longer met.
	half, ok := p.estimate("m", base.Add(perfDecayHalfLife), "t")
	if !ok {
		t.Fatal("observed model missing after decay")
	}
	if half.TTFT.Samples < 2 || half.TTFT.Samples > 3 {
		t.Fatalf("ttft samples after one half-life = %d, want about 2.5", half.TTFT.Samples)
	}
	if !half.TTFT.Insufficient || !half.Insufficient {
		t.Fatalf("estimate must be insufficient after one half-life: %+v", half)
	}

	// Natural regression to the prior: after enough half-lives no effective
	// samples remain and predictedCompletionMS uses the documented constants.
	gone, ok := p.estimate("m", base.Add(30*time.Minute), "t")
	if !ok {
		t.Fatal("observed model missing after long decay")
	}
	if gone.TTFT.Samples != 0 || gone.TPS.Samples != 0 {
		t.Fatalf("effective samples must decay to zero, got %+v", gone)
	}
	if got := predictedCompletionMS(gone); got != perfPriorTTFTMs+float64(perfReferenceOutputTokens)/perfPriorTPS*1000 {
		t.Fatalf("predicted completion = %v, want the neutral prior", got)
	}
}

func TestPredictedCompletionUsesSufficientMedians(t *testing.T) {
	p := newTargetPerformance()
	now := time.Unix(1700000000, 0)
	for i := 0; i < 5; i++ {
		p.observe(perfTestSample("m", 700, 80, now))
	}
	est, _ := p.estimate("m", now, "t")
	want := est.TTFT.P50 + float64(perfReferenceOutputTokens)/est.TPS.P50*1000
	if got := predictedCompletionMS(est); math.Abs(got-want) > 1e-9 {
		t.Fatalf("predicted completion = %v, want %v", got, want)
	}
}

func TestPerformanceModelTableIsBounded(t *testing.T) {
	p := newTargetPerformance()
	now := time.Unix(1700000000, 0)
	for i := 0; i < maxPerfModels+1; i++ {
		p.observe(perfTestSample("model-"+itoa(i), 500, 50, now))
	}
	if got := p.droppedCount(); got != 1 {
		t.Fatalf("dropped = %d, want 1", got)
	}
	if _, ok := p.estimate("model-"+itoa(maxPerfModels), now, "t"); ok {
		t.Fatal("the overflow model must not be tracked")
	}
	if _, ok := p.estimate("model-0", now, "t"); !ok {
		t.Fatal("a tracked model disappeared")
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var digits [8]byte
	i := len(digits)
	for v > 0 {
		i--
		digits[i] = byte('0' + v%10)
		v /= 10
	}
	return string(digits[i:])
}

func perfTestURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// Non-identity edits (capacity, pool, pause) keep the estimates; a new
// connection identity or a resume starts empty, like the breaker state.
func TestPerformanceStateFollowsConfigIdentity(t *testing.T) {
	s := &Service{logger: newTestLogger()}
	oldTarget := &Target{Name: "a", Endpoint: perfTestURL(t, "https://a.example")}
	old := newTargetState(oldTarget, s.logger)
	now := time.Unix(1700000000, 0)
	old.recordPerformance("m", perfObservation{
		Streaming: true, StartedAt: now, FinishedAt: now.Add(time.Second),
		ContentSeen: true, FirstContentAt: now.Add(400 * time.Millisecond), LastContentAt: now.Add(900 * time.Millisecond),
	})
	shared := old.performanceStats()
	s.targetsByName = map[string]*targetState{"a": old}
	s.targetOrder = []*targetState{old}

	nonIdentity := newTargetState(&Target{Name: "a", Endpoint: perfTestURL(t, "https://a.example"), MaxConcurrency: 4}, s.logger)
	logResets := s.replaceTargetsLocked(map[string]*targetState{"a": nonIdentity}, []*targetState{nonIdentity})
	logResets()
	if nonIdentity.perf.Load() != shared {
		t.Fatal("a non-identity edit must share the existing performance state")
	}
	if est, ok := nonIdentity.performanceEstimate("m", now); !ok || est.TTFT.Samples != 1 {
		t.Fatalf("shared estimate lost its sample: %+v ok=%v", est, ok)
	}

	// A new upstream identity starts empty.
	identityChange := newTargetState(&Target{Name: "a", Endpoint: perfTestURL(t, "https://other.example")}, s.logger)
	logResets = s.replaceTargetsLocked(map[string]*targetState{"a": identityChange}, []*targetState{identityChange})
	logResets()
	if identityChange.perf.Load() != nil {
		t.Fatal("an identity change must start with an empty performance state")
	}
	if _, ok := identityChange.performanceEstimate("m", now); ok {
		t.Fatal("the new identity must not report the old model estimate")
	}

	// Resume (paused -> active) also forks, matching shouldForkTargetHealth.
	paused := newTargetState(&Target{Name: "b", Endpoint: perfTestURL(t, "https://b.example"), Paused: true}, s.logger)
	now2 := now
	paused.recordPerformance("m", perfObservation{
		Streaming: true, StartedAt: now2, FinishedAt: now2.Add(time.Second),
		ContentSeen: true, FirstContentAt: now2.Add(300 * time.Millisecond), LastContentAt: now2.Add(800 * time.Millisecond),
	})
	s.targetsByName["b"] = paused
	s.targetOrder = append(s.targetOrder, paused)
	resumed := newTargetState(&Target{Name: "b", Endpoint: perfTestURL(t, "https://b.example")}, s.logger)
	logResets = s.replaceTargetsLocked(map[string]*targetState{"a": identityChange, "b": resumed}, []*targetState{identityChange, resumed})
	logResets()
	if resumed.perf.Load() != nil {
		t.Fatal("a resume must start with an empty performance state")
	}
}

func TestServicePerformanceSnapshots(t *testing.T) {
	s := &Service{logger: newTestLogger()}
	state := newTargetState(&Target{Name: "a"}, s.logger)
	now := time.Unix(1700000000, 0)
	state.recordPerformance("m", perfObservation{
		Streaming: true, StartedAt: now, FinishedAt: now.Add(time.Second),
		ContentSeen: true, FirstContentAt: now.Add(500 * time.Millisecond), LastContentAt: now.Add(time.Second),
	})
	state.recordPerformance("m", perfObservation{
		Status: http.StatusOK, StartedAt: now, FinishedAt: now.Add(2 * time.Second), FinalUsage: true, OutputTokens: 100,
	})
	s.targetsByName = map[string]*targetState{"a": state}
	s.targetOrder = []*targetState{state}

	est, ok := s.TargetModelPerformance("a", "m", now)
	if !ok || est.Target != "a" || est.UpstreamModel != "m" || est.TTFT.Samples != 1 || est.NonStream.Samples != 1 {
		t.Fatalf("estimate = %+v ok=%v", est, ok)
	}
	// Snapshots are copies: mutating one must not change the state.
	est.TTFT.P50 = -1
	again, _ := s.TargetModelPerformance("a", "m", now)
	if again.TTFT.P50 == -1 {
		t.Fatal("snapshot exposed mutable state")
	}
	if _, ok := s.TargetModelPerformance("a", "absent", now); ok {
		t.Fatal("an unobserved model must not report an estimate")
	}

	batch := s.TargetPerformanceSnapshot(now)
	if len(batch) != 1 || batch[0].Target != "a" || len(batch[0].Models) != 1 {
		t.Fatalf("batch snapshot = %+v", batch)
	}
}

// The observation pipe must exist even when no usage recorder, trace store or
// request statistics recorder is configured: the performance estimate depends
// on it and must not silently disappear.
func TestWriteResponseRecordsPerformanceWithoutOtherObservers(t *testing.T) {
	s := &Service{logger: newTestLogger()}
	state := newTargetState(&Target{Name: "t"}, s.logger)
	s.targetsByName = map[string]*targetState{"t": state}
	s.targetOrder = []*targetState{state}
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: [DONE]\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	startedAt := time.Now()
	s.writeResponse(httptest.NewRecorder(), req, state, resp, func() {}, 1, "up-model", nil, startedAt, 0, false, nil, nil)

	est, ok := s.TargetModelPerformance("t", "up-model", time.Now())
	if !ok {
		t.Fatal("writeResponse dropped the performance observation")
	}
	if est.TTFT.Samples != 1 || !est.TTFT.Insufficient {
		t.Fatalf("ttft estimate = %+v, want one (insufficient) sample", est.TTFT)
	}
	if est.TPS.Samples != 0 {
		t.Fatalf("tps samples = %d, want 0 without final usage", est.TPS.Samples)
	}
}

func TestPerformanceObservationSwitch(t *testing.T) {
	s := &Service{}
	if !s.performanceObservationEnabled() {
		t.Fatal("performance observation must default to enabled")
	}
	s.SetPerformanceObservationEnabled(false)
	if s.performanceObservationEnabled() {
		t.Fatal("switch did not disable performance observation")
	}
	s.SetPerformanceObservationEnabled(true)
	if !s.performanceObservationEnabled() {
		t.Fatal("switch did not re-enable performance observation")
	}
}

func TestPerformanceConcurrentRecordAndRead(t *testing.T) {
	p := newTargetPerformance()
	now := time.Unix(1700000000, 0)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				p.observe(perfTestSample("m", float64(100+worker), float64(10+j%50), now))
				_, _ = p.estimate("m", now, "t")
				_ = p.snapshot(now, "t")
			}
		}(i)
	}
	wg.Wait()
	if est, ok := p.estimate("m", now, "t"); !ok || est.TTFT.Samples == 0 {
		t.Fatalf("estimate = %+v ok=%v after concurrent writes", est, ok)
	}
}

func BenchmarkPerformanceObserve(b *testing.B) {
	p := newTargetPerformance()
	now := time.Unix(1700000000, 0)
	sample := perfTestSample("m", 500, 50, now)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p.observe(sample)
	}
}

func BenchmarkPerformanceEstimate(b *testing.B) {
	p := newTargetPerformance()
	now := time.Unix(1700000000, 0)
	for i := 0; i < 5; i++ {
		p.observe(perfTestSample("m", 500, 50, now))
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = p.estimate("m", now, "t")
	}
}
