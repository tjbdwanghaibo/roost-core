#!/usr/bin/env python3
"""核对 Markdown 相对链接与锚点（默认范围 docs/framework/**）。

根包门禁 doc_links_gate_test.go 只查相对链接的目标文件存在，不查 #锚点。
本脚本补上锚点：每个相对链接的目标文件必须存在；带 #锚点 且目标是 .md 时，
锚点必须等于目标文件某个标题按 GitHub 规则生成的 slug（含重名的 -1、-2 后缀）。

用法：
  python3 scripts/check-doc-anchors.py                 # 查 docs/framework
  python3 scripts/check-doc-anchors.py docs/USER_GUIDE.md docs/framework
退出码：0 = 零断链；1 = 有断链（逐条列出 文件:行: 链接 —— 原因）。

不查：外部 URL、mailto、代码块与行内代码里的方括号、HTML 注释里的内容。
"""
import os
import re
import sys
import urllib.parse

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

LINK = re.compile(r'\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"]*")?\s*\)')
INLINE_CODE = re.compile(r'`[^`]*`')
HTML_COMMENT = re.compile(r'<!--.*?-->', re.S)
HEADING = re.compile(r'^(#{1,6})\s+(.*?)\s*#*\s*$')
EXPLICIT_ANCHOR = re.compile(r'<a\s+(?:name|id)="([^"]+)"')


def strip_fences(text):
    """按行返回 (行号, 内容)，跳过 ``` / ~~~ 围起来的代码块。"""
    out, fenced = [], False
    for i, line in enumerate(text.split('\n'), 1):
        s = line.strip()
        if s.startswith('```') or s.startswith('~~~'):
            fenced = not fenced
            continue
        if not fenced:
            out.append((i, line))
    return out


def github_slug(title):
    t = re.sub(r'!?\[([^\]]*)\]\([^)]*\)', r'\1', title)  # 链接 / 图片只留文字
    t = re.sub(r'<[^>]+>', '', t)                          # 行内 HTML
    t = t.strip().lower()
    t = ''.join(ch for ch in t if ch.isalnum() or ch in '-_ ' or ch == '‍')
    return t.replace(' ', '-')


_anchor_cache = {}


def anchors_of(path):
    if path in _anchor_cache:
        return _anchor_cache[path]
    with open(path, encoding='utf-8') as f:
        text = f.read()
    seen, anchors = {}, set()
    for _, line in strip_fences(text):
        for m in EXPLICIT_ANCHOR.finditer(line):
            anchors.add(m.group(1))
        m = HEADING.match(line)
        if not m:
            continue
        base = github_slug(m.group(2))
        n = seen.get(base, 0)
        anchors.add(base if n == 0 else f'{base}-{n}')
        seen[base] = n + 1
    _anchor_cache[path] = anchors
    return anchors


def check_file(path):
    broken = []
    with open(path, encoding='utf-8') as f:
        text = f.read()
    text = HTML_COMMENT.sub(lambda m: '\n' * m.group(0).count('\n'), text)
    for lineno, line in strip_fences(text):
        line = INLINE_CODE.sub('', line)
        for m in LINK.finditer(line):
            target = m.group(1)
            if re.match(r'^[a-zA-Z][a-zA-Z0-9+.-]*:', target):
                continue  # http:、https:、mailto: 等
            target = target.split('?', 1)[0]
            file_part, _, anchor = target.partition('#')
            if file_part:
                dest = os.path.normpath(os.path.join(os.path.dirname(path), urllib.parse.unquote(file_part)))
            else:
                dest = path
            rel = os.path.relpath(path, ROOT)
            if not os.path.exists(dest):
                broken.append(f'{rel}:{lineno}: {m.group(1)} —— 目标不存在')
                continue
            if anchor and dest.endswith('.md') and os.path.isfile(dest):
                if urllib.parse.unquote(anchor).lower() not in anchors_of(dest):
                    broken.append(f'{rel}:{lineno}: {m.group(1)} —— 锚点不存在')
    return broken


def collect(args):
    files = []
    for a in args:
        p = os.path.join(ROOT, a)
        if os.path.isdir(p):
            for d, _, names in os.walk(p):
                files += [os.path.join(d, n) for n in names if n.endswith('.md')]
        else:
            files.append(p)
    return sorted(files)


def main():
    args = sys.argv[1:] or ['docs/framework']
    files = collect(args)
    broken = []
    for f in files:
        broken += check_file(f)
    for b in broken:
        print(b)
    print(f'checked {len(files)} files, {len(broken)} broken', file=sys.stderr)
    return 1 if broken else 0


if __name__ == '__main__':
    sys.exit(main())
