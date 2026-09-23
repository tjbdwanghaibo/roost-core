package lockstep

import "sort"

// DesyncDetector compares client-reported simulation hashes at sampled
// keyframes and rules by majority: the players whose hash disagrees with the
// most common one are the desynced (or cheating) ones. A hash mismatch on a
// deterministic simulation is always an incident — either tampering or a
// determinism bug — so verdicts are meant to be acted on (kick, flag,
// dump), not smoothed over.
type DesyncDetector struct {
	// quorum is the minimum size of the AGREEING group before a frame can
	// be judged: a verdict exists only once some hash has quorum reports
	// backing it. Choose quorum > seats/2 (the kit Room derives it that
	// way) and the majority can never flip afterwards — a minority that
	// reports first can no longer frame an honest player.
	quorum        int
	reports       map[FrameID]map[PlayerID]uint64
	trimmedBefore FrameID
}

// DesyncVerdict is the ruling for one sampled frame.
type DesyncVerdict struct {
	Frame FrameID
	// Majority is the hash the largest group of players agrees on. On a tie
	// the smaller hash value wins — deterministic, and a tie already means
	// the match is unsalvageable regardless of which side is "right".
	Majority uint64
	// Outliers are the players whose report disagrees with the majority, in
	// ascending order.
	Outliers []PlayerID
}

// NewDesyncDetector builds a detector requiring quorum reports per frame
// (quorum <= 0 selects 2).
func NewDesyncDetector(quorum int) *DesyncDetector {
	if quorum <= 0 {
		quorum = 2
	}
	return &DesyncDetector{quorum: quorum, reports: make(map[FrameID]map[PlayerID]uint64)}
}

// Report records one player's simulation hash for a sampled frame. The
// first report per (frame, player) wins — later duplicates are ignored, so
// a client cannot revise its story. It returns a verdict once some hash is
// backed by at least quorum agreeing reports; later reports re-judge with
// the larger set. Frames already trimmed are tombstoned: late reports for
// them are ignored, so a colluding pair cannot rebuild a "majority" on a
// frame whose honest reports were already reclaimed.
func (d *DesyncDetector) Report(player PlayerID, frame FrameID, hash uint64) (DesyncVerdict, bool) {
	if frame < d.trimmedBefore {
		return DesyncVerdict{}, false
	}
	reports := d.reports[frame]
	if reports == nil {
		reports = make(map[PlayerID]uint64)
		d.reports[frame] = reports
	}
	if _, submitted := reports[player]; !submitted {
		reports[player] = hash
	}
	counts := make(map[uint64]int, len(reports))
	best := 0
	for _, h := range reports {
		counts[h]++
		if counts[h] > best {
			best = counts[h]
		}
	}
	if best < d.quorum {
		return DesyncVerdict{}, false
	}
	return d.judge(frame, reports), true
}

// Trim drops report state for frames before the given id (already judged
// and acted on) and tombstones them: later reports for trimmed frames are
// ignored instead of rebuilding a fresh (and forgeable) report set.
func (d *DesyncDetector) Trim(before FrameID) {
	if before > d.trimmedBefore {
		d.trimmedBefore = before
	}
	for frame := range d.reports {
		if frame < before {
			delete(d.reports, frame)
		}
	}
}

func (d *DesyncDetector) judge(frame FrameID, reports map[PlayerID]uint64) DesyncVerdict {
	counts := make(map[uint64]int, len(reports))
	for _, hash := range reports {
		counts[hash]++
	}
	majority, majorityCount := uint64(0), 0
	for hash, count := range counts {
		if count > majorityCount || count == majorityCount && hash < majority {
			majority, majorityCount = hash, count
		}
	}
	verdict := DesyncVerdict{Frame: frame, Majority: majority}
	for player, hash := range reports {
		if hash != majority {
			verdict.Outliers = append(verdict.Outliers, player)
		}
	}
	sort.Slice(verdict.Outliers, func(i, j int) bool { return verdict.Outliers[i] < verdict.Outliers[j] })
	return verdict
}
