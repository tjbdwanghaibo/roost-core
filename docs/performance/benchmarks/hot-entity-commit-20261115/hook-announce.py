# M-15 复现 lb-fix2 的条件（不提交）：每次登录同步发一条世界频道公告（修复前的做法），等 3 秒。
import pathlib
p = pathlib.Path('game/announce/announce.go')
s = p.read_text()
old = '''func (b *Batcher) Entered(playerID int64) {
	if b == nil {
		return
	}
'''
new = old + '''	{
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = b.publish(ctx, Text(1, []int64{playerID}), fmt.Sprintf("enter:%s:%d", b.keyPrefix, playerID))
		return
	}
'''
assert old in s
p.write_text(s.replace(old, new, 1))
