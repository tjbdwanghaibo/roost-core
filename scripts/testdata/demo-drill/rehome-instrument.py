#!/usr/bin/env python3
"""只改临时生成工程；core 用 go build -overlay，绝不改检出仓库。"""
import json
import pathlib
import sys

repo, generated, out = map(pathlib.Path, sys.argv[1:4])
mutant = len(sys.argv) > 4 and sys.argv[4] == "skip-checkpoint"

def replace_once(text, old, new):
    if text.count(old) != 1:
        raise SystemExit(f"fault anchor drift: expected one occurrence of {old!r}")
    return text.replace(old, new)

driver = generated / "game/rehome/rehome_driver.go"
text = driver.read_text()
for anchor, stage in [
    # 锚点带前导换行：D22 之后 copying 在异地清理分支里还有一处更深缩进的同形语句，只认函数体第一层那一处。
    ("\n\tquiescing, cancel := context.WithTimeout(ctx, departBudget)", "accepted"),
    ("\n\trechecking, cancel := context.WithTimeout(ctx, callBudget)", "quiesced"),
    ("\n\tdeparting, cancel := context.WithTimeout(ctx, departBudget)", "leaving"),
    ("\n\tcopying, cancel := context.WithTimeout(ctx, departBudget)", "handed-off"),
    ("\n\td.finishCopied(ctx, move, verdict, binding, Aborted, &copied)", "committed"),
]:
    text = replace_once(text, anchor, f'\n\trehomeDrillPoint("{stage}", move)'+anchor)
anchor = '\t\terr := d.documents.Drop(dropping, copied, sid)\n\t\tcancel()\n\t\tif err == nil {\n\t\t\treturn'
text = replace_once(text, anchor, anchor.replace('\n\t\t\treturn', '\n\t\t\tif sid == d.self { rehomeDrillPoint("dropped", move) }\n\t\t\treturn'))
driver.write_text(text)
(generated / "game/rehome/rehome_drill_faults.go").write_text((repo / "scripts/testdata/demo-drill/rehome-faults.go.txt").read_text())

# 暂停发生在真实 Mongo 投影完成、WAL Ack 之前；不伪造已落库或 checkpoint。
handoff = repo / "framework/dataengine/engine/entity_handoff.go"
text = handoff.read_text()
text = replace_once(text, 'import (', 'import (\n "os"\n "path/filepath"\n "strings"')
text = replace_once(text, 'func (p *Projector) waitProjectedRecordsConfirmed(ctx context.Context) error {', '''func (p *Projector) waitProjectedRecordsConfirmed(ctx context.Context) error {
 started := time.Now()
 defer func() { slog.Info("rehome drill: checkpoint wait", "elapsed_us", time.Since(started).Microseconds()) }()''')
text = replace_once(text, '\tticker := time.NewTicker(handOffCheckpointPoll)', ''' if dir := os.Getenv("DRILL_REHOME_FAULT_DIR"); dir != "" {
  if err := os.WriteFile(filepath.Join(dir, "checkpoint-waiting"), []byte("projected records still unconfirmed"), 0600); err != nil { panic(err) }
 }
\tticker := time.NewTicker(handOffCheckpointPoll)''')
if mutant:
    start = text.index('\t// 3. 等 checkpoint。')
    end = text.index('\t// 4. 卸载', start)
    text = text[:start] + '\t// MUTANT: checkpoint wait removed.\n' + text[end:]
text += '''
// rehomeDrillHoldAck 只由临时 overlay 的 ReplayPass 调用。
func rehomeDrillHoldAck(ctx context.Context, records []coredata.CommitRecord) error {
 dir := os.Getenv("DRILL_REHOME_FAULT_DIR")
 if dir == "" { return nil }
 raw, err := os.ReadFile(filepath.Join(dir, "hold-checkpoint"))
 if os.IsNotExist(err) { return nil } else if err != nil { return err }
 var target int64
 if _, err := fmt.Sscan(string(raw), &target); err != nil { return err }
 matched := false
 for _, record := range records {
  // demo 在交出前最后一笔 Player 写入是 LeaveScene（保存位置）。Prepare 之后还有这笔写入，不能提前堵住回放。
  if !strings.HasSuffix(record.Handler, "LeaveScene") { continue }
  for _, mutation := range record.Mutations { if mutation.Key.ID == target { matched = true } }
 }
 if !matched { return nil }
 for {
  if _, err := os.Stat(filepath.Join(dir, "hold-checkpoint")); os.IsNotExist(err) { return nil } else if err != nil { return err }
  if err := os.WriteFile(filepath.Join(dir, "checkpoint-held"), []byte("Mongo projected; before WAL Ack"), 0600); err != nil { return err }
  select { case <-ctx.Done(): return ctx.Err(); case <-time.After(10*time.Millisecond): }
 }
}
'''
out.mkdir(parents=True, exist_ok=True)
(out / handoff.name).write_text(text)
replay = repo / "framework/dataengine/engine/projector_replay.go"
text = replace_once(replay.read_text(), '\t\tackStart := hoptrace.Now()', '\t\tif err := rehomeDrillHoldAck(ctx, records[acked:processed]); err != nil { return err }\n\t\tackStart := hoptrace.Now()')
(out / replay.name).write_text(text)
# 后台 checkpoint（v1.48.0 起缺省打开）时确认在本轮的后台写者里做，不走上面的同步分支；两处都要堵，
# 否则 checkpoint 用例的“暂停确认”根本没生效，交出会在确认之前合法返回、演练误报 CHECKPOINT_BYPASS。
writer = repo / "framework/dataengine/engine/projector_checkpoint.go"
text = replace_once(writer.read_text(), '\t\t\tstart := hoptrace.Now()\n\t\t\terr := p.ack(ctx, fences[target-1])',
    '\t\t\tif herr := rehomeDrillHoldAck(ctx, records[from:target]); herr != nil {\n\t\t\t\tw.mu.Lock()\n\t\t\t\tw.err = herr\n\t\t\t\tw.notifyLocked()\n\t\t\t\tw.mu.Unlock()\n\t\t\t\treturn\n\t\t\t}\n'
    '\t\t\tstart := hoptrace.Now()\n\t\t\terr := p.ack(ctx, fences[target-1])')
(out / writer.name).write_text(text)
(out / "overlay.json").write_text(json.dumps({"Replace": {str(handoff): str(out / handoff.name), str(replay): str(out / replay.name), str(writer): str(out / writer.name)}}))
