package dataengine

import (
	"errors"
	"fmt"
	"math"

	coredata "github.com/tjbdwanghaibo/cube-core/dataengine"
	corenest "github.com/tjbdwanghaibo/cube-core/nest"
)

type projectionSegment struct {
	records []coredata.CommitRecord
	fences  []corenest.CommitFence
	batch   bool
}

func isBatchProjectionRecord(record coredata.CommitRecord) bool {
	return record.Handler != MigrationHandler && len(record.Mutations) == 1 &&
		len(record.Effects) == 0 && len(record.Receipts) == 0 && record.Mutations[0].Remote == nil
}

func saturatingAdd(a, b int) int {
	if b > 0 && a > math.MaxInt-b {
		return math.MaxInt
	}
	if b < 0 && a < math.MinInt-b {
		return math.MinInt
	}
	return a + b
}

func saturatingSum(parts ...int) int {
	total := 0
	for _, part := range parts {
		total = saturatingAdd(total, part)
	}
	return total
}

func saturatingMul(a, b int) int {
	if a == 0 || b == 0 {
		return 0
	}
	if a > 0 {
		if b > 0 && a > math.MaxInt/b {
			return math.MaxInt
		}
		if b < 0 && b < math.MinInt/a {
			return math.MinInt
		}
	} else {
		if b > 0 && a < math.MinInt/b {
			return math.MinInt
		}
		if b < 0 && a < math.MaxInt/b {
			return math.MaxInt
		}
	}
	return a * b
}

func projectionRecordLogicalBytes(record coredata.CommitRecord) int {
	n := 64
	n = saturatingAdd(n, len(record.Handler))
	n = saturatingAdd(n, len(record.RequestID))
	for _, m := range record.Mutations {
		n = saturatingAdd(n, saturatingSum(80, len(m.Key.Database), len(m.Key.Resource), len(m.Codec), len(m.Data), len(m.Patch.SetBSON)))
		for _, path := range m.Patch.Unset {
			n = saturatingAdd(n, saturatingSum(16, len(path)))
		}
		if m.Remote != nil {
			n = saturatingAdd(n, 256)
			for _, rm := range m.Remote.Mutations {
				n = saturatingAdd(n, saturatingSum(32, len(rm.Database), len(rm.Collection), len(rm.Data)))
			}
			for _, rd := range m.Remote.Deletes {
				n = saturatingAdd(n, saturatingSum(24, len(rd.Database), len(rd.Collection)))
			}
			for _, snapshot := range m.Remote.Snapshots {
				n = saturatingAdd(n, saturatingSum(64, len(snapshot.Data)))
			}
			n = saturatingAdd(n, saturatingMul(len(m.Remote.Invalidations), 32))
		}
	}
	for _, e := range record.Effects {
		n = saturatingAdd(n, saturatingSum(48, len(e.ID), len(e.Topic), len(e.Key), len(e.Payload)))
		for k, v := range e.Headers {
			n = saturatingAdd(n, saturatingSum(16, len(k), len(v)))
		}
	}
	for _, r := range record.Receipts {
		n = saturatingAdd(n, saturatingSum(48, len(r.Namespace), len(r.ID), len(r.Digest), len(r.Payload)))
	}
	return n
}

func planProjectionSegments(records []coredata.CommitRecord, fences []corenest.CommitFence, maxRecords, maxBytes int) ([]projectionSegment, error) {
	if len(records) != len(fences) {
		return nil, errors.New("dataengine: records and fences length mismatch")
	}
	if maxRecords <= 0 || maxBytes <= 0 {
		return nil, errors.New("dataengine: projection limits must be positive")
	}
	segments := make([]projectionSegment, 0)
	for i := 0; i < len(records); {
		if !isBatchProjectionRecord(records[i]) {
			segments = append(segments, projectionSegment{records: []coredata.CommitRecord{records[i]}, fences: []corenest.CommitFence{fences[i]}})
			i++
			continue
		}
		start, bytes := i, 0
		for i < len(records) && isBatchProjectionRecord(records[i]) {
			sz := projectionRecordLogicalBytes(records[i])
			if i > start && (i-start >= maxRecords || bytes > maxBytes-sz) {
				break
			}
			bytes = saturatingAdd(bytes, sz)
			i++
			if i-start >= maxRecords {
				break
			}
		}
		if i == start {
			return nil, fmt.Errorf("dataengine: planner made no progress")
		}
		segments = append(segments, projectionSegment{records: records[start:i], fences: fences[start:i], batch: true})
	}
	return segments, nil
}
