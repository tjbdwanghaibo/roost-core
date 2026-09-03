package loadtest

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/admin"
)

// Admin command names (reachable through the assembled ops surface).
const (
	AdminCommandProfiles = "robot.loadtest.profiles"
	AdminCommandStart    = "robot.loadtest.start"
	AdminCommandStop     = "robot.loadtest.stop"
	AdminCommandStatus   = "robot.loadtest.status"
	AdminCommandHistory  = "robot.loadtest.history"
	AdminCommandReport   = "robot.loadtest.report"
)

// RegisterAdminCommands registers the load-test control commands on the
// given admin registry — pass the instance from app.Lookup (app.ModAdmin),
// never the package default (the assembled ops surface only consults the
// instance).
func RegisterAdminCommands(reg *admin.Registry, m *Manager) error {
	if reg == nil {
		return errors.New("robot loadtest: admin registry is required")
	}
	if m == nil {
		return errors.New("robot loadtest: manager is required")
	}
	return errors.Join(
		reg.Register(admin.CommandDef{
			Name:        AdminCommandProfiles,
			Description: "list robot load-test profiles",
			Handler: func(context.Context, admin.Command) (admin.Result, error) {
				return admin.Result{Data: map[string]any{"profiles": m.Profiles()}}, nil
			},
		}),
		reg.Register(admin.CommandDef{
			Name:        AdminCommandStart,
			Description: "start a robot load-test run for a profile",
			Handler: func(ctx context.Context, cmd admin.Command) (admin.Result, error) {
				req, err := admin.DecodePayload[StartRequest](cmd)
				if err != nil {
					return admin.Result{}, err
				}
				snapshot, err := m.StartAdmin(ctx, req)
				if err != nil {
					return admin.Result{}, err
				}
				return admin.Result{Data: map[string]any{"run": snapshot}}, nil
			},
		}),
		reg.Register(admin.CommandDef{
			Name:        AdminCommandStop,
			Description: "stop the active robot load-test run",
			Handler: func(ctx context.Context, cmd admin.Command) (admin.Result, error) {
				req, err := admin.DecodePayload[StopRequest](cmd)
				if err != nil {
					return admin.Result{}, err
				}
				result, err := m.Stop(ctx, req)
				if err != nil {
					return admin.Result{}, err
				}
				return admin.Result{Data: map[string]any{"stop": result}}, nil
			},
		}),
		reg.Register(admin.CommandDef{
			Name:        AdminCommandStatus,
			Description: "show the active robot load-test run",
			Handler: func(_ context.Context, cmd admin.Command) (admin.Result, error) {
				req, err := admin.DecodePayload[StatusRequest](cmd)
				if err != nil {
					return admin.Result{}, err
				}
				return admin.Result{Data: map[string]any{"status": m.Status(req)}}, nil
			},
		}),
		reg.Register(admin.CommandDef{
			Name:        AdminCommandHistory,
			Description: "list finished robot load-test runs",
			Handler: func(_ context.Context, cmd admin.Command) (admin.Result, error) {
				req, err := admin.DecodePayload[HistoryRequest](cmd)
				if err != nil {
					return admin.Result{}, err
				}
				return admin.Result{Data: map[string]any{"history": m.History(req)}}, nil
			},
		}),
		reg.Register(admin.CommandDef{
			Name:        AdminCommandReport,
			Description: "render the markdown report for a finished run (default: latest)",
			Handler: func(_ context.Context, cmd admin.Command) (admin.Result, error) {
				req, err := admin.DecodePayload[ReportRequest](cmd)
				if err != nil {
					return admin.Result{}, err
				}
				report, err := m.Report(req)
				if err != nil {
					return admin.Result{}, err
				}
				return admin.Result{Data: map[string]any{"report": report}}, nil
			},
		}),
	)
}

// Report renders the structured + markdown report for one finished run
// (empty RunID selects the most recent).
func (m *Manager) Report(req ReportRequest) (map[string]any, error) {
	if m == nil {
		return nil, errors.New("robot loadtest: manager is nil")
	}
	m.mu.Lock()
	runs := m.historyLocked(0)
	m.mu.Unlock()
	if len(runs) == 0 {
		return nil, errors.New("robot loadtest: no finished runs")
	}
	var snapshot *RunSnapshot
	if strings.TrimSpace(req.RunID) == "" {
		snapshot = &runs[0]
	} else {
		for i := range runs {
			if runs[i].RunID == req.RunID {
				snapshot = &runs[i]
				break
			}
		}
	}
	if snapshot == nil {
		return nil, fmt.Errorf("robot loadtest: run %q not in history", req.RunID)
	}
	return map[string]any{
		"run":      *snapshot,
		"markdown": renderMarkdown(*snapshot),
	}, nil
}

func renderMarkdown(run RunSnapshot) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Load-test report — %s\n\n", run.RunID)
	fmt.Fprintf(&b, "- profile: %s · scenario: %s · robots: %d\n", run.Profile, run.Scenario, run.Count)
	fmt.Fprintf(&b, "- state: **%s** (stop: %s)\n", run.State, run.StopReason)
	elapsed := run.EndedAtMS - run.StartedAtMS
	fmt.Fprintf(&b, "- elapsed: %dms\n\n", elapsed)
	fmt.Fprintf(&b, "## Outcome\n\n| started | success | failure | canceled | failure rate |\n| --- | --- | --- | --- | --- |\n| %d | %d | %d | %d | %.2f%% |\n\n",
		run.Stats.Started, run.Stats.Success, run.Stats.Failure, run.Stats.Canceled, run.Stats.FailureRate*100)
	if len(run.QuantilesMS) > 0 {
		fmt.Fprintf(&b, "## Scenario latency\n\n| p50 | p90 | p95 | p99 |\n| --- | --- | --- | --- |\n| %dms | %dms | %dms | %dms |\n\n",
			run.QuantilesMS["p50"], run.QuantilesMS["p90"], run.QuantilesMS["p95"], run.QuantilesMS["p99"])
	}
	if len(run.Thresholds) > 0 {
		b.WriteString("## Thresholds\n\n| metric | max | actual | verdict |\n| --- | --- | --- | --- |\n")
		for _, t := range run.Thresholds {
			verdict := "pass"
			if t.Violated {
				verdict = "**FAIL**"
			}
			fmt.Fprintf(&b, "| %s | %g | %g | %s |\n", t.Metric, t.Max, t.Actual, verdict)
		}
		b.WriteString("\n")
	}
	if run.Error != "" {
		fmt.Fprintf(&b, "## Error\n\n```\n%s\n```\n", run.Error)
	}
	return b.String()
}
