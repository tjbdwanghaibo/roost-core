#!/usr/bin/env python3
"""在已装 team+facebook 的生成工程上逐条做关键行变异：每条只改一处，跑指定用例，要求它失败并带上预期文字，最后还原。"""
import pathlib, subprocess, sys

root = pathlib.Path(sys.argv[1])
MUTATIONS = [
    ("M1 收件箱未打开时不经 Nest 读", "game/modules/facebook/facebook.go", "if !found || !lease.Shared {", "if false && (!found || !lease.Shared) {",
     "./game/modules/facebook", "TestFacebookInvitesNeverClaimsAnUnopenedFacebook", "unopened facebook"),
    ("M2 已共享时只装进内存", "game/modules/facebook/facebook.go", "if found && lease.Shared {", "if false && found && lease.Shared {",
     "./game/modules/facebook", "TestFacebookEnsureOpensSharedOnce", "calls="),
    ("M3 投递后本进程直接告知", "game/modules/facebook/facebook.go", "\tdispatch(ctx, registry, invite)\n\tif err := publish", "\tif err := publish",
     "./game/modules/facebook", "TestFacebookDeliverRecordsThenTellsEveryGame", "local notices="),
    ("M4 广播告知", "game/modules/facebook/facebook.go", "\tif err := publish(ctx, registry, invite); err != nil {", "\tif err := error(nil); err != nil {",
     "./game/modules/facebook", "TestFacebookDeliverRecordsThenTellsEveryGame", "broadcast="),
    ("M5 告知的键必须是收件人", "game/modules/facebook/facebook.go", "invite.To != msg.Key", "false",
     "./game/modules/facebook", "TestFacebookNoticeFromAnotherGameReachesTheFeature", "notice whose key is not its recipient accepted"),
    ("M6 恰好到期即不列出", "game/components/facebook/facebook.go", "now < invite.GetExpireAt()", "now <= invite.GetExpireAt()",
     "./game/handler", "TestFacebookInviteIsListedUntilItExpires", "invite listed at its deadline"),
    ("M7 收件箱有界", "game/components/facebook/facebook.go", "!renewing && dao.InvitesLen() >= MaxInvites", "!renewing && dao.InvitesLen() > MaxInvites",
     "./game/handler", "TestFacebookInvitesAreBoundedAndPruned", "invite beyond the bound accepted"),
    ("M8 team 邀请提交后经 facebook 投递", "game/controllers/team.go", "if invitation.ExpireAt == 0 {", "if invitation.ExpireAt != 0 {",
     "./game/controllers", "TestTeamEndpointInvitationNoticeFailureDoesNotUndoAuthority", "online invitee not told"),
    ("M9 TeamInvites 只认队伍来源", "game/controllers/team.go", "if !entity.MatchEntityID(note.Source, teamentity.EntityKindTeam) {", "if false {",
     "./game/controllers", "TestTeamEndpointInvitationQueryChecksAuthority", "looked up a non-team source"),
    ("M10 告知只推给本进程在线的被邀请人", "game/controllers/team.go", "transport.ActiveSessions(invite.To) == 0", "false",
     "./game/controllers", "TestTeamEndpointInvitationNoticeFailureDoesNotUndoAuthority", "offline player was pushed"),
]
failed = 0
for label, rel, old, new, pkg, test, evidence in MUTATIONS:
    path = root / rel
    before = path.read_text()
    if before.count(old) != 1:
        print(f"{label}: target count {before.count(old)} in {rel}"); failed += 1; continue
    path.write_text(before.replace(old, new, 1))
    try:
        out = subprocess.run(["go", "test", "-count=1", "-run", f"^{test}$", pkg], cwd=root, capture_output=True, text=True)
    finally:
        path.write_text(before)
    text = out.stdout + out.stderr
    caught = out.returncode != 0 and f"--- FAIL: {test}" in text and evidence in text
    line = next((l.strip() for l in text.splitlines() if evidence in l), "")
    print(f"{'RED ' if caught else 'MISS'} {label} [{rel}] {test}: {line}")
    if not caught:
        failed += 1
        print(text[-2000:])
sys.exit(1 if failed else 0)
