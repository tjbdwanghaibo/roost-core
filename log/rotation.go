package log

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/metrics"
)

// rotateRetryInterval 是打开新分片失败之后到下一次重试的间隔。失败期间继续写当前分片，
// 不在每一行上重试 MkdirAll + Open（RR-20261005-NC-263）。
const rotateRetryInterval = time.Second

type timeRotatingFileOptions struct {
	Dir        string
	Filename   string
	Interval   time.Duration
	TimeFormat string
	NowFunc    func() time.Time
}

type timeRotatingFileWriter struct {
	mu         sync.Mutex
	dir        string
	filename   string
	interval   time.Duration
	timeFormat string
	nowFunc    func() time.Time

	slice string
	file  *os.File
	done  bool
	// retryAt 非零时，打开新分片失败过：在此之前继续写当前分片、不重试。
	retryAt time.Time
}

func newTimeRotatingFileWriter(opts timeRotatingFileOptions) (*timeRotatingFileWriter, error) {
	if opts.Interval <= 0 {
		return nil, errors.New("log rotate interval must be positive")
	}
	if opts.NowFunc == nil {
		opts.NowFunc = time.Now
	}
	if opts.TimeFormat == "" {
		opts.TimeFormat = defaultRotateTimeFormat(opts.Interval)
	}
	w := &timeRotatingFileWriter{
		dir:        opts.Dir,
		filename:   opts.Filename,
		interval:   opts.Interval,
		timeFormat: opts.TimeFormat,
		nowFunc:    opts.NowFunc,
	}
	if err := w.rotateLocked(opts.NowFunc()); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *timeRotatingFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.done {
		return 0, os.ErrClosed
	}
	if err := w.rotateLocked(w.nowFunc()); err != nil && w.file == nil {
		return 0, err
	}
	// 新分片打不开时 rotateLocked 保留了当前分片：这一行照常写进去，不丢（RR-20261005-NC-263）。
	return w.file.Write(p)
}

func (w *timeRotatingFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.done = true
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

func (w *timeRotatingFileWriter) rotateLocked(now time.Time) error {
	slice := w.sliceName(now)
	if w.file != nil && w.slice == slice {
		return nil
	}
	if w.file != nil && now.Before(w.retryAt) {
		return nil // 上次打开失败，重试间隔内继续写当前分片
	}

	next, err := w.openSlice(slice)
	if err != nil {
		// 旧实现在这里返回错误、调用方丢掉整行，且每一行都重试一次。现在计数、记下重试时刻；
		// 调用方有当前分片时继续写它，没有时（首次打开）才报错。
		metrics.IncCounter("log.rotate_failures", nil, 1)
		w.retryAt = now.Add(rotateRetryInterval)
		return err
	}
	w.retryAt = time.Time{}
	old := w.file
	w.file = next
	w.slice = slice
	if old != nil {
		_ = old.Close()
	}
	return nil
}

func (w *timeRotatingFileWriter) openSlice(slice string) (*os.File, error) {
	path := filepath.Join(w.dir, rotatedLogFilename(w.filename, slice))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
}

func (w *timeRotatingFileWriter) sliceName(now time.Time) string {
	return rotateSlotStart(now, w.interval).Format(w.timeFormat)
}

func rotatedLogFilename(filename string, slice string) string {
	ext := filepath.Ext(filename)
	base := strings.TrimSuffix(filename, ext)
	if base == "" {
		base = "roost"
	}
	return base + "." + slice + ext
}

func defaultRotateTimeFormat(interval time.Duration) string {
	switch {
	case interval >= 24*time.Hour:
		return "20060102"
	case interval >= time.Hour && interval%time.Hour == 0:
		return "2006010215"
	case interval >= time.Minute && interval%time.Minute == 0:
		return "200601021504"
	default:
		return "20060102150405"
	}
}

func rotateSlotStart(now time.Time, interval time.Duration) time.Time {
	if interval == 24*time.Hour {
		year, month, day := now.Date()
		return time.Date(year, month, day, 0, 0, 0, 0, now.Location())
	}
	n := int64(interval)
	if n <= 0 {
		return now
	}
	return time.Unix(0, now.UnixNano()/n*n).In(now.Location())
}
