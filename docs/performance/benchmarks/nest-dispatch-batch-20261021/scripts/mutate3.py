import sys
f, m = sys.argv[1], sys.argv[2]
p = 'framework/nest/dispatch_queue.go' if f.endswith('dq.final') else 'framework/nest/client.go'
s = open(f).read()
def rep(a, b):
    global s
    assert s.count(a) == 1, (m, a)
    s = s.replace(a, b)
if m == 'no_tails':
    rep("""		if prev := q.tails[id]; prev != nil {""", """		if prev := q.tails[id]; prev != nil && !j.batch {""")
    rep("""		if q.tails[id] != nil {
			blocked = true""", """		if q.tails[id] != nil && !j.batch {
			blocked = true""")
elif m == 'signal_once':
    rep("""			for range min(n, q.lanes[lane].config.Workers) {""", """			for range min(n, 1) {""")
elif m == 'single_fifo':
    rep("""	default:
		state.batchReady.push(j)
	}""", """	default:
		state.ready.push(j)
	}""")
elif m == 'strict_priority':
    rep("""	if batchAvailable && (state.preferBatch || !hasOrdinary) {""", """	if batchAvailable && !hasOrdinary {""")
elif m == 'after_locked':
    rep("""			q.mu.Unlock()
			finished = i + 1
			if after != nil {
				after()
			}""", """			finished = i + 1
			if after != nil {
				after()
			}
			q.mu.Unlock()""")
elif m == 'after_before_finish':
    rep("""			// 转 I/O 或有出队后动作：先结束此前已执行的作业和本项，出队后动作在不持锁时、本组下一项之前执行。
			q.mu.Lock()""", """			if after != nil {
				after()
				after = nil
			}
			q.mu.Lock()""")
elif m == 'shared_params':
    rep('msg.Params = append(msg.Params[:0], item.Params...)', 'msg.Params = append(msg.Params[:0], items[0].Params...)')
elif m == 'whole_batch_fails':
    rep("""		fullID, err := normalizeClientID(item.ID)
		if err != nil {
			fail(i, err)
			continue
		}""", """		fullID, err := normalizeClientID(item.ID)
		if err != nil {
			for k := range items {
				fail(k, err)
			}
			for _, m := range msgs {
				recycleMsg(m)
			}
			return errs
		}""")
elif m == 'all_short':
    rep("""	j.lane = businessLane(msg)
	if prepareIO {""", """	j.lane = dispatchShortLane
	if prepareIO {""")
elif m == 'no_promote':
    rep("""			if !j.batch {
				q.promoteLocked(prev)
			}""", "")
elif m == 'urgent_no_skip':
    rep("""		if j := state.urgent[state.urgentHead]; !j.started {""", """		if j := state.urgent[state.urgentHead]; true {""")
elif m == 'no_share':
    rep("""	share := 1 + state.readyCount/max(1, state.config.Workers)""", """	share := dispatchWorkerBatch""")
elif m == 'finish_only_after_job':
    rep("""			for _, done := range local[finished:i] {
				q.finishJobLocked(state, done)
			}
			if rerouted {""", """			if rerouted {""")
    rep("""			finished = i + 1
			if after != nil {""", """			local = append(local[:i:i], local[i+1:]...)
			_ = finished
			if after != nil {""")
else:
    raise SystemExit('unknown ' + m)
open(p, 'w').write(s)
