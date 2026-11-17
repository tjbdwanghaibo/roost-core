#!/usr/bin/env bash
# 没有声明 client_export.config 的工程：旧生成器（HEAD 的干净树）与新生成器（工作树）各 project new 三种工程，diff -r。
# 唯一允许的差异是横排表 Convert<Type>CSV 里新增的一行 reader.Comment = '#'（第 0 项，协调者 2026-11-17 定）。
# 用法：same-tree.sh <旧 roost> <新 roost> <工作目录> <输出文件>
set -uo pipefail
old="$1"; new="$2"; work="$3"; out="$4"
export GOWORK=off
rm -rf "$work"; mkdir -p "$work"
: > "$out"
for side in old new; do
  bin="$old"; [ "$side" = new ] && bin="$new"
  mkdir -p "$work/$side"
  (cd "$work/$side" &&
    "$bin" project new planet -module example.com/planet -out plain > plain.log 2>&1; echo "$side plain rc=$?" >> "$out"
    "$bin" project new planet -module example.com/planet -out game -template game > game.log 2>&1; echo "$side game rc=$?" >> "$out"
    "$bin" project new planet -module example.com/planet -out demo -mods configdata,mongo,nats,dataengine,nest -template game-demo > demo.log 2>&1; echo "$side game-demo rc=$?" >> "$out")
done
for name in plain game demo; do
  files=$(cd "$work/new/$name" && find . -type f -not -path './.git/*' | wc -l | tr -d ' ')
  diff -r -x .git -x '*.log' "$work/old/$name" "$work/new/$name" > "$work/$name.diff" 2>&1
  changed=$(grep -c '^diff -r' "$work/$name.diff" || true)
  # diff 正文里除了 "> \treader.Comment = '#'" 与行号标记以外的行
  other=$(grep -vE "^diff -r |^[0-9]+a[0-9]+$|^> 	reader.Comment = '#'$" "$work/$name.diff" | wc -l | tr -d ' ')
  added=$(grep -cE "^> 	reader.Comment = '#'$" "$work/$name.diff" || true)
  echo "$name: files=$files changed-files=$changed added-comment-lines=$added other-diff-lines=$other" >> "$out"
  grep '^diff -r' "$work/$name.diff" | sed 's#'"$work"'/##g' >> "$out"
done
cat "$out"
