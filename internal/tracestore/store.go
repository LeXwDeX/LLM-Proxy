// Package tracestore 提供 DEBUG 模式下的全量请求/响应记录。
//
// 架构：
//   - 内存 Ring Buffer：sync.Map 存储最近 N 条记录，查询 O(1)
//   - 异步落盘：channel + goroutine 非阻塞写入磁盘
//   - 磁盘回退：内存 miss 时自动从磁盘文件查询（当前 + 备份）
//   - 仅 DEBUG 模式启用，生产环境零开销
package tracestore

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/natefinch/lumberjack.v2"
)

// TraceRecord 记录单个请求的完整 META 信息和内容。
type TraceRecord struct {
	// === META: 请求侧 ===
	TraceID         string            `json:"trace_id"`
	Timestamp       time.Time         `json:"timestamp"`
	ClientName      string            `json:"client_name"`
	ClientIP        string            `json:"client_ip"`
	ClientAccessKey string            `json:"client_access_key"` // 脱敏
	Method          string            `json:"method"`
	Path            string            `json:"path"`
	QueryParams     string            `json:"query_params"`
	RequestHeaders  map[string]string `json:"request_headers"` // 脱敏

	// === META: 路由决策 ===
	Target        string `json:"target"`
	EndpointType  string `json:"endpoint_type"`
	Model         string `json:"model"`
	SelectionKind string `json:"selection_kind"`
	KeyIndex      int    `json:"key_index"`
	KeyMask       string `json:"key_mask"`

	// === META: 上游 ===
	UpstreamURL       string            `json:"upstream_url"`
	UpstreamRequestID string            `json:"upstream_request_id"`
	UpstreamStatus    int               `json:"upstream_status"`
	UpstreamHeaders   map[string]string `json:"upstream_headers"`

	// === META: 结果 ===
	StatusCode          int   `json:"status_code"`
	DurationMS          int64 `json:"duration_ms"`
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	CachedTokens        int64 `json:"cached_tokens"`
	CacheCreationTokens int64 `json:"cache_creation_tokens"`

	// === 内容 ===
	RequestBody  string `json:"request_body"`
	ResponseBody string `json:"response_body"`
}

// Config 配置 TraceStore。
type Config struct {
	Enabled        bool   `json:"enabled"`
	RingBufferSize int    `json:"ring_buffer_size"`
	MaxBodySize    int    `json:"max_body_size"`
	DiskPath       string `json:"disk_path"`
	DiskMaxSizeMB  int    `json:"disk_max_size_mb"`
	DiskMaxBackups int    `json:"disk_max_backups"`
	DiskTTLHours   int    `json:"disk_ttl_hours"`
	ChannelBuffer  int    `json:"channel_buffer"`
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{
		Enabled:        false,
		RingBufferSize: 1000,
		MaxBodySize:    512 * 1024, // 512KB
		DiskPath:       "/var/lib/llms-proxy/trace.log",
		DiskMaxSizeMB:  500, // 500MB per file
		DiskMaxBackups: 10,  // 10 files = 5GB total
		DiskTTLHours:   120, // 5 days
		ChannelBuffer:  500,
	}
}

// Store 管理 trace 记录的内存和磁盘存储。
type Store struct {
	cfg    Config
	logger *slog.Logger

	// 内存 Ring Buffer
	ring sync.Map // map[string]*TraceRecord
	keys []string // 环形队列的 key 顺序
	head int      // 环形队列头指针
	mu   sync.Mutex

	// 异步落盘
	ch   chan *TraceRecord
	done chan struct{}

	// 磁盘 writer
	diskWriter io.Writer
	diskCloser io.Closer

	// 统计
	totalRecords   atomic.Int64
	droppedRecords atomic.Int64
	diskWrites     atomic.Int64
	diskReads      atomic.Int64

	// 关闭保护
	closeOnce sync.Once
	closed    atomic.Bool
	lifecycle sync.RWMutex // Record 持读锁发送，Close 持写锁关闭队列
}

// New 创建 TraceStore。如果 cfg.Enabled == false，返回 noop store。
func New(cfg Config, logger *slog.Logger) *Store {
	if !cfg.Enabled {
		return &Store{cfg: cfg, logger: logger}
	}

	if logger == nil {
		logger = slog.Default()
	}

	s := &Store{
		cfg:    cfg,
		logger: logger,
		keys:   make([]string, cfg.RingBufferSize),
		ch:     make(chan *TraceRecord, cfg.ChannelBuffer),
		done:   make(chan struct{}),
	}

	// 初始化磁盘 writer
	if cfg.DiskPath != "" {
		s.initDiskWriter()
	}

	// 启动异步落盘 goroutine
	go s.diskWriterLoop()

	logger.Info("trace store enabled",
		"ring_buffer_size", cfg.RingBufferSize,
		"disk_path", cfg.DiskPath,
		"max_body_size", cfg.MaxBodySize,
	)

	return s
}

// initDiskWriter 初始化磁盘 writer（lumberjack 轮转）。
func (s *Store) initDiskWriter() {
	dir := filepath.Dir(s.cfg.DiskPath)
	if dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			s.logger.Warn("trace store: mkdir failed, disk write disabled", "path", dir, "error", err)
			return
		}
	}

	rolling := &lumberjack.Logger{
		Filename:   s.cfg.DiskPath,
		MaxSize:    s.cfg.DiskMaxSizeMB,
		MaxBackups: s.cfg.DiskMaxBackups,
		MaxAge:     max(s.cfg.DiskTTLHours/24, 1), // 至少保留 1 天，避免 <24h 时 MaxAge=0 导致不按时间轮转
		Compress:   false,                         // 不压缩，保留明文 NDJSON 方便 grep/jq 查询历史数据
	}

	s.diskWriter = rolling
	s.diskCloser = rolling
	s.logger.Info("trace store: disk writer initialized", "path", s.cfg.DiskPath)
}

// diskWriterLoop 异步落盘 goroutine。
func (s *Store) diskWriterLoop() {
	defer close(s.done)

	for record := range s.ch {
		if s.diskWriter == nil {
			continue
		}

		line, err := json.Marshal(record)
		if err != nil {
			s.logger.Warn("trace store: marshal failed", "trace_id", record.TraceID, "error", err)
			continue
		}

		line = append(line, '\n')
		if _, err := s.diskWriter.Write(line); err != nil {
			s.logger.Warn("trace store: disk write failed", "trace_id", record.TraceID, "error", err)
			continue
		}

		s.diskWrites.Add(1)
	}
}

// Record 记录一条 trace。非阻塞，channel 满时丢弃。
func (s *Store) Record(record *TraceRecord) {
	s.lifecycle.RLock()
	defer s.lifecycle.RUnlock()
	if !s.cfg.Enabled || record == nil || s.closed.Load() {
		return
	}

	// 截断后复制，避免短字符串仍持有整个大请求/响应的底层存储。
	if s.cfg.MaxBodySize > 0 {
		if len(record.RequestBody) > s.cfg.MaxBodySize {
			record.RequestBody = strings.Clone(record.RequestBody[:s.cfg.MaxBodySize])
		}
		if len(record.ResponseBody) > s.cfg.MaxBodySize {
			record.ResponseBody = strings.Clone(record.ResponseBody[:s.cfg.MaxBodySize])
		}
	}

	// 写入内存 Ring Buffer
	s.ring.Store(record.TraceID, record)

	s.mu.Lock()
	// 如果环形队列已满，删除最旧的记录
	if s.keys[s.head] != "" {
		s.ring.Delete(s.keys[s.head])
	}
	s.keys[s.head] = record.TraceID
	s.head = (s.head + 1) % len(s.keys)
	s.mu.Unlock()

	s.totalRecords.Add(1)

	// 异步落盘（非阻塞）
	select {
	case s.ch <- record:
	default:
		s.droppedRecords.Add(1)
	}
}

// Get 按 trace_id 查询记录。先查内存，miss 时回退到磁盘文件。
func (s *Store) Get(traceID string) *TraceRecord {
	if !s.cfg.Enabled {
		return nil
	}

	// 先查内存
	if v, ok := s.ring.Load(traceID); ok {
		return v.(*TraceRecord)
	}

	// 内存 miss，回退到磁盘
	return s.getFromDisk(traceID)
}

// getFromDisk 从磁盘文件读取指定 trace_id 的记录。
func (s *Store) getFromDisk(traceID string) *TraceRecord {
	if s.cfg.DiskPath == "" {
		return nil
	}

	s.diskReads.Add(1)

	// 构造精确匹配模式，避免 body 内容误匹配
	matchPattern := []byte(`"trace_id":"` + traceID + `"`)

	// 读取当前文件和所有备份文件
	files := s.getTraceFiles()
	for _, file := range files {
		if record := s.searchInFile(file, traceID, matchPattern); record != nil {
			return record
		}
	}
	return nil
}

// traceFileEntry 用于排序备份文件。
type traceFileEntry struct {
	path    string
	current bool      // 是否为当前活跃文件（永远排最前）
	ts      time.Time // lumberjack 备份文件名中编码的轮转时间戳
}

// lumberjackBackupTimeFormat 是 lumberjack 备份文件名中使用的时间戳格式。
// 备份文件名形如 trace-2026-05-28T08-38-19.357.log。
const lumberjackBackupTimeFormat = "2006-01-02T15-04-05.000"

// getTraceFiles 返回所有 trace 文件路径（当前文件 + 备份文件，按时间倒序：越新越先）。
//
// 注意：lumberjack 的备份文件名格式是 <prefix>-<timestamp><ext>
// （如 trace-2026-05-28T08-38-19.357.log），而非 trace.log.1 这类数字后缀。
// 早期实现按数字后缀匹配，导致永远找不到任何备份文件，磁盘回退查询只能搜到当前
// trace.log，凡是已轮转进备份的 trace_id 全部 "读不到硬盘"。此处按 lumberjack
// 实际命名解析。
func (s *Store) getTraceFiles() []string {
	dir := filepath.Dir(s.cfg.DiskPath)
	base := filepath.Base(s.cfg.DiskPath)
	ext := filepath.Ext(base)           // ".log"
	prefix := base[:len(base)-len(ext)] // "trace"
	backupPrefix := prefix + "-"        // "trace-"

	var entries []traceFileEntry

	// 当前文件（永远最新）
	if _, err := os.Stat(s.cfg.DiskPath); err == nil {
		entries = append(entries, traceFileEntry{path: s.cfg.DiskPath, current: true})
	}

	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("trace store: failed to read trace directory", "path", dir, "error", err)
		}
		// 至少返回当前文件
		var files []string
		for _, e := range entries {
			files = append(files, e.path)
		}
		return files
	}

	for _, de := range dirEntries {
		name := de.Name()
		if name == base || !strings.HasPrefix(name, backupPrefix) || !strings.HasSuffix(name, ext) {
			continue
		}
		// 提取中间的时间戳：trace-<timestamp>.log → <timestamp>
		tsStr := name[len(backupPrefix) : len(name)-len(ext)]
		ts, err := time.Parse(lumberjackBackupTimeFormat, tsStr)
		if err != nil {
			continue // 跳过无法解析时间戳的文件（如 .gz 压缩备份）
		}
		entries = append(entries, traceFileEntry{
			path: filepath.Join(dir, name),
			ts:   ts,
		})
	}

	// 排序：当前文件最先，其余按时间戳倒序（越新越先）
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].current != entries[j].current {
			return entries[i].current
		}
		return entries[i].ts.After(entries[j].ts)
	})

	files := make([]string, len(entries))
	for i, e := range entries {
		files[i] = e.path
	}
	return files
}

// searchInFile 在指定文件中搜索 trace_id。
func (s *Store) searchInFile(filePath, traceID string, matchPattern []byte) *TraceRecord {
	f, err := os.Open(filePath)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("trace store: failed to open file for search", "path", filePath, "error", err)
		}
		return nil
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	for {
		// JSON escaping can expand each body byte sixfold. Read one NDJSON
		// record without Scanner's fixed token ceiling, which otherwise also
		// prevents reading every later record in the same file.
		line, err := reader.ReadBytes('\n')
		if err != nil && err != io.EOF {
			if s.logger != nil {
				s.logger.Warn("trace store: file read error", "path", filePath, "error", err)
			}
			return nil
		}
		if len(line) == 0 {
			break
		}
		// 精确匹配 "trace_id":"xxx"，避免 body 内容误匹配
		if !bytes.Contains(line, matchPattern) {
			continue
		}

		// 完整解析
		var record TraceRecord
		if err := json.Unmarshal(line, &record); err != nil {
			continue
		}
		if record.TraceID == traceID {
			return &record
		}
	}

	return nil
}

// List 列出最近 N 条记录（按时间倒序），支持 offset 分页。
// offset=0, limit=50 → 最新 50 条
// offset=50, limit=50 → 第 51-100 条（更早的记录）
//
// 内存 ring buffer 与磁盘文件可能有重叠（刚落盘的记录同时在两边）。
// 为保证分页正确性，统一从两个数据源各收集 offset+limit 条候选，
// 按 TraceID 去重、按时间倒序排序后再分页。
func (s *Store) List(offset, limit int) []*TraceRecord {
	if !s.cfg.Enabled || limit <= 0 {
		return nil
	}
	if offset < 0 {
		offset = 0
	}

	totalNeeded := offset + limit

	// 第一步：在锁内收集内存记录的 key（倒序，从最新开始）
	s.mu.Lock()
	var memKeys []string
	n := len(s.keys)
	start := (s.head - 1 + n) % n
	for i := 0; i < totalNeeded && i < n; i++ {
		idx := (start - i + n) % n
		key := s.keys[idx]
		if key == "" {
			break
		}
		memKeys = append(memKeys, key)
	}
	s.mu.Unlock()

	// 从内存 ring buffer 加载记录
	seen := make(map[string]bool, totalNeeded*2)
	var merged []*TraceRecord
	for _, key := range memKeys {
		if v, ok := s.ring.Load(key); ok {
			if rec, ok := v.(*TraceRecord); ok && rec != nil && !seen[rec.TraceID] {
				merged = append(merged, rec)
				seen[rec.TraceID] = true
			}
		}
	}

	// 如果内存候选不足 totalNeeded 条，从磁盘补充。
	// 磁盘和内存可能有重叠记录，靠 TraceID 去重。
	// listFromDisk 只负责返回"最新的 candidateCount 条"候选，
	// offset 分页由本函数统一处理，避免跨数据源 offset 错位。
	if len(merged) < totalNeeded {
		diskRecords := s.listFromDisk(0, totalNeeded)
		for _, r := range diskRecords {
			if r == nil || seen[r.TraceID] {
				continue
			}
			merged = append(merged, r)
			seen[r.TraceID] = true
		}
	}

	// 全局倒序排序（内存 + 磁盘合并后）
	sort.Slice(merged, func(i, j int) bool {
		return merged[i].Timestamp.After(merged[j].Timestamp)
	})

	// 分页
	if offset >= len(merged) {
		return nil
	}
	end := offset + limit
	if end > len(merged) {
		end = len(merged)
	}
	return merged[offset:end]
}

// listFromDisk 从磁盘文件读取最新的 candidateCount 条记录（按时间倒序）。
// 本函数只负责收集候选，不做 offset 分页——offset 由调用方统一处理，
// 因为内存与磁盘是独立数据源，跨源 offset 会因记录重叠而错位。
func (s *Store) listFromDisk(_, candidateCount int) []*TraceRecord {
	if s.cfg.DiskPath == "" || candidateCount <= 0 {
		return nil
	}

	s.diskReads.Add(1)

	files := s.getTraceFiles()
	var allRecords []*TraceRecord

	for _, file := range files {
		records := s.readLastNFromFile(file, candidateCount)
		allRecords = append(allRecords, records...)
	}

	// 按时间倒序排序
	sort.Slice(allRecords, func(i, j int) bool {
		return allRecords[i].Timestamp.After(allRecords[j].Timestamp)
	})

	// 只保留最新的 candidateCount 条（跨文件合并后截断）
	if len(allRecords) > candidateCount {
		allRecords = allRecords[:candidateCount]
	}
	return allRecords
}

// readLastNFromFile 从文件末尾读取最近 N 条记录。
// 使用环形缓冲区，只保留最后 N 行，避免将整个文件读入内存。
func (s *Store) readLastNFromFile(filePath string, n int) []*TraceRecord {
	f, err := os.Open(filePath)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("trace store: failed to open file for list", "path", filePath, "error", err)
		}
		return nil
	}
	defer f.Close()

	// 环形缓冲区：只保留最后 N 行
	ring := make([]string, n)
	ringHead := 0
	ringCount := 0

	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			if s.logger != nil {
				s.logger.Warn("trace store: file read error in readLastN", "path", filePath, "error", err)
			}
			break
		}
		if len(line) == 0 {
			break
		}
		ring[ringHead] = line
		ringHead = (ringHead + 1) % n
		if ringCount < n {
			ringCount++
		}
	}

	// 从环形缓冲区提取记录（按写入顺序）
	var records []*TraceRecord
	start := (ringHead - ringCount + n) % n
	for i := 0; i < ringCount; i++ {
		idx := (start + i) % n
		line := ring[idx]
		if line == "" {
			continue
		}
		var record TraceRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			continue
		}
		records = append(records, &record)
	}
	return records
}

// Stats 返回统计信息。
func (s *Store) Stats() map[string]int64 {
	return map[string]int64{
		"total_records":    s.totalRecords.Load(),
		"dropped_records":  s.droppedRecords.Load(),
		"disk_writes":      s.diskWrites.Load(),
		"disk_reads":       s.diskReads.Load(),
		"ring_buffer_size": int64(s.cfg.RingBufferSize),
	}
}

// Enabled 报告 Record 是否会保存记录：nil、未启用或已关闭的 store 返回 false。
// 调用方据此在关闭时跳过请求/响应体的采集与复制。
func (s *Store) Enabled() bool {
	return s != nil && s.cfg.Enabled && !s.closed.Load()
}

// MaxBodySize 返回配置的最大 body 大小。
func (s *Store) MaxBodySize() int {
	return s.cfg.MaxBodySize
}

// Close 关闭 store，等待异步落盘完成。可安全多次调用。
func (s *Store) Close() error {
	if !s.cfg.Enabled {
		return nil
	}

	var err error
	s.closeOnce.Do(func() {
		// 等待已经进入的 Record 完成发送，再关闭队列并排空落盘。
		s.lifecycle.Lock()
		s.closed.Store(true)
		close(s.ch)
		s.lifecycle.Unlock()
		<-s.done

		if s.diskCloser != nil {
			err = s.diskCloser.Close()
		}
	})
	return err
}
