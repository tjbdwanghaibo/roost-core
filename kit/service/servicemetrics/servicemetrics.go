// Package servicemetrics re-exports roost-core/servicemetrics: the reporting
// seam every kit service takes as a collaborator. The implementation moved to
// core with the first service domain package (roost-core/service/match, M-06)
// so that domain code can report without importing roost-kit; existing
// importers — kit services and the collaborators roost-codegen generates —
// keep this import path. New code may import roost-core/servicemetrics
// directly; the two names denote the same types.
package servicemetrics

import core "github.com/tjbdwanghaibo/roost-core/servicemetrics"

// Reporter receives a service's accepted / refused / replayed / dropped /
// conflict counts and queue depths. nil means no reporting.
type Reporter = core.Reporter

// Sink wraps a possibly-nil Reporter so callers never check for nil.
type Sink = core.Sink

// Recorder is the in-memory Reporter tests assert against.
type Recorder = core.Recorder

// Wrap returns a Sink over reporter; a nil reporter yields a Sink that
// reports nothing and never fails an operation.
func Wrap(reporter Reporter) Sink { return core.Wrap(reporter) }

// KeyedReporter is a Reporter that keeps a depth per object under a fixed
// name (decision C6).
type KeyedReporter = core.KeyedReporter

// MetricsReporter is the production Reporter: events go into the process's
// metrics registry, which kit ops serves on /metrics.
type MetricsReporter = core.MetricsReporter

// NewMetricsReporter returns the production Reporter for one service.
func NewMetricsReporter(service string) *MetricsReporter { return core.NewMetricsReporter(service) }

// NewRecorder returns an empty Recorder.
func NewRecorder() *Recorder { return core.NewRecorder() }
