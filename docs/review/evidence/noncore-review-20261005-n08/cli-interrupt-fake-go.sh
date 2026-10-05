#!/bin/sh
# 替身 go：记录工作目录后长睡，模拟正在下载 / 编译的 go 命令。
pwd > "$FAKE_GO_MARK.tmp" && mv "$FAKE_GO_MARK.tmp" "$FAKE_GO_MARK"
sleep 600
