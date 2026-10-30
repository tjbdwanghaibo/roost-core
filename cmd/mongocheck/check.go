package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

// guideSection 是每条结论附带的说明位置：DataEngine 设计篇的“DataEngine 对 MongoDB 的最低要求”一节。
const guideSection = "docs/framework/guide/03-dataengine.md#dataengine-对-mongodb-的最低要求"

// requiredLifetimeSeconds 是部署要求的 transactionLifetimeLimitSeconds，与 dataengine.startup_timeout 缺省 60s 配套
// （被 kill -9 的进程留下的事务最晚在 1.5 倍时回收，即 30s，留 30s 余量；wiring/dataengine/mod.go StartupTimeout）。
const requiredLifetimeSeconds = 20

// Level 是一条检查的结论。
type Level string

const (
	Pass Level = "PASS"
	Warn Level = "WARN"
	Fail Level = "FAIL"
)

// Result 是一条检查的输出：名字、结论、看到的值、原因与修复建议。所有字段都不含连接串、主机名或账号。
type Result struct {
	Name   string
	Level  Level
	Value  string
	Reason string
	Fix    string
}

// Facts 是从服务端只读取得的事实。每项带自己的错误：取不到某一项只影响那一条检查。错误在 Evaluate 里只经
// describeError 转成不含连接信息的类别再输出。
type Facts struct {
	// buildInfo
	Version    string
	VersionErr error

	// hello
	SetName               string
	IsMongos              bool
	LogicalSessionMinutes *int64
	HelloErr              error

	// getParameter featureCompatibilityVersion
	FCV    string
	FCVErr error

	// getParameter transactionLifetimeLimitSeconds
	LifetimeSeconds float64
	LifetimeErr     error

	// serverStatus.storageEngine
	StorageEngine string
	Persistent    *bool
	StorageErr    error
}

// Evaluate 把事实逐项判定成结论，顺序固定。判定规则与设计篇“最低要求”一节一一对应，代码依据写在各函数注释里。
func Evaluate(f Facts) []Result {
	return []Result{
		checkVersion(f),
		checkFCV(f),
		checkTopology(f),
		checkSessions(f),
		checkLifetime(f),
		checkStorage(f),
	}
}

// HasFail 报告结论里是否有 FAIL（决定退出码）。
func HasFail(results []Result) bool {
	for _, r := range results {
		if r.Level == Fail {
			return true
		}
	}
	return false
}

// checkVersion：代码用到的特性最晚到 4.4（事务里隐式建集合；Put 投影是 4.2 起的 $replaceWith 管道更新；驱动 v2.6.0 要求 4.2），
// 部署下限定为 5.0（4.4 已于 2024-02 结束官方支持）。
func checkVersion(f Facts) Result {
	r := Result{Name: "server-version"}
	if f.VersionErr != nil {
		r.Level, r.Value = Fail, "unknown"
		r.Reason = "buildInfo 失败：" + describeError(f.VersionErr)
		r.Fix = "确认连接串、网络与账号；buildInfo 不需要额外权限"
		return r
	}
	r.Value = f.Version
	major, minor, ok := parseMajorMinor(f.Version)
	switch {
	case !ok:
		r.Level = Warn
		r.Reason = "无法解析版本号"
		r.Fix = "人工确认服务端是 MongoDB 5.0 及以上"
	case major < 4 || (major == 4 && minor < 4):
		r.Level = Fail
		r.Reason = "低于代码硬底 4.4：事务里第一次写某个集合时隐式建集合要求 4.4 起"
		r.Fix = "升级到 MongoDB 5.0 及以上（推荐 7.0 / 8.0）"
	case major == 4:
		r.Level = Warn
		r.Reason = "4.4 满足代码硬底，但已于 2024-02 结束官方支持，低于部署下限 5.0"
		r.Fix = "升级到 MongoDB 5.0 及以上（推荐 7.0 / 8.0）"
	default:
		r.Level = Pass
		r.Reason = "不低于部署下限 5.0"
	}
	return r
}

// checkFCV：事务里隐式建集合要求 featureCompatibilityVersion 4.4 起（MongoDB 文档 “Create Collections and Indexes in
// a Transaction”）。FCV 低于二进制版本说明升级没做完，按部署下限的同一口径判定。
func checkFCV(f Facts) Result {
	r := Result{Name: "feature-compatibility-version"}
	if f.FCVErr != nil {
		r.Level, r.Value = Warn, "unknown"
		r.Reason = "getParameter featureCompatibilityVersion 失败：" + describeError(f.FCVErr)
		if f.IsMongos {
			r.Fix = "经 mongos 读不到时，在每个分片的主节点上执行 db.adminCommand({getParameter: 1, featureCompatibilityVersion: 1})"
		} else {
			r.Fix = "给检查账号 clusterMonitor 角色（含 getParameter），或由 DBA 执行 db.adminCommand({getParameter: 1, featureCompatibilityVersion: 1})"
		}
		return r
	}
	r.Value = f.FCV
	major, minor, ok := parseMajorMinor(f.FCV)
	binMajor, binMinor, binOK := parseMajorMinor(f.Version)
	switch {
	case !ok:
		r.Level = Warn
		r.Reason = "无法解析 featureCompatibilityVersion"
		r.Fix = "人工确认 FCV 不低于 5.0"
	case major < 4 || (major == 4 && minor < 4):
		r.Level = Fail
		r.Reason = "FCV 低于 4.4：事务里不能隐式建集合，新建实体的多集合首个提交会失败"
		r.Fix = "db.adminCommand({setFeatureCompatibilityVersion: \"<当前主版本>\"})（7.0 起还要加 confirm: true）"
	case major == 4:
		r.Level = Warn
		r.Reason = "FCV 4.4 满足代码硬底，低于部署下限 5.0"
		r.Fix = "升级服务端并把 FCV 设到当前主版本"
	case binOK && (major != binMajor || minor != binMinor):
		r.Level = Warn
		r.Reason = fmt.Sprintf("FCV 与服务端版本 %d.%d 不一致，升级尚未完成", binMajor, binMinor)
		r.Fix = "确认各成员都已升级后执行 setFeatureCompatibilityVersion 设到当前主版本"
	default:
		r.Level = Pass
		r.Reason = "不低于 5.0，与服务端版本一致"
	}
	return r
}

// checkTopology 与框架启动检查同一规则（infra/storage/mongo/driver/client.go ValidateDeployment，mongo.require_replica_set
// 缺省 true）：hello 有 setName（副本集）或 msg 为 isdbgrid（mongos）才接受。投影的事务只在副本集或分片集群上可用。
func checkTopology(f Facts) Result {
	r := Result{Name: "topology"}
	if f.HelloErr != nil {
		r.Level, r.Value = Fail, "unknown"
		r.Reason = "hello 失败：" + describeError(f.HelloErr)
		r.Fix = "确认连接串、网络与账号；hello 不需要额外权限"
		return r
	}
	switch {
	case f.SetName != "":
		r.Level, r.Value = Pass, "replica-set"
		r.Reason = "副本集（单节点副本集即可），事务可用"
	case f.IsMongos:
		r.Level, r.Value = Warn, "sharded-cluster (mongos)"
		r.Reason = "框架启动检查接受 mongos，但仓库只在副本集上验证过，分片集群不在验证范围内"
		r.Fix = "生产按副本集部署；坚持分片集群时自行验证 DataEngine 投影、事务与 saga 冷副本"
	default:
		r.Level, r.Value = Fail, "standalone"
		r.Reason = "单机没有事务：多 mutation 记录、批量投影、带回执 / effect / Remote 写的记录都在事务里写；mongo.require_replica_set=true 时 Mongo Mod 启动即拒绝"
		r.Fix = "改成单节点副本集：mongod.conf 加 replication.replSetName（例如 rs0），重启 mongod，执行一次 rs.initiate({_id: \"rs0\", members: [{_id: 0, host: \"<应用可达的主机名>:27017\"}]})，连接串加 replicaSet=rs0；已有数据原地保留"
	}
	return r
}

// checkSessions：ValidateDeployment 无论 require_replica_set 取值都要求 hello 带 logicalSessionTimeoutMinutes（服务端支持会话），
// 否则启动失败。
func checkSessions(f Facts) Result {
	r := Result{Name: "logical-sessions"}
	if f.HelloErr != nil {
		r.Level, r.Value = Fail, "unknown"
		r.Reason = "hello 失败：" + describeError(f.HelloErr)
		r.Fix = "确认连接串、网络与账号"
		return r
	}
	if f.LogicalSessionMinutes == nil {
		r.Level, r.Value = Fail, "absent"
		r.Reason = "hello 没有 logicalSessionTimeoutMinutes：服务端不支持会话，Mongo Mod 启动时 ValidateDeployment 拒绝"
		r.Fix = "检查是否连到了 MongoDB 本身（兼容 MongoDB API 的托管服务不在验证范围内）以及副本集各成员是否都已启动"
		return r
	}
	r.Level, r.Value = Pass, fmt.Sprintf("logicalSessionTimeoutMinutes=%d", *f.LogicalSessionMinutes)
	r.Reason = "服务端支持会话与事务"
	return r
}

// checkLifetime：部署要求 transactionLifetimeLimitSeconds=20。被 kill -9 的进程留下的投影事务持锁到这个时限，服务端最晚在
// 1.5 倍时回收，其间重启排空写同一文档一直写冲突；dataengine.startup_timeout 缺省 60s 按 20s（最晚 30s 回收）算。
// 更大时（服务端缺省 60s，最晚 90s 回收）强杀后的重启可能在启动期限内排不空而失败，判 FAIL；更小不影响这条，
// 但不是验证过的取值，判 WARN。
func checkLifetime(f Facts) Result {
	r := Result{Name: "transaction-lifetime-limit"}
	setting := "mongod.conf 的 setParameter 下写 transactionLifetimeLimitSeconds: 20（或启动参数 --setParameter transactionLifetimeLimitSeconds=20），副本集每个成员都要设；运行中可用 db.adminCommand({setParameter: 1, transactionLifetimeLimitSeconds: 20}) 立即生效，但重启后以配置文件为准"
	if f.LifetimeErr != nil {
		r.Level, r.Value = Warn, "unknown"
		r.Reason = "getParameter transactionLifetimeLimitSeconds 失败：" + describeError(f.LifetimeErr)
		if f.IsMongos {
			r.Fix = "这是 mongod 参数，经 mongos 读不到；在每个分片的每个成员上确认：" + setting
		} else {
			r.Fix = "给检查账号 clusterMonitor 角色（含 getParameter）后重跑，或由 DBA 确认：" + setting
		}
		return r
	}
	r.Value = strconv.FormatFloat(f.LifetimeSeconds, 'f', -1, 64)
	switch {
	case f.LifetimeSeconds == requiredLifetimeSeconds:
		r.Level = Pass
		r.Reason = "等于部署要求 20（只读到当前所连成员的值，其余成员也要相同）"
	case f.LifetimeSeconds > requiredLifetimeSeconds:
		r.Level = Fail
		r.Reason = fmt.Sprintf("大于部署要求 20：被 kill -9 的进程留下的事务最晚 %.0fs 才回收，超过 dataengine.startup_timeout 缺省 60s 所按的 30s，强杀后重启可能排空失败", 1.5*f.LifetimeSeconds)
		r.Fix = setting
	default:
		r.Level = Warn
		r.Reason = "小于部署要求 20：不是验证过的取值，服务端会中止持续更久的事务"
		r.Fix = setting
	}
	return r
}

// checkStorage：DataEngine 的写关注是 majority + journal（driver/client.go NewClient 与 StartSession），要求存储引擎持久化；
// 副本集事务要求 WiredTiger（社区版 4.2 起只有 WiredTiger）。serverStatus 要 clusterMonitor 权限，取不到时 WARN。
func checkStorage(f Facts) Result {
	r := Result{Name: "storage-engine"}
	if f.StorageErr != nil {
		r.Level, r.Value = Warn, "unknown"
		r.Reason = "serverStatus 失败：" + describeError(f.StorageErr)
		r.Fix = "给检查账号 clusterMonitor 角色（含 serverStatus）后重跑，或由 DBA 确认存储引擎是 wiredTiger"
		return r
	}
	r.Value = f.StorageEngine
	switch {
	case f.Persistent != nil && !*f.Persistent:
		r.Level = Fail
		r.Reason = "存储引擎不持久化：DataEngine 的落库确认依赖 journal（写关注 j: true），进程重启后数据会丢"
		r.Fix = "使用 wiredTiger 存储引擎"
	case f.StorageEngine == "wiredTiger":
		r.Level = Pass
		r.Reason = "wiredTiger，持久化"
	case f.IsMongos && f.StorageEngine == "":
		r.Level = Warn
		r.Reason = "mongos 不报告存储引擎"
		r.Fix = "在各分片成员上确认存储引擎是 wiredTiger"
	default:
		r.Level = Warn
		r.Reason = "不是 wiredTiger，不在验证范围内"
		r.Fix = "使用 wiredTiger 存储引擎"
	}
	return r
}

// parseMajorMinor 取 "7.0.45"、"7.0"、"4.4.29-rc1" 这类版本串的主、次版本号。
func parseMajorMinor(version string) (major, minor int, ok bool) {
	parts := strings.SplitN(version, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	minorText := parts[1]
	if cut := strings.IndexFunc(minorText, func(r rune) bool { return r < '0' || r > '9' }); cut >= 0 {
		minorText = minorText[:cut]
	}
	minor, err = strconv.Atoi(minorText)
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

// describeError 把驱动错误转成不含连接信息的类别。驱动的错误文本可能带主机名、端口、副本集成员列表或连接串片段，
// 一律不原样输出：服务端命令错误只输出错误名与错误码，其余按类别描述。
func describeError(err error) string {
	if err == nil {
		return ""
	}
	var safe safeError
	if errors.As(err, &safe) {
		return string(safe)
	}
	var cmd mongo.CommandError
	if errors.As(err, &cmd) && (cmd.Name != "" || cmd.Code != 0) {
		hint := ""
		switch cmd.Code {
		case 13: // Unauthorized
			hint = "，账号缺少权限"
		case 18: // AuthenticationFailed
			hint = "，认证失败"
		}
		return fmt.Sprintf("服务端错误 %s (code %d)%s", cmd.Name, cmd.Code, hint)
	}
	text := err.Error()
	switch {
	case strings.Contains(text, "AuthenticationFailed") || strings.Contains(text, "auth error"):
		return "认证失败（检查账号、密码与 authSource）"
	case errors.Is(err, context.DeadlineExceeded) || mongo.IsTimeout(err):
		return "超时：选不到可用服务器或命令超时（检查网络、TLS、副本集名与成员主机名是否从本机可达）"
	case mongo.IsNetworkError(err):
		return "网络错误"
	case strings.Contains(text, "error parsing uri") || strings.Contains(text, "scheme must be"):
		return "连接串无效"
	}
	return fmt.Sprintf("未分类的错误（%T；原文可能含连接信息，不输出）", err)
}

// safeError 是本工具自己构造、确定不含连接信息的错误，describeError 原样输出。
type safeError string

func (e safeError) Error() string { return string(e) }
