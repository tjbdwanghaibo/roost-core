#!/usr/bin/env bash
# revn04：把第22轮的正式生成消费者（docs/review/evidence/noncore-review-20261005-22）换到隔离真实
# Mongo 副本集上重跑：正式 DAO CLI → Repository → MigrationRunner → 文件 WAL → Projector → MongoStore
# → 新 Manager 重载。只改后端（mongotest → mongo/driver）、库名（编译期常量 db=revn04_n04_review）与集合
# 前缀；测试断言不改。用法：source 隔离环境 env.sh 后 `bash run-real-mongo-consumer.sh <全新输出目录>`。
# 结束后自行删除 revn04_n04_review 库（脚本最后打印删除命令，不自动删）。
set -euo pipefail
out=${1:?usage: run-real-mongo-consumer.sh <new-output-dir>}
here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../../../.." && pwd)
e22=$repo/docs/review/evidence/noncore-review-20261005-22
db=revn04_n04_review
[ ! -e "$out" ] || { echo "use a new output directory" >&2; exit 2; }
mkdir -p "$out/db/def"
conv() { sed -e 's/"review"/realDatabase/g' -e 's#"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"##' \
  -e 's/mongotest\.NewClient()/newRealClient(t)/g' -e 's/\*mongotest\.Client/*realClient/g' -e 's/roost_review_/revn04_review_/g' "$1"; }
sed -e "s/db=review/db=$db/" -e 's/roost_review_/revn04_review_/g' "$e22/consumer-definition.go.txt" > "$out/db/def/record.go"
for f in consumer aggregate nested contention nested-migration; do conv "$e22/$f-test.go.txt" > "$out/db/${f//-/_}_test.go"; done
cp "$here/realclient-test.go.txt" "$out/db/realclient_test.go"
printf 'module example.com/revn04n04\n\ngo 1.27.0\n\nrequire github.com/tjbdwanghaibo/roost-core v0.0.0\n\nreplace github.com/tjbdwanghaibo/roost-core => %s\n' "$repo" > "$out/go.mod"
(cd "$repo" && GOWORK=off go build -o "$out/dao" ./codegen/cmd/dao && "$out/dao" -def "$out/db/def" -out "$out/db" -pkg db > "$out/generate.log" 2>&1)
(cd "$out" && GOWORK=off go mod tidy >/dev/null && GOWORK=off go vet ./db && GOWORK=off go test -race -count=1 -timeout=300s -json ./db > consumer-real.jsonl)
echo "done; drop afterwards: mongosh \"\$ROOST_DATAENGINE_IT_MONGO_URI\" --eval 'db.getSiblingDB(\"$db\").dropDatabase()'"
