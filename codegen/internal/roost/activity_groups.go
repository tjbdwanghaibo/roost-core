package roost

import (
	"fmt"
	"slices"
	"strings"
)

// activityGroupsFile is the activity groups file of a project that hosts the
// activity coordinator (decision C4, docs/feature/C4-ACTIVITY-GROUPS-FILE-2026-10-06.md):
// which game servers take part in the same server-wide activity. The
// coordinator reads it through activity.groups_file (its sweep groups) and so
// does the game-demo's game (its group and Live candidates); both validate it
// with kit/service/global/activity.LoadGroupsFile.
//
// It is relative like config_data.dir, resolved against the working
// directory: the project root in development, WORKDIR /app in the image (the
// Dockerfile copies it there), the release under systemd (install.sh copies
// it). It is release content like configs/data, not per-environment secret
// config, so compose and k8s use the image's copy; an environment with a
// different grouping mounts its own file over it or points groups_file at one.
const activityGroupsFile = "configs/activity_groups.yaml"

// hostsActivityCoordinator reports whether a service of the project is the
// activity framework service.
func hostsActivityCoordinator(m Manifest) bool {
	for _, service := range m.Services {
		if strings.TrimSpace(service.Framework) == "activity" {
			return true
		}
	}
	return false
}

// defaultActivityGroupSIDs is every sid the generated deployments start the
// first business service with: 1000 locally, under systemd and in k8s; the
// second local process (second-game.sh); and the production compose, which
// numbers services 1000+index. Listing all of them lets each generated way of
// running the project start without an edit; the Live query only counts the
// ones whose process is actually up, so a listed sid that is not running is
// not waited for.
func defaultActivityGroupSIDs(m Manifest) []int {
	sids := []int{1000}
	game := firstBusinessService(m)
	if game == "" {
		return sids
	}
	sids = append(sids, secondGameSID)
	for index, service := range sortedServiceNames(m) {
		if service == game {
			sids = append(sids, 1000+index)
		}
	}
	slices.Sort(sids)
	return slices.Compact(sids)
}

// renderActivityGroups is the starting activity groups file: one group named
// after the project, the same id the game-demo used as a constant before.
// The project owns the file after it is created.
func renderActivityGroups(m Manifest) string {
	var sids []string
	for _, sid := range defaultActivityGroupSIDs(m) {
		sids = append(sids, fmt.Sprint(sid))
	}
	return `# Activity groups: which game servers take part in the same server-wide
# activity. The activity coordinator (activity.groups_file) and the game
# servers that run activities read this one file.
#
#   id         the group's id, the coordinator's Key.GroupID; not "", no "/"
#   game_sids  every game server (its --sid) that may take part; each sid in
#              one group only, at most 64 per group (the coordinator's limit
#              for one activity window), positive int32
#
# An activity window waits for the members whose process is up (the App
# singleton lock's Live query), so listing a server that is not running is
# harmless. A game whose --sid is in no group, or a file that breaks a rule
# above, stops the process at start with the file named.
#
# The sids below are the ones the generated deployments start the game with
# (local and k8s 1000, deploy/dev/second-game.sh 1001, the production compose
# 1000+N). List your real servers here; a deployment that splits its servers
# into independent groups adds a group per set.
groups:
  - id: ` + m.Project.Name + `
    game_sids: [` + strings.Join(sids, ", ") + `]
`
}

// renderShellActivityGroups installs the activity groups file into the release
// next to configs/data, for every service: the coordinator reads it, and so
// may any game service. guard runs before the release directory exists.
func renderShellActivityGroups(m Manifest) (guard, install string) {
	if !hostsActivityCoordinator(m) {
		return "", ""
	}
	guard = `ACTIVITY_GROUPS=${ACTIVITY_GROUPS:-"$ROOT/` + activityGroupsFile + `"}
[ -f "$ACTIVITY_GROUPS" ] || { printf 'activity groups file not found: %s; set ACTIVITY_GROUPS\n' "$ACTIVITY_GROUPS" >&2; exit 2; }
`
	install = `# activity.groups_file is relative (` + activityGroupsFile + `) and resolves under WorkingDirectory, the release.
install -d -m 0755 "$RELEASE_ROOT/configs"
install -m 0644 "$ACTIVITY_GROUPS" "$RELEASE_ROOT/` + activityGroupsFile + `"
`
	return guard, install
}
