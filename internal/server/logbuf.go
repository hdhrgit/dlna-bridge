package server

import (
	"fmt"
	"log"
	"sync"
	"time"
)

// LogLine is one line of the log kept for the settings page.
type LogLine struct {
	Seq  int64     `json:"seq"`
	Time time.Time `json:"t"`
	Text string    `json:"text"`
}

// LogBuffer keeps the last lines logged, as well as writing them to the log.
type LogBuffer struct {
	mu    sync.Mutex
	max   int
	seq   int64
	lines []LogLine
}

func NewLogBuffer(max int) *LogBuffer { return &LogBuffer{max: max} }

func (b *LogBuffer) Logf(format string, a ...any) {
	log.Printf(format, a...)
	text := fmt.Sprintf(format, a...)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	b.lines = append(b.lines, LogLine{Seq: b.seq, Time: time.Now(), Text: text})
	if len(b.lines) > b.max {
		b.lines = append(b.lines[:0], b.lines[len(b.lines)-b.max:]...)
	}
}

// Since returns the lines after seq (all of them for 0), and the last seq.
func (b *LogBuffer) Since(seq int64) ([]LogLine, int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	i := 0
	for i < len(b.lines) && b.lines[i].Seq <= seq {
		i++
	}
	return append([]LogLine(nil), b.lines[i:]...), b.seq
}
