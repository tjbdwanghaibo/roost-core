"""在生成工程的 room_presence_test.go 里，紧跟 Track(2, 102, "b", 1) 之后插入一段等待：
等后台断线先跑完（第一次调用在错误入队之前完成）。这是夹具允许的调度，只是把它变成必然发生。
不改运行时代码；只用于红 / 绿对照，不提交。"""
import sys
from pathlib import Path

path = Path(sys.argv[1])
text = path.read_text()
anchor = 'w.presence.Track(2, 102, "b", 1)'
idx = text.index(anchor)
line_end = text.index("\n", idx)
force = (
    "\n\tfor deadline := time.Now().Add(2 * time.Second); w.presence.Tracked() != 0 && time.Now().Before(deadline); {"
    " time.Sleep(time.Millisecond) } // FORCED: 后台断线先于注入跑完"
)
path.write_text(text[:line_end] + force + text[line_end:])
print("forced schedule inserted after", anchor)
