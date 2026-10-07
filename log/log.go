package log

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/goroutine"
	"github.com/tjbdwanghaibo/roost-core/metrics"
)

var (
	loggerMu      sync.RWMutex
	defaultLogger = slog.Default()
	outputFile    io.Closer
	// consoleOutput 是 Init 配置的非文件输出（Output 或 stdout），文件 sink 时为 nil；
	// sinkOptions 是那次 Init 的选项。Close 用它们重建默认 logger（RR-20261005-NC-165）。
	consoleOutput io.Writer
	sinkOptions   Options
)

type Options struct {
	Level     slog.Level
	LevelText string
	Output    io.Writer
	JSON      bool

	Stdout   bool
	File     bool
	Dir      string
	Filename string
	Service  string
	Sid      int

	RotateInterval   time.Duration
	RotateTimeFormat string
	NowFunc          func() time.Time

	Caller    bool
	FrameFunc func() uint64

	DisableGoID    bool
	DisableFrame   bool
	DisableContext bool
}

type contextHandler struct {
	next slog.Handler
	opts Options
}

func Init(opts Options) error {
	level, err := resolveLevel(opts)
	if err != nil {
		return err
	}
	opts.Level = level

	var sinks fanOutWriter
	var file io.WriteCloser
	var console io.Writer
	if opts.Output != nil {
		console = opts.Output
	} else if opts.Stdout || !opts.File {
		console = os.Stdout
	}
	if console != nil {
		sinks = append(sinks, namedSink{name: "console", w: console})
	}
	if opts.File {
		file, err = openLogWriter(opts)
		if err != nil {
			return err
		}
		sinks = append(sinks, namedSink{name: "file", w: file})
	}
	if len(sinks) == 0 {
		sinks = append(sinks, namedSink{name: "console", w: os.Stdout})
	}

	var output io.Writer = sinks

	logger := newLogger(output, opts)
	loggerMu.Lock()
	oldFile := outputFile
	outputFile = file
	consoleOutput = console
	sinkOptions = opts
	defaultLogger = logger
	loggerMu.Unlock()
	if oldFile != nil {
		_ = oldFile.Close()
	}
	slog.SetDefault(logger)
	return nil
}

func newLogger(output io.Writer, opts Options) *slog.Logger {
	handlerOpts := &slog.HandlerOptions{Level: opts.Level}
	var handler slog.Handler
	if opts.JSON {
		handler = slog.NewJSONHandler(output, handlerOpts)
	} else {
		handler = newOrderedTextHandler(output, handlerOpts)
	}
	return slog.New(contextHandler{next: handler, opts: opts})
}

// Close 关闭 Init 打开的日志文件，并把默认 logger 换成同格式、只写非文件输出的 logger；
// 只配了文件时改写 stderr（RR-20261005-NC-165）。
//
// 关闭之后仍有日志要写：app.run 返回后生成的 main 用 slog 写 "server exit" 和退出原因，停机超时时
// 仍在运行的 Serve / Shutdown / Mod 也会继续打日志。旧 Close 只关文件、默认 logger 仍写已关闭的
// 文件，只配文件 sink 时这些行全部被吞掉。调用方在 Init 之后自己 SetDefault 了别的 logger 时，
// 只更新本包的 Default，不覆盖调用方的 slog 默认值。
func Close() error {
	loggerMu.Lock()
	if outputFile == nil {
		loggerMu.Unlock()
		return nil
	}
	err := outputFile.Close()
	outputFile = nil
	fallback := consoleOutput
	if fallback == nil {
		fallback = os.Stderr
	}
	installed := defaultLogger
	defaultLogger = newLogger(fallback, sinkOptions)
	replacement := defaultLogger
	loggerMu.Unlock()
	if slog.Default() == installed {
		slog.SetDefault(replacement)
	}
	return err
}

func Default() *slog.Logger {
	loggerMu.RLock()
	defer loggerMu.RUnlock()
	return defaultLogger
}

func Debug(msg string, args ...any) {
	logWithCallerSkip(context.Background(), Default(), slog.LevelDebug, msg, 3, args...)
}

func Info(msg string, args ...any) {
	logWithCallerSkip(context.Background(), Default(), slog.LevelInfo, msg, 3, args...)
}

func Warn(msg string, args ...any) {
	logWithCallerSkip(context.Background(), Default(), slog.LevelWarn, msg, 3, args...)
}

func Error(msg string, args ...any) {
	logWithCallerSkip(context.Background(), Default(), slog.LevelError, msg, 3, args...)
}

func ParseLevel(text string) (slog.Level, error) {
	var level slog.Level
	text = strings.TrimSpace(text)
	if text == "" {
		return level, nil
	}
	switch strings.ToLower(text) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		if err := level.UnmarshalText([]byte(strings.ToUpper(text))); err != nil {
			return level, fmt.Errorf("log level %q: %w", text, err)
		}
		return level, nil
	}
}

func resolveLevel(opts Options) (slog.Level, error) {
	if strings.TrimSpace(opts.LevelText) == "" {
		return opts.Level, nil
	}
	return ParseLevel(opts.LevelText)
}

func openLogWriter(opts Options) (io.WriteCloser, error) {
	dir := opts.Dir
	if dir == "" {
		dir = "log"
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	name := opts.Filename
	if name == "" {
		base := opts.Service
		if base == "" {
			base = "roost"
		}
		if opts.Sid != 0 {
			base = fmt.Sprintf("%s-%d", base, opts.Sid)
		}
		name = base + ".log"
	}
	if opts.RotateInterval > 0 {
		return newTimeRotatingFileWriter(timeRotatingFileOptions{
			Dir:        dir,
			Filename:   name,
			Interval:   opts.RotateInterval,
			TimeFormat: opts.RotateTimeFormat,
			NowFunc:    opts.NowFunc,
		})
	}
	return os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
}

func (h contextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h contextHandler) Handle(ctx context.Context, record slog.Record) error {
	attrs := h.contextAttrs(record.PC)
	if len(attrs) == 0 {
		return h.next.Handle(ctx, record)
	}

	next := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	next.AddAttrs(attrs...)
	record.Attrs(func(attr slog.Attr) bool {
		next.AddAttrs(attr)
		return true
	})
	return h.next.Handle(ctx, next)
}

func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{next: h.next.WithAttrs(attrs), opts: h.opts}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{next: h.next.WithGroup(name), opts: h.opts}
}

func (h contextHandler) contextAttrs(pc uintptr) []slog.Attr {
	attrs := make([]slog.Attr, 0, 8)
	if !h.opts.DisableGoID {
		attrs = append(attrs, slog.Int64("goId", goroutine.GoID()))
	}
	frame := uint64(0)
	c := fctx.CurrentContext()
	if c != nil && c.Frame != 0 {
		frame = c.Frame
	}
	if frame == 0 && h.opts.FrameFunc != nil {
		frame = h.opts.FrameFunc()
	}
	if !h.opts.DisableFrame {
		attrs = append(attrs, slog.Uint64("frame", frame))
	}
	nowMilli := time.Now().UnixMilli()
	// 日志时间统一使用系统钟；请求的业务时间由调用方按业务字段记录。
	attrs = append(attrs, slog.Int64("server_time_ms", nowMilli))
	if h.opts.DisableContext || c == nil {
		if h.opts.Caller {
			attrs = append(attrs, callerAttrs(pc)...)
		}
		return attrs
	}
	meta := c.Meta
	if meta.Source != "" {
		attrs = append(attrs, slog.String("source", meta.Source))
	}
	if meta.Handler != "" {
		attrs = append(attrs, slog.String("handler", meta.Handler))
	}
	if h.opts.Caller {
		attrs = append(attrs, callerAttrs(pc)...)
	}
	if meta.PlayerID != 0 {
		attrs = append(attrs, slog.Int64("player", meta.PlayerID))
	}
	if meta.MsgID != 0 {
		attrs = append(attrs, slog.Uint64("msg", uint64(meta.MsgID)))
	}
	if meta.Seq != 0 {
		attrs = append(attrs, slog.Uint64("seq", uint64(meta.Seq)))
	}
	return attrs
}

func logWithCallerSkip(ctx context.Context, logger *slog.Logger, level slog.Level, msg string, skip int, args ...any) {
	if logger == nil {
		logger = Default()
	}
	if !logger.Enabled(ctx, level) {
		return
	}
	var pcs [1]uintptr
	runtime.Callers(skip, pcs[:])
	record := slog.NewRecord(time.Now(), level, msg, pcs[0])
	record.Add(args...)
	_ = logger.Handler().Handle(ctx, record)
}

func callerAttrs(pc uintptr) []slog.Attr {
	if pc == 0 {
		return nil
	}
	fs := runtime.CallersFrames([]uintptr{pc})
	frame, _ := fs.Next()
	if frame.File == "" {
		return nil
	}
	attrs := []slog.Attr{slog.String("caller", frame.File+":"+strconv.Itoa(frame.Line))}
	if frame.Function != "" {
		attrs = append(attrs, slog.String("caller_func", frame.Function))
	}
	return attrs
}

// namedSink 是一个日志输出及其在 log.write_errors 里的标签。
type namedSink struct {
	name string
	w    io.Writer
}

// fanOutWriter 把每一行写给全部 sink：某个 sink 出错不影响其余 sink，错误按 sink 计入
// log.write_errors，返回第一个错误（RR-20261005-NC-263）。旧实现用 io.MultiWriter，它在第一个写
// 失败处停止——控制台排在前面，控制台出错时文件也收不到这一行；而 slog 吞掉 handler 的写错误，
// 丢行没有任何痕迹。
type fanOutWriter []namedSink

func (f fanOutWriter) Write(p []byte) (int, error) {
	var first error
	for _, sink := range f {
		n, err := sink.w.Write(p)
		if err == nil && n != len(p) {
			err = io.ErrShortWrite
		}
		if err != nil {
			metrics.IncCounter("log.write_errors", metrics.Labels{"sink": sink.name}, 1)
			if first == nil {
				first = err
			}
		}
	}
	if first != nil {
		return 0, first
	}
	return len(p), nil
}
