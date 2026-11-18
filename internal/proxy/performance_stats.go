// performance_stats.go — 每个（target，上游模型原始拼写）的上游生成性能估计（#212）。
//
// 采样只在 writeResponse 收尾发生：TTFT 从 attempt 起点（含排队、选 key、拨号）
// 到首个内容事件；TPS 为最终输出 token 除以首末内容事件的间隔，仅在输出 token
// 达到门槛时记录；非流式响应单独记录总耗时与输出 token，不混入流式分位。
//
// 为什么选带时间衰减的固定分桶直方图，而不是 EWMA 或滑动窗口：
//   - 需要分位数（p50/p90），EWMA 只能给出均值；
//   - 固定桶 + 惰性指数衰减的内存与请求量无关；滑动窗口要么保留样本列表，
//     要么按秒维护大量桶，成本都随流量或时间增长；
//   - 衰减让旧观测的权重自然下降：有效样本数低于门槛后估计标记 Insufficient，
//     调用方回退到中性先验，等价于自然向先验回归，无需显式清理过期条目。
//
// 半衰期 5 分钟；衰减只在记录与读取时按经过时间惰性计算，不启动后台任务。
// 状态按 target 隔离（每 target 一把锁，不用全局锁），模型表上限 maxPerfModels，
// 溢出丢弃并计数；模型键与 modelLimits 同一拼写口径（本次尝试实际转发的上游
// 模型名，见 resolveUpstreamModel/ensureModelAllowed）。
package proxy

import (
	"context"
	"errors"
	"math"
	"sort"
	"sync"
	"time"
)

const (
	// perfDecayHalfLife is the observation half-life: effective sample counts
	// halve every five minutes without new samples.
	perfDecayHalfLife = 5 * time.Minute
	// perfMinSamples is the effective-sample threshold below which an estimate
	// is marked Insufficient and consumers fall back to the neutral prior.
	perfMinSamples = 5
	// perfPriorTTFTMs / perfPriorTPS are the neutral priors used whenever the
	// corresponding estimate is insufficient: a first content event at 1000ms
	// and a 50 tokens/second stream.
	perfPriorTTFTMs = 1000.0
	perfPriorTPS    = 50.0
	// perfReferenceOutputTokens is the reference output length used by
	// predictedCompletionMS (topic of #199) to turn a throughput estimate into
	// an expected completion time.
	perfReferenceOutputTokens = 512
	// perfMinTPSOutputTokens is the smallest final output token count that
	// yields a throughput sample; shorter responses say little about long-run
	// tokens/second.
	perfMinTPSOutputTokens = 32
	// perfCancelPenaltyWait is the wait after which a cancellation/timeout with
	// no content event becomes a TTFT lower-bound penalty sample. Faster
	// cancellations are only counted as fast cancels.
	perfCancelPenaltyWait = 3 * time.Second
	// maxPerfModels bounds the per-target model table; a new model beyond it is
	// dropped and counted.
	maxPerfModels = 32
)

// perfTTFTBoundsMs are exclusive-ish upper bounds of the TTFT histogram, in
// milliseconds. A sample above the last bound lands in the overflow bucket.
var perfTTFTBoundsMs = [...]float64{
	50, 100, 200, 400, 600, 800, 1000, 1500, 2000, 3000, 5000, 8000, 12000, 20000, 30000, 60000,
}

// perfTPSBounds are upper bounds of the tokens/second histogram.
var perfTPSBounds = [...]float64{
	0.5, 1, 2, 3, 5, 7, 10, 15, 20, 30, 50, 75, 100, 150, 200, 400,
}

// PerfEstimate is one read-only percentile summary. Samples is the effective
// (decayed) sample count rounded to an integer.
type PerfEstimate struct {
	P50          float64 `json:"p50"`
	P90          float64 `json:"p90"`
	Samples      int     `json:"samples"`
	Insufficient bool    `json:"insufficient"`
}

// NonStreamPerf is the separate non-streaming response summary: total attempt
// time and output tokens. It is never mixed into streaming TTFT/TPS.
type NonStreamPerf struct {
	Samples         int     `json:"samples"`
	AvgMs           float64 `json:"avg_ms"`
	AvgOutputTokens float64 `json:"avg_output_tokens"`
}

// TargetModelPerf is a read-only snapshot of one (target, upstream model)
// estimate. Percentiles are histogram bucket representatives: TTFT and TPS
// precision is bounded by the bucket width.
type TargetModelPerf struct {
	Target        string `json:"target"`
	UpstreamModel string `json:"upstream_model"`
	// TTFT is the time from attempt start (queueing, key selection, dial) to
	// the first non-empty content event.
	TTFT PerfEstimate `json:"ttft"`
	// TPS is final output tokens over the first-to-last content event window.
	TPS PerfEstimate `json:"tps"`
	// PenaltySamples counts cancellations/timeouts without content after
	// perfCancelPenaltyWait; their wait is recorded as a TTFT lower bound in a
	// separate histogram so it cannot enter the real TTFT percentiles.
	PenaltySamples    int          `json:"penalty_samples"`
	PenaltyTTFT       PerfEstimate `json:"penalty_ttft"`
	FastCancelSamples int          `json:"fast_cancel_samples"`
	// Insufficient is true when either TTFT or TPS has too few effective
	// samples; predictedCompletionMS then uses the neutral prior for that part.
	Insufficient bool          `json:"insufficient"`
	UpdatedAt    time.Time     `json:"updated_at"`
	NonStream    NonStreamPerf `json:"non_stream"`
}

// TargetPerformance is the read-only batch snapshot of one target's estimates.
type TargetPerformance struct {
	Target        string
	Models        []TargetModelPerf
	DroppedModels int64
}

// performanceSample is one completed response's contribution.
type performanceSample struct {
	model        string
	ttft         bool
	ttftMs       float64
	tps          bool
	tpsValue     float64
	penalty      bool
	penaltyMs    float64
	fastCancel   bool
	nonStream    bool
	nonStreamMs  float64
	outputTokens int64
	at           time.Time
}

// perfObservation is the completion evidence of one upstream response collected
// by writeResponse.
type perfObservation struct {
	StartedAt      time.Time
	FinishedAt     time.Time
	Streaming      bool
	ContentSeen    bool
	FirstContentAt time.Time
	LastContentAt  time.Time
	// FinalUsage means the protocol's official final usage was observed.
	FinalUsage   bool
	OutputTokens int64
	CopyErr      error
	// NoUsageCause mirrors responseNoUsageCause: client_gone/timeout settle
	// without content as penalties when the wait exceeded the threshold.
	NoUsageCause string
	Status       int
	// LocalResponse marks a proxy-generated response that never reached the
	// upstream; it is not performance evidence.
	LocalResponse bool
}

// performanceSampleFrom converts one completed response into a sample. ok=false
// means the response proves nothing about upstream generation performance.
func performanceSampleFrom(obs perfObservation) (performanceSample, bool) {
	if obs.LocalResponse {
		return performanceSample{}, false
	}
	sample := performanceSample{at: obs.FinishedAt, outputTokens: obs.OutputTokens}
	if obs.Streaming {
		if obs.ContentSeen {
			sample.ttft = true
			sample.ttftMs = float64(obs.FirstContentAt.Sub(obs.StartedAt)) / float64(time.Millisecond)
			if sample.ttftMs < 0 {
				sample.ttftMs = 0
			}
			window := obs.LastContentAt.Sub(obs.FirstContentAt)
			// An unfinished stream (no official final usage) reports TTFT only:
			// its token count is not the final output.
			if obs.FinalUsage && obs.OutputTokens >= perfMinTPSOutputTokens && window > 0 {
				sample.tps = true
				sample.tpsValue = float64(obs.OutputTokens) / window.Seconds()
			}
			return sample, true
		}
		canceled := errors.Is(obs.CopyErr, context.Canceled) ||
			errors.Is(obs.CopyErr, context.DeadlineExceeded) ||
			obs.NoUsageCause == noUsageCauseClientGone ||
			obs.NoUsageCause == noUsageCauseTimeout
		if !canceled {
			return performanceSample{}, false
		}
		waited := obs.FinishedAt.Sub(obs.StartedAt)
		if waited >= perfCancelPenaltyWait {
			sample.penalty = true
			sample.penaltyMs = float64(waited) / float64(time.Millisecond)
		} else {
			sample.fastCancel = true
		}
		return sample, true
	}
	// Non-streaming: only a successful response with official final usage
	// contributes duration and output tokens.
	if obs.Status < 200 || obs.Status >= 300 || !obs.FinalUsage || obs.OutputTokens <= 0 {
		return performanceSample{}, false
	}
	sample.nonStream = true
	sample.nonStreamMs = float64(obs.FinishedAt.Sub(obs.StartedAt)) / float64(time.Millisecond)
	if sample.nonStreamMs < 0 {
		sample.nonStreamMs = 0
	}
	return sample, true
}

// predictedCompletionMS estimates the completion time of a reference-length
// response from a snapshot: TTFT + perfReferenceOutputTokens / TPS, in
// milliseconds. Insufficient parts use the neutral priors. It is pure and does
// not affect routing.
func predictedCompletionMS(est TargetModelPerf) float64 {
	ttft := perfPriorTTFTMs
	if !est.TTFT.Insufficient && est.TTFT.P50 > 0 {
		ttft = est.TTFT.P50
	}
	tps := perfPriorTPS
	if !est.TPS.Insufficient && est.TPS.P50 > 0 {
		tps = est.TPS.P50
	}
	return ttft + float64(perfReferenceOutputTokens)/tps*1000
}

// decayFactor is 2^(-elapsed/half-life); elapsed <= 0 means no decay.
func decayFactor(elapsed time.Duration) float64 {
	if elapsed <= 0 {
		return 1
	}
	return math.Exp2(-float64(elapsed) / float64(perfDecayHalfLife))
}

// belowDecayFloor drops weights that have decayed to numerical noise.
const belowDecayFloor = 1e-9

// decayedHistogram is a fixed-bucket histogram whose counts decay exponentially.
// bucket i counts samples in (bounds[i-1], bounds[i]]; the last bucket overflows.
type decayedHistogram struct {
	bounds    []float64
	counts    []float64
	total     float64
	updatedAt time.Time
}

func newDecayedHistogram(bounds []float64) *decayedHistogram {
	return &decayedHistogram{bounds: bounds, counts: make([]float64, len(bounds)+1)}
}

func (h *decayedHistogram) decay(now time.Time) {
	if h.updatedAt.IsZero() {
		h.updatedAt = now
		return
	}
	if !now.After(h.updatedAt) {
		return
	}
	factor := decayFactor(now.Sub(h.updatedAt))
	h.updatedAt = now
	if h.total == 0 || factor == 0 {
		return
	}
	h.total *= factor
	if h.total < belowDecayFloor {
		h.total = 0
		for i := range h.counts {
			h.counts[i] = 0
		}
		return
	}
	for i := range h.counts {
		h.counts[i] *= factor
	}
}

func (h *decayedHistogram) observe(at time.Time, value float64) {
	h.decay(at)
	i := sort.SearchFloat64s(h.bounds, value)
	h.counts[i]++
	h.total++
}

// representative is the bucket midpoint; the first bucket starts at 0 and the
// overflow bucket uses its lower bound (conservative for both TTFT and TPS).
func (h *decayedHistogram) representative(i int) float64 {
	lower := 0.0
	if i > 0 {
		lower = h.bounds[i-1]
	}
	if i < len(h.bounds) {
		return (lower + h.bounds[i]) / 2
	}
	return lower
}

// quantilePair returns two percentiles and the effective total. q2 > q1.
func (h *decayedHistogram) quantilePair(now time.Time, q1, q2 float64) (float64, float64, float64) {
	h.decay(now)
	if h.total <= 0 {
		return 0, 0, 0
	}
	limit1, limit2 := h.total*q1, h.total*q2
	var v1, v2 float64
	got1, got2 := false, false
	cumulative := 0.0
	for i, count := range h.counts {
		cumulative += count
		if !got1 && cumulative >= limit1 {
			v1 = h.representative(i)
			got1 = true
		}
		if !got2 && cumulative >= limit2 {
			v2 = h.representative(i)
			got2 = true
			break
		}
	}
	if !got1 {
		v1 = h.representative(len(h.counts) - 1)
	}
	if !got2 {
		v2 = h.representative(len(h.counts) - 1)
	}
	return v1, v2, h.total
}

// decayedAggregate is a single decayed sum (used for counters and non-stream
// totals).
type decayedAggregate struct {
	value     float64
	updatedAt time.Time
}

func (a *decayedAggregate) decay(now time.Time) {
	if a.updatedAt.IsZero() {
		a.updatedAt = now
		return
	}
	if !now.After(a.updatedAt) {
		return
	}
	a.value *= decayFactor(now.Sub(a.updatedAt))
	a.updatedAt = now
	if a.value < belowDecayFloor {
		a.value = 0
	}
}

func (a *decayedAggregate) add(at time.Time, delta float64) {
	a.decay(at)
	a.value += delta
}

func (a *decayedAggregate) get(now time.Time) float64 {
	a.decay(now)
	return a.value
}

// modelPerfState holds one upstream model's estimates for one target.
type modelPerfState struct {
	ttft       *decayedHistogram
	tps        *decayedHistogram
	penalty    *decayedHistogram
	fastCancel decayedAggregate

	nonStreamSamples decayedAggregate
	nonStreamMs      decayedAggregate
	nonStreamTokens  decayedAggregate

	updatedAt time.Time
}

func newModelPerfState() *modelPerfState {
	return &modelPerfState{
		ttft:    newDecayedHistogram(perfTTFTBoundsMs[:]),
		tps:     newDecayedHistogram(perfTPSBounds[:]),
		penalty: newDecayedHistogram(perfTTFTBoundsMs[:]),
	}
}

func sampleCount(total float64) int {
	if total <= 0 {
		return 0
	}
	return int(math.Round(total))
}

func perfEstimate(p50, p90, total float64) PerfEstimate {
	est := PerfEstimate{P50: p50, P90: p90, Samples: sampleCount(total)}
	est.Insufficient = total < perfMinSamples
	return est
}

func (m *modelPerfState) snapshot(now time.Time) TargetModelPerf {
	ttftP50, ttftP90, ttftTotal := m.ttft.quantilePair(now, 0.5, 0.9)
	tpsP50, tpsP90, tpsTotal := m.tps.quantilePair(now, 0.5, 0.9)
	penaltyP50, penaltyP90, penaltyTotal := m.penalty.quantilePair(now, 0.5, 0.9)
	est := TargetModelPerf{
		TTFT:              perfEstimate(ttftP50, ttftP90, ttftTotal),
		TPS:               perfEstimate(tpsP50, tpsP90, tpsTotal),
		PenaltySamples:    sampleCount(penaltyTotal),
		PenaltyTTFT:       perfEstimate(penaltyP50, penaltyP90, penaltyTotal),
		FastCancelSamples: sampleCount(m.fastCancel.get(now)),
		UpdatedAt:         m.updatedAt,
	}
	est.Insufficient = est.TTFT.Insufficient || est.TPS.Insufficient
	if samples := m.nonStreamSamples.get(now); samples > 0 {
		est.NonStream = NonStreamPerf{
			Samples:         sampleCount(samples),
			AvgMs:           m.nonStreamMs.get(now) / samples,
			AvgOutputTokens: m.nonStreamTokens.get(now) / samples,
		}
	}
	return est
}

// targetPerformance is one target's performance state. Its own mutex keeps the
// hot path off any global lock; the model table is bounded.
type targetPerformance struct {
	mu      sync.Mutex
	models  map[string]*modelPerfState
	dropped int64
}

func newTargetPerformance() *targetPerformance {
	return &targetPerformance{models: make(map[string]*modelPerfState, 8)}
}

func (p *targetPerformance) observe(sample performanceSample) {
	at := sample.at
	if at.IsZero() {
		at = time.Now()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	model := p.models[sample.model]
	if model == nil {
		if len(p.models) >= maxPerfModels {
			p.dropped++
			return
		}
		model = newModelPerfState()
		p.models[sample.model] = model
	}
	if sample.ttft {
		model.ttft.observe(at, sample.ttftMs)
	}
	if sample.tps {
		model.tps.observe(at, sample.tpsValue)
	}
	if sample.penalty {
		model.penalty.observe(at, sample.penaltyMs)
	}
	if sample.fastCancel {
		model.fastCancel.add(at, 1)
	}
	if sample.nonStream {
		model.nonStreamSamples.add(at, 1)
		model.nonStreamMs.add(at, sample.nonStreamMs)
		model.nonStreamTokens.add(at, float64(sample.outputTokens))
	}
	model.updatedAt = at
}

// snapshot returns copies sorted by model name.
func (p *targetPerformance) snapshot(now time.Time, target string) []TargetModelPerf {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.models) == 0 {
		return nil
	}
	out := make([]TargetModelPerf, 0, len(p.models))
	for model, state := range p.models {
		est := state.snapshot(now)
		est.Target, est.UpstreamModel = target, model
		out = append(out, est)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpstreamModel < out[j].UpstreamModel })
	return out
}

func (p *targetPerformance) estimate(model string, now time.Time, target string) (TargetModelPerf, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.models[model]
	if state == nil {
		return TargetModelPerf{}, false
	}
	est := state.snapshot(now)
	est.Target, est.UpstreamModel = target, model
	return est, true
}

func (p *targetPerformance) droppedCount() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.dropped
}

// performanceStats returns the target's performance state, creating it lazily.
// A nil Load is a normal fresh generation; CompareAndSwap avoids a global lock.
func (s *targetState) performanceStats() *targetPerformance {
	if p := s.perf.Load(); p != nil {
		return p
	}
	fresh := newTargetPerformance()
	if s.perf.CompareAndSwap(nil, fresh) {
		return fresh
	}
	return s.perf.Load()
}

// recordPerformance converts one completed response into estimate samples.
func (s *targetState) recordPerformance(upstreamModel string, obs perfObservation) {
	sample, ok := performanceSampleFrom(obs)
	if !ok {
		return
	}
	sample.model = upstreamModel
	s.performanceStats().observe(sample)
}

func (s *targetState) performanceSnapshot(now time.Time) []TargetModelPerf {
	p := s.perf.Load()
	if p == nil {
		return nil
	}
	name := ""
	if s.target != nil {
		name = s.target.Name
	}
	return p.snapshot(now, name)
}

func (s *targetState) performanceEstimate(upstreamModel string, now time.Time) (TargetModelPerf, bool) {
	p := s.perf.Load()
	if p == nil {
		return TargetModelPerf{}, false
	}
	name := ""
	if s.target != nil {
		name = s.target.Name
	}
	return p.estimate(upstreamModel, now, name)
}

// performanceObservationEnabled reports whether completed upstream responses
// feed the per-(target, upstream model) estimates. It is on by default and
// independent of usage persistence: the same observation pipe serves usage,
// traces, request statistics and these estimates.
func (s *Service) performanceObservationEnabled() bool {
	return !s.perfObservationDisabled.Load()
}

// SetPerformanceObservationEnabled toggles performance estimate collection.
func (s *Service) SetPerformanceObservationEnabled(enabled bool) {
	s.perfObservationDisabled.Store(!enabled)
}

// TargetModelPerformance returns the read-only estimate of one (target,
// upstream model) pair. ok=false means no samples were recorded for it.
func (s *Service) TargetModelPerformance(targetName, upstreamModel string, now time.Time) (TargetModelPerf, bool) {
	state, ok := s.targetByName(targetName)
	if !ok || state == nil {
		return TargetModelPerf{}, false
	}
	return state.performanceEstimate(upstreamModel, now)
}

// TargetPerformanceSnapshot returns a read-only batch snapshot of every target
// that has recorded performance samples, sorted by target name.
func (s *Service) TargetPerformanceSnapshot(now time.Time) []TargetPerformance {
	snapshot := s.targetSnapshot()
	out := make([]TargetPerformance, 0, len(snapshot))
	for _, state := range snapshot {
		if state == nil || state.Target() == nil {
			continue
		}
		models := state.performanceSnapshot(now)
		dropped := int64(0)
		if p := state.perf.Load(); p != nil {
			dropped = p.droppedCount()
		}
		if len(models) == 0 && dropped == 0 {
			continue
		}
		out = append(out, TargetPerformance{Target: state.Target().Name, Models: models, DroppedModels: dropped})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Target < out[j].Target })
	return out
}
