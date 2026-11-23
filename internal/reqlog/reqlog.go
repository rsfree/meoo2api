// Package reqlog 记录每请求一行元数据（不含提示词与响应正文），
// 供面板"运行日志"视图查看，并追加到 data/requests.jsonl。
package reqlog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Entry 是一次请求的元数据。
type Entry struct {
	Time     int64  `json:"t"`
	ID       string `json:"id"`
	Model    string `json:"model"`
	KeyLabel string `json:"key"`
	Account  string `json:"account,omitempty"`
	Stream   bool   `json:"stream"`
	Status   int    `json:"status"`
	TTFB     int64  `json:"ttfb_ms"`
	Dur      int64  `json:"dur_ms"`
	Prompt   int64  `json:"prompt_tokens"`
	Compl    int64  `json:"completion_tokens"`
	Tokens   int64  `json:"total_tokens"`
	IP       string `json:"ip,omitempty"`
	UA       string `json:"ua,omitempty"`
	Err      string `json:"err,omitempty"`
	Note     string `json:"note,omitempty"` // 200 但结果可疑时的说明（如思考耗尽预算）
	Channel  string `json:"ch,omitempty"`   // chat | models | admin
}

const (
	ringSize   = 800
	maxLines   = 20000
	rotateKeep = 8000
)

// Log 是内存环形 + 追加文件的日志。
type Log struct {
	mu    sync.RWMutex
	ring  []Entry
	f     *os.File
	path  string
	lines int
}

// Open 打开日志文件（追加模式 0600）。
func Open(dir string) (*Log, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "requests.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	l := &Log{f: f, path: path, ring: make([]Entry, 0, ringSize)}
	l.lines = countLines(path)
	return l, nil
}

func countLines(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		n++
	}
	return n
}

// Close 关闭文件。
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// Add 写入一条日志；必要时滚动文件。
func (l *Log) Add(e Entry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.ring) >= ringSize {
		copy(l.ring, l.ring[1:])
		l.ring[len(l.ring)-1] = e
	} else {
		l.ring = append(l.ring, e)
	}
	if l.f == nil {
		return
	}
	if buf, err := json.Marshal(e); err == nil {
		_, _ = l.f.Write(append(buf, '\n'))
		l.lines++
	}
	if l.lines > maxLines {
		l.rotateLocked()
	}
}

func (l *Log) rotateLocked() {
	if l.f == nil {
		return
	}
	// 保留末尾 rotateKeep 行，原子替换。
	keep := l.ring
	if len(keep) > rotateKeep {
		keep = keep[len(keep)-rotateKeep:]
	}
	_ = l.f.Close()
	tmp := l.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		l.f = nil
		return
	}
	w := bufio.NewWriter(f)
	for _, e := range keep {
		if buf, err := json.Marshal(e); err == nil {
			_, _ = w.Write(append(buf, '\n'))
		}
	}
	_ = w.Flush()
	_ = f.Sync()
	_ = f.Close()
	if err := os.Rename(tmp, l.path); err != nil {
		l.f = nil
		return
	}
	nf, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err == nil {
		l.f = nf
		l.lines = len(keep)
	}
}

// Filter 是面板筛选条件。
type Filter struct {
	Channel string
	Model   string
	Status  string // ok | err
	Limit   int
}

// Recent 返回最近的日志（新的在前）。
func (l *Log) Recent(f Filter) []Entry {
	l.mu.RLock()
	defer l.mu.RUnlock()
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	out := make([]Entry, 0, limit)
	for i := len(l.ring) - 1; i >= 0 && len(out) < limit; i-- {
		e := l.ring[i]
		if f.Channel != "" && e.Channel != f.Channel {
			continue
		}
		if f.Model != "" && e.Model != f.Model {
			continue
		}
		if f.Status == "ok" && e.Status >= 400 {
			continue
		}
		if f.Status == "err" && e.Status < 400 {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Stats 汇总最近窗口内的成功/失败与耗时。
//
// 499 单独计数：那是客户端自己断开（常见于推理模型长回答超过客户端耐心），
// 既不该算成功（会掩盖真实失败），也不该算网关失败（会冤枉服务）。单列最诚实，
// 成功率的分母只含「网关切完的请求」。
func (l *Log) Stats() map[string]any {
	l.mu.RLock()
	defer l.mu.RUnlock()
	var ok, errs, canceled int
	var sumTTFB, sumDur, tokens int64
	for _, e := range l.ring {
		if e.Channel != "chat" {
			continue
		}
		switch {
		case e.Status == 499:
			canceled++
			continue
		case e.Status >= 400, e.Status == 0:
			// Status == 0 是历史脏数据（旧版本错误分支固定写 0）。
			// 面板的判据是 >=400 为失败，0 会被当成成功，必须在这里兜住。
			errs++
		default:
			ok++
		}
		sumTTFB += e.TTFB
		sumDur += e.Dur
		tokens += e.Tokens
	}
	n := ok + errs
	avgTTFB, avgDur := int64(0), int64(0)
	if n > 0 {
		avgTTFB, avgDur = sumTTFB/int64(n), sumDur/int64(n)
	}
	rate := 0.0
	if n > 0 {
		rate = float64(ok) * 100 / float64(n)
	}
	return map[string]any{
		"chat_ok": ok, "chat_err": errs, "chat_canceled": canceled, "success_rate": rate,
		"avg_ttfb_ms": avgTTFB, "avg_dur_ms": avgDur, "tokens": tokens,
	}
}
