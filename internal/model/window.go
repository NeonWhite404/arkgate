package model

import (
	"sync"
	"time"
)

// Window 是一个滑动的速率/用量窗口，用于限流（RPM/TPM）。
// 它记录一段时间内的（时间戳, 数值）样本序列，读取时惰性淘汰过期样本。
// value 单位视用途而定：RPM 里每次插入 1，TPM 里插入 token 数。
type Window struct {
	mu      sync.Mutex
	span    time.Duration
	entries []entry
}

type entry struct {
	ts  time.Time
	val int64
}

func NewWindow(span time.Duration) *Window {
	return &Window{span: span}
}

func (w *Window) Add(val int64) {
	now := time.Now()
	w.mu.Lock()
	w.pruneLocked(now)
	w.entries = append(w.entries, entry{ts: now, val: val})
	w.mu.Unlock()
}

// Count 返回窗口内样本个数。
func (w *Window) Count() int64 { return w.sum(false) }

// Sum 返回窗口内样本数值之和。
func (w *Window) Sum() int64 { return w.sum(true) }

func (w *Window) sum(useVal bool) int64 {
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pruneLocked(now)
	var n int64
	for _, e := range w.entries {
		if useVal {
			n += e.val
		} else {
			n++
		}
	}
	return n
}

func (w *Window) pruneLocked(now time.Time) {
	cut := now.Add(-w.span)
	i := 0
	for i < len(w.entries) && w.entries[i].ts.Before(cut) {
		i++
	}
	if i > 0 {
		w.entries = append(w.entries[:0:0], w.entries[i:]...)
	}
}
