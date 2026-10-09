package log

import (
	"errors"
	"os"
	"testing"
	"time"
)

// U-0149 · C2 · gap map core `log` 2/2：轮转间隔必须为正；关闭后的写入报 os.ErrClosed。
func TestRotatingWriterRefusesNonPositiveIntervalsAndWritesAfterClose(t *testing.T) {
	if _, err := newTimeRotatingFileWriter(timeRotatingFileOptions{Dir: t.TempDir(), Filename: "app.log"}); err == nil {
		t.Fatal("interval 0 accepted")
	}
	writer, err := newTimeRotatingFileWriter(timeRotatingFileOptions{Dir: t.TempDir(), Filename: "app.log", Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := writer.Write([]byte("late\n")); !errors.Is(err, os.ErrClosed) || n != 0 {
		t.Fatalf("Write after Close = (%d, %v), want os.ErrClosed", n, err)
	}
}
