package tracestore

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStoreDisabled(t *testing.T) {
	cfg := Config{Enabled: false}
	store := New(cfg, nil)

	// Record should be no-op
	store.Record(&TraceRecord{TraceID: "test-1"})

	// Get should return nil
	if record := store.Get("test-1"); record != nil {
		t.Errorf("expected nil from disabled store, got %+v", record)
	}

	// List should return nil
	if records := store.List(0, 10); records != nil {
		t.Errorf("expected nil from disabled store, got %d records", len(records))
	}

	// Stats should return zeros
	stats := store.Stats()
	if stats["total_records"] != 0 {
		t.Errorf("expected 0 total_records, got %d", stats["total_records"])
	}
}

func TestStoreRingBuffer(t *testing.T) {
	cfg := Config{
		Enabled:        true,
		RingBufferSize: 3,
		MaxBodySize:    1024,
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)
	defer store.Close()

	// Record 5 entries (ring buffer size is 3)
	for i := 0; i < 5; i++ {
		store.Record(&TraceRecord{
			TraceID:   "test-" + string(rune('0'+i)),
			Timestamp: time.Now(),
		})
	}

	// Wait for async processing
	time.Sleep(100 * time.Millisecond)

	// First 2 entries should be evicted
	if record := store.Get("test-0"); record != nil {
		t.Errorf("expected test-0 to be evicted, got %+v", record)
	}
	if record := store.Get("test-1"); record != nil {
		t.Errorf("expected test-1 to be evicted, got %+v", record)
	}

	// Last 3 entries should be present
	for i := 2; i < 5; i++ {
		id := "test-" + string(rune('0'+i))
		if record := store.Get(id); record == nil {
			t.Errorf("expected %s to be present", id)
		}
	}

	// Stats should show 5 total records
	stats := store.Stats()
	if stats["total_records"] != 5 {
		t.Errorf("expected 5 total_records, got %d", stats["total_records"])
	}
}

func TestStoreList(t *testing.T) {
	cfg := Config{
		Enabled:        true,
		RingBufferSize: 10,
		MaxBodySize:    1024,
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)
	defer store.Close()

	// Record 5 entries
	for i := 0; i < 5; i++ {
		store.Record(&TraceRecord{
			TraceID:   "test-" + string(rune('0'+i)),
			Timestamp: time.Now(),
		})
	}

	// Wait for async processing
	time.Sleep(100 * time.Millisecond)

	// List should return entries in reverse order (newest first)
	records := store.List(0, 3)
	if len(records) != 3 {
		t.Errorf("expected 3 records, got %d", len(records))
	}

	// First record should be the newest (test-4)
	if records[0].TraceID != "test-4" {
		t.Errorf("expected first record to be test-4, got %s", records[0].TraceID)
	}
}

func TestStoreBodyTruncation(t *testing.T) {
	cfg := Config{
		Enabled:        true,
		RingBufferSize: 10,
		MaxBodySize:    10, // Very small limit
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)
	defer store.Close()

	// Record with large body
	largeBody := strings.Repeat("x", 100)

	store.Record(&TraceRecord{
		TraceID:      "test-large",
		Timestamp:    time.Now(),
		RequestBody:  largeBody,
		ResponseBody: largeBody,
	})

	// Wait for async processing
	time.Sleep(100 * time.Millisecond)

	// Body should be truncated
	record := store.Get("test-large")
	if record == nil {
		t.Fatal("expected record to be present")
	}

	if len(record.RequestBody) != 10 {
		t.Errorf("expected RequestBody to be truncated to 10 bytes, got %d", len(record.RequestBody))
	}
	if len(record.ResponseBody) != 10 {
		t.Errorf("expected ResponseBody to be truncated to 10 bytes, got %d", len(record.ResponseBody))
	}
}

func TestStoreChannelDrop(t *testing.T) {
	cfg := Config{
		Enabled:        true,
		RingBufferSize: 100,
		MaxBodySize:    1024,
		ChannelBuffer:  1, // Very small buffer
	}
	store := New(cfg, nil)
	defer store.Close()

	// Rapidly record many entries to overflow channel
	for i := 0; i < 100; i++ {
		store.Record(&TraceRecord{
			TraceID:   "test-" + string(rune('0'+i%10)),
			Timestamp: time.Now(),
		})
	}

	// Wait for processing
	time.Sleep(200 * time.Millisecond)

	// Stats should show some dropped records
	stats := store.Stats()
	if stats["dropped_records"] == 0 {
		t.Log("warning: no dropped records detected (may be timing-dependent)")
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg.Enabled {
		t.Error("expected Enabled to be false by default")
	}
	if cfg.RingBufferSize != 1000 {
		t.Errorf("expected RingBufferSize 1000, got %d", cfg.RingBufferSize)
	}
	if cfg.MaxBodySize != 512*1024 {
		t.Errorf("expected MaxBodySize 512KB, got %d", cfg.MaxBodySize)
	}
	if cfg.DiskPath != "/var/lib/llms-proxy/trace.log" {
		t.Errorf("expected default DiskPath, got %s", cfg.DiskPath)
	}
	if cfg.DiskMaxSizeMB != 500 {
		t.Errorf("expected DiskMaxSizeMB 500, got %d", cfg.DiskMaxSizeMB)
	}
	if cfg.DiskMaxBackups != 10 {
		t.Errorf("expected DiskMaxBackups 10, got %d", cfg.DiskMaxBackups)
	}
	if cfg.DiskTTLHours != 120 {
		t.Errorf("expected DiskTTLHours 120 (5 days), got %d", cfg.DiskTTLHours)
	}
	if cfg.ChannelBuffer != 500 {
		t.Errorf("expected ChannelBuffer 500, got %d", cfg.ChannelBuffer)
	}
}

func TestStoreDiskWrite(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "tracestore-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	diskPath := filepath.Join(tmpDir, "trace.log")

	cfg := Config{
		Enabled:        true,
		RingBufferSize: 10,
		MaxBodySize:    1024,
		DiskPath:       diskPath,
		DiskMaxSizeMB:  10,
		DiskTTLHours:   1,
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)

	// Record 5 entries
	for i := 0; i < 5; i++ {
		store.Record(&TraceRecord{
			TraceID:   "disk-test-" + string(rune('0'+i)),
			Timestamp: time.Now(),
			Model:     "test-model",
		})
	}

	// Close to flush disk writes
	store.Close()

	// Verify disk file exists and has content
	info, err := os.Stat(diskPath)
	if err != nil {
		t.Fatalf("expected disk file to exist: %v", err)
	}
	if info.Size() == 0 {
		t.Error("expected disk file to have content")
	}

	// Stats should show disk writes
	stats := store.Stats()
	if stats["disk_writes"] != 5 {
		t.Errorf("expected 5 disk_writes, got %d", stats["disk_writes"])
	}
}

func TestStoreConcurrent(t *testing.T) {
	cfg := Config{
		Enabled:        true,
		RingBufferSize: 100,
		MaxBodySize:    1024,
		ChannelBuffer:  100,
	}
	store := New(cfg, nil)
	defer store.Close()

	var wg sync.WaitGroup
	numGoroutines := 10
	numRecordsPerGoroutine := 100

	// Concurrent writes
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for j := 0; j < numRecordsPerGoroutine; j++ {
				store.Record(&TraceRecord{
					TraceID:   "concurrent-" + string(rune('0'+id)) + "-" + string(rune('0'+j%10)),
					Timestamp: time.Now(),
				})
			}
		}(i)
	}

	// Concurrent reads
	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < numRecordsPerGoroutine; j++ {
				store.Get("concurrent-0-0")
store.List(0, 10)
				store.Stats()
			}
		}()
	}

	wg.Wait()

	// Should not panic and stats should be reasonable
	stats := store.Stats()
	totalRecords := stats["total_records"]
	if totalRecords != int64(numGoroutines*numRecordsPerGoroutine) {
		t.Errorf("expected %d total_records, got %d", numGoroutines*numRecordsPerGoroutine, totalRecords)
	}
}

func TestStoreNilRecord(t *testing.T) {
	cfg := Config{
		Enabled:        true,
		RingBufferSize: 10,
		MaxBodySize:    1024,
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)
	defer store.Close()

	// Should not panic
	store.Record(nil)

	// Stats should still be 0
	stats := store.Stats()
	if stats["total_records"] != 0 {
		t.Errorf("expected 0 total_records after nil record, got %d", stats["total_records"])
	}
}

func TestStoreEmptyTraceID(t *testing.T) {
	cfg := Config{
		Enabled:        true,
		RingBufferSize: 10,
		MaxBodySize:    1024,
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)
	defer store.Close()

	// Record with empty trace_id
	store.Record(&TraceRecord{
		TraceID:   "",
		Timestamp: time.Now(),
	})

	time.Sleep(100 * time.Millisecond)

	// Should be stored (empty string is valid key)
	record := store.Get("")
	if record == nil {
		t.Error("expected record with empty trace_id to be stored")
	}
}

func TestStoreListEmpty(t *testing.T) {
	cfg := Config{
		Enabled:        true,
		RingBufferSize: 10,
		MaxBodySize:    1024,
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)
	defer store.Close()

	// List on empty store
	records := store.List(0, 10)
	if len(records) != 0 {
		t.Errorf("expected 0 records from empty store, got %d", len(records))
	}

	// List with limit 0
	records = store.List(0, 0)
	if records != nil {
		t.Errorf("expected nil from List(0, 0), got %v", records)
	}

	// List with negative limit
	records = store.List(0, -1)
	if records != nil {
		t.Errorf("expected nil from List(0, -1), got %v", records)
	}
}

func TestStoreCloseIdempotent(t *testing.T) {
	cfg := Config{
		Enabled:        true,
		RingBufferSize: 10,
		MaxBodySize:    1024,
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)

	// Close multiple times should not panic
	store.Close()
	store.Close()
	store.Close()
}

func TestStoreCloseDisabled(t *testing.T) {
	cfg := Config{Enabled: false}
	store := New(cfg, nil)

	// Close on disabled store should not panic
	err := store.Close()
	if err != nil {
		t.Errorf("expected no error from Close on disabled store, got %v", err)
	}
}

func TestStoreGetFromDisk(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "tracestore-disk-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	diskPath := filepath.Join(tmpDir, "trace.log")

	cfg := Config{
		Enabled:        true,
		RingBufferSize: 2, // 很小的 ring buffer，会很快溢出
		MaxBodySize:    1024,
		DiskPath:       diskPath,
		DiskMaxSizeMB:  10,
		DiskMaxBackups: 3,
		DiskTTLHours:   24,
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)

	// 写入 5 条记录（ring buffer 只能存 2 条，其余只在磁盘）
	for i := 0; i < 5; i++ {
		store.Record(&TraceRecord{
			TraceID:   "disk-test-" + string(rune('0'+i)),
			Timestamp: time.Now(),
			Model:     "test-model",
		})
	}

	// 等待异步落盘
	store.Close()

	// 重新创建 store（模拟重启），ring buffer 为空
	store2 := New(cfg, nil)
	defer store2.Close()

	// 内存中应该没有记录
	if _, ok := store2.ring.Load("disk-test-0"); ok {
		t.Error("expected memory to be empty after restart")
	}

	// 但 Get 应该能从磁盘读取
	record := store2.Get("disk-test-0")
	if record == nil {
		t.Fatal("expected Get to find record from disk")
	}
	if record.TraceID != "disk-test-0" {
		t.Errorf("expected trace_id disk-test-0, got %s", record.TraceID)
	}

	// 查询最后一条记录
	record = store2.Get("disk-test-4")
	if record == nil {
		t.Fatal("expected Get to find last record from disk")
	}
}

func TestStoreListFromDisk(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "tracestore-list-disk-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	diskPath := filepath.Join(tmpDir, "trace.log")

	cfg := Config{
		Enabled:        true,
		RingBufferSize: 3, // 小 ring buffer
		MaxBodySize:    1024,
		DiskPath:       diskPath,
		DiskMaxSizeMB:  10,
		DiskMaxBackups: 3,
		DiskTTLHours:   24,
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)

	// 写入 10 条记录
	for i := 0; i < 10; i++ {
		store.Record(&TraceRecord{
			TraceID:   "list-test-" + string(rune('0'+i)),
			Timestamp: time.Now().Add(time.Duration(i) * time.Second),
			Model:     "test-model",
		})
	}

	// 等待异步落盘
	store.Close()

	// 重新创建 store（模拟重启）
	store2 := New(cfg, nil)
	defer store2.Close()

	// List 应该能从磁盘读取记录
	records := store2.List(0, 5)
	if len(records) != 5 {
		t.Errorf("expected 5 records from List, got %d", len(records))
	}

	// 验证记录按时间倒序
	for i := 1; i < len(records); i++ {
		if records[i].Timestamp.After(records[i-1].Timestamp) {
			t.Errorf("records not sorted by time: %v > %v", records[i].Timestamp, records[i-1].Timestamp)
		}
	}

	// List 更多记录
	records = store2.List(0, 10)
	if len(records) != 10 {
		t.Errorf("expected 10 records from List, got %d", len(records))
	}
}

func TestStoreGetFromDiskWithBackups(t *testing.T) {
	// Create temp directory
	tmpDir, err := os.MkdirTemp("", "tracestore-backup-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	diskPath := filepath.Join(tmpDir, "trace.log")

	// 手动创建当前文件和备份文件
	currentContent := `{"trace_id":"current-1","timestamp":"2026-05-27T10:00:00Z","model":"test"}
{"trace_id":"current-2","timestamp":"2026-05-27T10:01:00Z","model":"test"}
`
	backup1Content := `{"trace_id":"backup1-1","timestamp":"2026-05-27T09:00:00Z","model":"test"}
{"trace_id":"backup1-2","timestamp":"2026-05-27T09:01:00Z","model":"test"}
`

	if err := os.WriteFile(diskPath, []byte(currentContent), 0644); err != nil {
		t.Fatal(err)
	}
	// lumberjack 备份文件命名格式：<prefix>-<timestamp><ext>
	backupPath := filepath.Join(tmpDir, "trace-2026-05-27T09-00-00.000.log")
	if err := os.WriteFile(backupPath, []byte(backup1Content), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := Config{
		Enabled:        true,
		RingBufferSize: 10,
		MaxBodySize:    1024,
		DiskPath:       diskPath,
		DiskMaxSizeMB:  10,
		DiskMaxBackups: 3,
		DiskTTLHours:   24,
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)
	defer store.Close()

	// 查询当前文件中的记录
	record := store.Get("current-1")
	if record == nil {
		t.Fatal("expected to find current-1 from current file")
	}

	// 查询备份文件中的记录
	record = store.Get("backup1-1")
	if record == nil {
		t.Fatal("expected to find backup1-1 from backup file")
	}

	// 查询不存在的记录
	record = store.Get("nonexistent")
	if record != nil {
		t.Error("expected nil for nonexistent record")
	}
}

// TestReadLastNFromFileRingBuffer 验证 readLastNFromFile 使用环形缓冲区，不会将整个文件加载到内存
func TestReadLastNFromFileRingBuffer(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "tracestore-ring-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	filePath := filepath.Join(tmpDir, "large.log")

	// 写入 1000 条记录
	f, err := os.Create(filePath)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		record := TraceRecord{
			TraceID:   fmt.Sprintf("record-%d", i),
			Timestamp: time.Now().Add(time.Duration(i) * time.Second),
			Model:     "test-model",
		}
		data, _ := json.Marshal(record)
		f.Write(data)
		f.Write([]byte("\n"))
	}
	f.Close()

	cfg := Config{
		Enabled:        true,
		RingBufferSize: 10,
		MaxBodySize:    1024,
		DiskPath:       filePath,
		DiskMaxSizeMB:  10,
		DiskMaxBackups: 3,
		DiskTTLHours:   24,
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)
	defer store.Close()

	// 只读取最后 10 条记录
	records := store.readLastNFromFile(filePath, 10)
	if len(records) != 10 {
		t.Errorf("expected 10 records, got %d", len(records))
	}

	// 验证是最后 10 条（record-990 到 record-999）
	if records[0].TraceID != "record-990" {
		t.Errorf("expected first record to be record-990, got %s", records[0].TraceID)
	}
	if records[9].TraceID != "record-999" {
		t.Errorf("expected last record to be record-999, got %s", records[9].TraceID)
	}
}

// TestGetTraceFilesSorted 验证备份文件按 lumberjack 时间戳正确排序（越新越先）
func TestGetTraceFilesSorted(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "tracestore-sort-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	// 创建当前文件和多个 lumberjack 备份文件（乱序创建）
	files := []string{
		"trace.log",
		"trace-2026-05-27T08-00-00.000.log",
		"trace-2026-05-27T10-00-00.000.log",
		"trace-2026-05-27T09-00-00.000.log",
	}
	for _, name := range files {
		f, err := os.Create(filepath.Join(tmpDir, name))
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
	}

	cfg := Config{
		Enabled:        true,
		RingBufferSize: 10,
		MaxBodySize:    1024,
		DiskPath:       filepath.Join(tmpDir, "trace.log"),
		DiskMaxSizeMB:  10,
		DiskMaxBackups: 3,
		DiskTTLHours:   24,
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)
	defer store.Close()

	sortedFiles := store.getTraceFiles()

	// 验证顺序：当前文件最先，其余备份按时间戳倒序（越新越先）
	expected := []string{
		"trace.log",
		"trace-2026-05-27T10-00-00.000.log",
		"trace-2026-05-27T09-00-00.000.log",
		"trace-2026-05-27T08-00-00.000.log",
	}
	if len(sortedFiles) != len(expected) {
		t.Fatalf("expected %d files, got %d", len(expected), len(sortedFiles))
	}

	for i, exp := range expected {
		actual := filepath.Base(sortedFiles[i])
		if actual != exp {
			t.Errorf("position %d: expected %s, got %s", i, exp, actual)
		}
	}
}

// TestSearchInFilePreciseMatch 验证 searchInFile 使用精确匹配，不会误匹配 body 内容
func TestSearchInFilePreciseMatch(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "tracestore-precise-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	filePath := filepath.Join(tmpDir, "trace.log")

	// 写入一条记录，其 body 包含另一个 trace_id 的字符串
	f, err := os.Create(filePath)
	if err != nil {
		t.Fatal(err)
	}

	// 记录 1：trace_id 是 "real-id"，但 body 包含 "fake-id"
	record1 := TraceRecord{
		TraceID:      "real-id",
		Timestamp:    time.Now(),
		Model:        "test-model",
		RequestBody:  `{"message": "This contains fake-id in the body"}`,
		ResponseBody: `{"result": "Also mentions fake-id here"}`,
	}
	data1, _ := json.Marshal(record1)
	f.Write(data1)
	f.Write([]byte("\n"))

	// 记录 2：trace_id 是 "fake-id"
	record2 := TraceRecord{
		TraceID:   "fake-id",
		Timestamp: time.Now(),
		Model:     "test-model",
	}
	data2, _ := json.Marshal(record2)
	f.Write(data2)
	f.Write([]byte("\n"))
	f.Close()

	cfg := Config{
		Enabled:        true,
		RingBufferSize: 10,
		MaxBodySize:    1024,
		DiskPath:       filePath,
		DiskMaxSizeMB:  10,
		DiskMaxBackups: 3,
		DiskTTLHours:   24,
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)
	defer store.Close()

	// 搜索 "fake-id"，应该返回记录 2，而不是记录 1
	matchPattern := []byte(`"trace_id":"fake-id"`)
	record := store.searchInFile(filePath, "fake-id", matchPattern)
	if record == nil {
		t.Fatal("expected to find fake-id")
	}
	if record.TraceID != "fake-id" {
		t.Errorf("expected trace_id to be fake-id, got %s", record.TraceID)
	}

	// 搜索 "real-id"，应该返回记录 1
	matchPattern = []byte(`"trace_id":"real-id"`)
	record = store.searchInFile(filePath, "real-id", matchPattern)
	if record == nil {
		t.Fatal("expected to find real-id")
	}
	if record.TraceID != "real-id" {
		t.Errorf("expected trace_id to be real-id, got %s", record.TraceID)
	}
	// 验证 body 确实包含 "fake-id" 字符串
	if !strings.Contains(record.RequestBody, "fake-id") {
		t.Error("expected request body to contain fake-id")
	}
}

// TestListConcurrentAccess 验证 List() 在磁盘 I/O 期间不阻塞 Record()
func TestListConcurrentAccess(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "tracestore-concurrent-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	filePath := filepath.Join(tmpDir, "trace.log")

	// 预先写入大量记录到磁盘
	f, err := os.Create(filePath)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		record := TraceRecord{
			TraceID:   fmt.Sprintf("disk-record-%d", i),
			Timestamp: time.Now().Add(time.Duration(i) * time.Second),
			Model:     "test-model",
		}
		data, _ := json.Marshal(record)
		f.Write(data)
		f.Write([]byte("\n"))
	}
	f.Close()

	cfg := Config{
		Enabled:        true,
		RingBufferSize: 5, // 很小的内存缓冲区
		MaxBodySize:    1024,
		DiskPath:       filePath,
		DiskMaxSizeMB:  10,
		DiskMaxBackups: 3,
		DiskTTLHours:   24,
		ChannelBuffer:  100,
	}
	store := New(cfg, nil)
	defer store.Close()

	// 启动一个 goroutine 持续调用 List()（会触发磁盘 I/O）
	listDone := make(chan bool)
	go func() {
		for i := 0; i < 10; i++ {
			records := store.List(0, 50)
			if len(records) == 0 {
				t.Error("List() returned empty results")
			}
			time.Sleep(10 * time.Millisecond)
		}
		listDone <- true
	}()

	// 同时持续调用 Record()，验证不会被阻塞
	recordDone := make(chan bool)
	go func() {
		for i := 0; i < 100; i++ {
			store.Record(&TraceRecord{
				TraceID:   fmt.Sprintf("new-record-%d", i),
				Timestamp: time.Now(),
				Model:     "test-model",
			})
			time.Sleep(1 * time.Millisecond)
		}
		recordDone <- true
	}()

	// 等待两个 goroutine 完成
	<-listDone
	<-recordDone

	// 验证 Record() 成功写入了记录
	stats := store.Stats()
	if stats["total_records"] < 100 {
		t.Errorf("expected at least 100 records, got %d", stats["total_records"])
	}
}

// TestStoreListOffset 验证 List 的 offset 分页功能。
func TestStoreListOffset(t *testing.T) {
	cfg := Config{
		Enabled:        true,
		RingBufferSize: 10,
		MaxBodySize:    1024,
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)
	defer store.Close()

	// Record 5 entries with distinct timestamps
	for i := 0; i < 5; i++ {
		store.Record(&TraceRecord{
			TraceID:   "offset-test-" + string(rune('0'+i)),
			Timestamp: time.Now().Add(time.Duration(i) * time.Second),
		})
	}

	// Wait for async processing
	time.Sleep(100 * time.Millisecond)

	// offset=0, limit=5 → should return all 5 records (newest first: 4, 3, 2, 1, 0)
	records := store.List(0, 5)
	if len(records) != 5 {
		t.Fatalf("expected 5 records with offset=0, got %d", len(records))
	}

	// offset=2, limit=3 → should skip newest 2, return records 2, 1, 0 (older)
	records = store.List(2, 3)
	if len(records) != 3 {
		t.Fatalf("expected 3 records with offset=2, got %d", len(records))
	}
	// The first record should be the 3rd newest (offset-test-2)
	if records[0].TraceID != "offset-test-2" {
		t.Errorf("expected first record to be offset-test-2 (3rd newest), got %s", records[0].TraceID)
	}

	// offset=4, limit=1 → should return only the oldest (offset-test-0)
	records = store.List(4, 1)
	if len(records) != 1 {
		t.Fatalf("expected 1 record with offset=4, got %d", len(records))
	}
	if records[0].TraceID != "offset-test-0" {
		t.Errorf("expected record to be offset-test-0 (oldest), got %s", records[0].TraceID)
	}

	// offset=5, limit=1 → beyond available, should return nil/empty
	records = store.List(5, 1)
	if len(records) != 0 {
		t.Errorf("expected 0 records when offset exceeds data, got %d", len(records))
	}

	// offset=10, limit=5 → way beyond, should return nil/empty
	records = store.List(10, 5)
	if len(records) != 0 {
		t.Errorf("expected 0 records when offset way exceeds data, got %d", len(records))
	}

	// negative offset → should be treated as 0
	records = store.List(-1, 2)
	if len(records) != 2 {
		t.Fatalf("expected 2 records with offset=-1 (treated as 0), got %d", len(records))
	}
	if records[0].TraceID != "offset-test-4" {
		t.Errorf("expected first record to be offset-test-4 (newest), got %s", records[0].TraceID)
	}
}

// TestStoreListOffsetFromDisk 验证 offset 分页在磁盘回退模式下正常工作。
func TestStoreListOffsetFromDisk(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "tracestore-offset-disk-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	diskPath := filepath.Join(tmpDir, "trace.log")

	cfg := Config{
		Enabled:        true,
		RingBufferSize: 3, // 小 ring buffer，记录会溢出到磁盘
		MaxBodySize:    1024,
		DiskPath:       diskPath,
		DiskMaxSizeMB:  10,
		DiskMaxBackups: 3,
		DiskTTLHours:   24,
		ChannelBuffer:  10,
	}
	store := New(cfg, nil)

	// 写入 10 条记录（ring buffer 只存 3 条，其余在磁盘）
	for i := 0; i < 10; i++ {
		store.Record(&TraceRecord{
			TraceID:   "offset-disk-" + string(rune('0'+i)),
			Timestamp: time.Now().Add(time.Duration(i) * time.Second),
			Model:     "test-model",
		})
	}

	// 等待异步落盘
	store.Close()

	// 重新创建 store（模拟重启，ring buffer 为空）
	store2 := New(cfg, nil)
	defer store2.Close()

	// offset=0, limit=5 → 最新的 5 条（offset-disk-9, 8, 7, 6, 5）
	records := store2.List(0, 5)
	if len(records) != 5 {
		t.Fatalf("expected 5 records with offset=0 from disk, got %d", len(records))
	}
	if records[0].TraceID != "offset-disk-9" {
		t.Errorf("expected first record to be offset-disk-9, got %s", records[0].TraceID)
	}

	// offset=5, limit=5 → 更早的 5 条（offset-disk-4, 3, 2, 1, 0）
	records = store2.List(5, 5)
	if len(records) != 5 {
		t.Fatalf("expected 5 records with offset=5 from disk, got %d", len(records))
	}
	if records[0].TraceID != "offset-disk-4" {
		t.Errorf("expected first record to be offset-disk-4, got %s", records[0].TraceID)
	}

	// offset=10, limit=5 → beyond data, empty
	records = store2.List(10, 5)
	if len(records) != 0 {
		t.Errorf("expected 0 records with offset=10 from disk, got %d", len(records))
	}
}
