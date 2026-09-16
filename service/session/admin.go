package session

import (
	"context"
	"fmt"
	"strings"
)

// Admin is the operator surface: the one thing a human can do about a resource
// this service cannot hand back.
//
// # Why this exists
//
// A Releaser that fails leaves the resource PENDING on the run, on purpose —
// dropping it is how the implementation this replaces leaked a replica
// permanently — and the Server's sweep retries it on every tick. That is the
// right behaviour for a releaser that is temporarily down.
//
// It is not a way out when the releaser can NEVER succeed. A replica that was
// deleted out of band, a resource id that was never valid, an external system
// that has forgotten the thing exists: the release fails identically to a
// transient outage, so the sweep retries it forever, and the resource stays
// pending for the life of the record. Nothing in this package can mark it
// released except a Releaser that returns nil.
//
// # What actually breaks
//
// The leaked external resource is the smaller half. The OWNER IS BLOCKED,
// permanently, and the path is worth tracing because it is not obvious from
// Run.Live:
//
//	Enter -> the owner's claim is held -> resolveClaim -> the run is not live
//	      -> resolve() releases its resources -> the release fails
//	      -> resolveClaim returns the error -> THE CLAIM IS NEVER FREED
//
// The claim is deliberately freed only after the cleanup succeeds, "so the
// claim is never the thing that outlives the cleanup" — which is right, and it
// means a release that can never succeed is a player who can never enter
// again. Every Enter fails with the releaser's error.
//
// This was stated the other way round while planning — "the owner is not
// blocked, Run.Live does not consider pending resources" — and that reading is
// wrong: it is the claim, not Live, that gates Enter. TestAStuckResourceBlocks
// TheOwnerUntilForced pins the real behaviour, and the mutation that removes
// ForceRelease's effect makes it fail.
//
// # No bus transport
//
// There is no //roost:rpc marker. This method asserts a fact about the
// OUTSIDE WORLD that this service cannot verify — "that resource is really
// gone" — so it is exactly the operation that must not be reachable from any
// process on a bus that carries no caller identity. It is reachable only in
// the process that owns session.
type Admin interface {
	// ForceRelease marks a pending resource released WITHOUT calling the
	// Releaser. The caller is asserting that the resource no longer exists;
	// this service cannot check that, which is why the note is required and
	// recorded.
	ForceRelease(ctx context.Context, runID string, kind string, resourceID string, note string) (run Run, err error)
}

// MaxAdminNoteBytes bounds an operator note. It is stored on the run and read
// back by whoever looks at it next.
const MaxAdminNoteBytes = 512

// ForceRelease implements Admin.
//
// It skips the Releaser deliberately, and that is the whole point: the reason
// an operator is here is that the Releaser cannot succeed. Calling it anyway
// and ignoring the error would be worse than skipping it — it would look like
// a release was attempted and reported as done.
//
// So this is the one operation in the package that breaks "release exactly
// once" as an automatic guarantee, and it replaces it with a recorded human
// assertion. Three things make that honest rather than a hole:
//
//   - The note is required. "The resource is gone" is a claim about the
//     outside world that this service cannot check, so the claim, who made it
//     and when are stored on the run.
//   - Only PENDING resources are accepted. A resource already released is
//     refused rather than re-marked, so a forced release cannot overwrite the
//     timestamp of a real one and make the history unreadable.
//   - ForcedReleases counts them. A run with three forced releases is a run
//     whose resource accounting is not automatic any more, and the next reader
//     needs to know that without reading a log.
func (s *Service) ForceRelease(ctx context.Context, runID string, kind string, resourceID string, note string) (Run, error) {
	if strings.TrimSpace(runID) == "" {
		return Run{}, fmt.Errorf("%w: run id is empty", ErrRequestInvalid)
	}
	if strings.TrimSpace(kind) == "" || strings.TrimSpace(resourceID) == "" {
		return Run{}, fmt.Errorf("%w: a resource kind and id are required", ErrRequestInvalid)
	}
	note, err := validateAdminNote(note)
	if err != nil {
		return Run{}, err
	}
	nowUnix := s.cfg.Now().Unix()
	var result Run
	_, _, err = s.cfg.Runs.Update(ctx, runID, func(current Run, found bool) (Run, bool, error) {
		if !found {
			return current, false, fmt.Errorf("%w: %s", ErrRunMissing, runID)
		}
		next := current.clone()
		for index, held := range next.Resources {
			if held.Kind != kind || held.ID != resourceID {
				continue
			}
			if held.Released() {
				return current, false, fmt.Errorf("%w: %s %s on run %s was already released at %d",
					ErrNotResolvable, kind, resourceID, runID, held.ReleasedAtUnix)
			}
			next.Resources[index].ReleasedAtUnix = nowUnix
			next.Resources[index].ForcedRelease = true
			next.ForcedReleases++
			next.AdminNote = note
			next.AdminActionAtUnix = nowUnix
			next.UpdatedAtUnix = nowUnix
			result = next.clone()
			return next, true, nil
		}
		return current, false, fmt.Errorf("%w: run %s holds no %s %s",
			ErrNotResolvable, runID, kind, resourceID)
	})
	if err != nil {
		return Run{}, err
	}
	// Counted apart from a real release, so the two are never summed into one
	// figure that reads as "resources handed back".
	s.report.Dropped("resource.force_released", 1)
	return result, nil
}

// validateAdminNote requires a reason and bounds it.
func validateAdminNote(note string) (string, error) {
	trimmed := strings.TrimSpace(note)
	if trimmed == "" {
		return "", fmt.Errorf("%w: an operator note is required; forcing a release asserts "+
			"something about the outside world that this service cannot check, so the claim has "+
			"to be recorded", ErrAdminNoteRequired)
	}
	if len(trimmed) > MaxAdminNoteBytes {
		return "", fmt.Errorf("%w: note is %d bytes, limit %d",
			ErrAdminNoteRequired, len(trimmed), MaxAdminNoteBytes)
	}
	return trimmed, nil
}

var _ Admin = (*Service)(nil)
