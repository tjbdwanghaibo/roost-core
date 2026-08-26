package lockstep

import "sort"

// DesyncDetector compares client-reported simulation hashes at sampled
// keyframes and rules by majority: the players whose hash disagrees with the
// most common one are the desynced (or cheating) ones. A hash mismatch on a
// deterministic simulation is always an incident — either tampering or a
// determinism bug — so verdicts are meant to be acted on (kick, flag,
// dump), not smoothed over.
type DesyncDetector struct {
	// quorum is the minimum number of reports before a frame can be judged.
	quorum  int
	reports map[FrameID]map[PlayerID]uint64
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
// a client cannot revise its story. It returns a verdict once the frame has
// quorum; further reports for an already-judged frame re-judge with the
// larger set (the ruling can only gain outliers, never lose them, because
// first reports are immutable).
func (d *DesyncDetector) Report(player PlayerID, frame FrameID, hash uint64) (DesyncVerdict, bool) {
	reports := d.reports[frame]
	if reports == nil {
		reports = make(map[PlayerID]uint64)
		d.reports[frame] = reports
	}
	if _, submitted := reports[player]; !submitted {
		reports[player] = hash
	}
	if len(reports) < d.quorum {
		return DesyncVerdict{}, false
	}
	return d.judge(frame, reports), true
}

// Trim drops report state for frames before the given id (already judged
// and acted on).
func (d *DesyncDetector) Trim(before FrameID) {
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
