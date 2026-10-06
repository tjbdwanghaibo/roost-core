package activity

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/app"
)

// RR-20261005-01 在 C4 组文件形态下的第三条承诺：候选（一组的成员）不能多到一次 App.Live 查询都报错——
// 那样每一拍开窗都失败，活动永远不开。C4 之前由 game-demo 启动时检查“本服 + activity.game_sids 不超过
// app.SingletonLiveMaxSIDs”兑现（TestActivityRefusesACandidateListNoWindowCouldOpenWith 的
// more-candidates-than-one-live-query）；C4 之后一组至多 MaxExpectedGames 个成员（ParseGroups 加载时拒绝），
// game-demo 用一次 Live 查整组，所以只要组上限不超过 Live 的单次上限，这条就由组上限兑现。两个常量改一个
// 不改另一个时这里先红（game-demo 的 activityGroup 还有一道运行期防线，但那是启动失败，不是加载时点名文件）。
func TestAGroupFitsOneLiveQuery(t *testing.T) {
	if MaxExpectedGames > app.SingletonLiveMaxSIDs {
		t.Fatalf("an activity group may hold %d game servers (MaxExpectedGames) but one App.Live query takes at most %d (app.SingletonLiveMaxSIDs); "+
			"a full group's Live query would fail on every pass and its windows would never open", MaxExpectedGames, app.SingletonLiveMaxSIDs)
	}
}
