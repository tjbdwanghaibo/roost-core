// Package policy holds the ways subscriptions get organized on top of an
// entitysync.Manager: by views (Interest: spatial views over an AOI grid and
// relation views over explicit entity sets), by membership in a group (Group),
// or one binding at a time (Direct). None of them touch the wire or the
// frames — a policy only decides WHO receives WHOM and says so to the
// manager. The manager, in turn, knows nothing about any of them.
package policy
