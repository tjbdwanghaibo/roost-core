#!/usr/bin/env bash
# 需要真实 Redis Cluster 的 integration 套件（文件里以 ROOST_REVIEW_CLUSTER 为准入）。
# CI 的 Redis job 只有单实例、dataengine-env.sh 也不起集群，所以这些用例在 CI 里
# 一律 skip；这里是它们唯一的执行入口，由审查 / 修复的人在本机对着 3 主 3 从集群跑。
# 包清单由 integration_coverage_promises_test.go 钉在磁盘上的文件：每个含
# ROOST_REVIEW_CLUSTER 测试文件的包都要列在这里，没有的包不能列（那一格是空绿）。
#
# 用法：ROOST_REVIEW_CLUSTER=127.0.0.1:7000,...,127.0.0.1:7005 bash kit/scripts/integration/redis-cluster-suites.sh
# 集群里的键会按每次运行的随机前缀写入并清理；不要指向共享的集群。
set -euo pipefail
: "${ROOST_REVIEW_CLUSTER:?comma-separated addresses of a Redis Cluster reserved for tests}"
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$repo_dir"
export GOWORK=off
# -p 1：这些包共用同一个集群，串行避免互相干扰。同包里以 REDIS_ADDR / ROOST_DATAENGINE_IT
# 准入的用例在这里照常 skip，它们各自的入口是 ci.yml 的 Redis job 与 dataengine-env.sh test。
# RR-20261005-NC-201：“照常 skip”要靠这里清掉准入变量，不能靠调用者的环境。之前在 source 过
# 隔离环境 env.sh 的 shell 里运行，./redis/driver 的 toxic 用例与 ./remoteentity 的进程杀死 /
# 分区用例会整包跑到共享隔离环境上（没有 -run 限制），还会 /reset 共享 toxiproxy。
unset ROOST_DATAENGINE_IT REDIS_ADDR
go test -tags=integration -count=1 -p 1 ./kit/redis ./kit/service/mail ./redis/driver ./remoteentity ./service/mail
