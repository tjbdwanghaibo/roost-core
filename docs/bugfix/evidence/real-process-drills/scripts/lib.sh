# 真实进程演练的公共函数（docs/bugfix/REAL-PROCESS-DRILLS-2026-10-06.md）。用 bash source，不要直接执行。
#
# DRILL_HOME 是演练的私有目录（setup.sh 在里面放私有依赖、生成工程、etcd 与日志），必须先设置：
#   export DRILL_HOME=<私有目录，例如 scratchpad 下的 rpd2>
# 依赖由 scripts/mirror-local.sh 起在 $DRILL_HOME/env（端口偏移 DRILL_OFFSET，缺省 30000）；env.sh 含连接串，
# 只 source、不打印。
: "${DRILL_HOME:?set DRILL_HOME to the private drill directory}"
D=$DRILL_HOME
SCRIPTS=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)   # 下面会 cd 到生成工程，先记下脚本目录
DAO_DB=${DRILL_DAO_DB:-rpd2drill_game}            # 生成工程 db/gen_*_dao.go 的 *DaoDBName（setup.sh 改写）
KEY_PREFIX=${DRILL_KEY_PREFIX:-roost:rpd:singleton} # 生成配置的 singleton.key_prefix（工程名 rpd）
GAME_OPS_PORT=${DRILL_GAME_OPS_PORT:-36100}
cd "$D/rpd" || return 1
export PATH=/opt/homebrew/bin:$PATH
# shellcheck disable=SC1091
source "$D/env/roost-dataengine-it/env.sh"
R="redis-cli -h ${ROOST_DATAENGINE_IT_REDIS_ADDR%:*} -p ${ROOST_DATAENGINE_IT_REDIS_ADDR##*:}"
mkdir -p "$D/logs"

ts() { perl -MTime::HiRes=time -MPOSIX=strftime -e '$t=time; printf "%s.%03d\n", strftime("%H:%M:%S",localtime $t), ($t-int $t)*1000'; }
mark() { echo "$(ts) $*" | tee -a "$D/timeline.txt"; }

# start_game <名字> <sid> <配置文件名> [环境变量赋值...]：在新会话里起 bin/app game，记下 pid 与退出码。
start_game() {
  local name=$1 sid=$2 cfg=$3; shift 3
  rm -f "$D/logs/$name.exit" "$D/logs/$name.pid"
  nohup env "$@" perl -MPOSIX -e "setsid; exec @ARGV" bash -c "bin/app game --sid $sid --config configs/service/$cfg > $D/logs/$name.log 2>&1 & echo \$! > $D/logs/$name.pid; wait \$!; echo \$? > $D/logs/$name.exit" >/dev/null 2>&1 &
  for _ in $(seq 1 100); do [ -s "$D/logs/$name.pid" ] && break; perl -e 'select undef,undef,undef,0.02'; done
  mark "start $name pid=$(cat "$D/logs/$name.pid") sid=$sid config=$cfg $*"
}
pidof() { cat "$D/logs/$1.pid"; }
alive() { kill -0 "$(pidof "$1")" 2>/dev/null; }
# wait_ready <名字> <ops 端口> <超时秒>：/readyz 200 返回 0；进程先退出返回 1；超时返回 2。
wait_ready() {
  local n=$(( $3 * 10 ))
  for _ in $(seq 1 $n); do
    curl -fsS --max-time 1 -o /dev/null "http://127.0.0.1:$2/readyz" 2>/dev/null && { mark "$1 ready (readyz 200)"; return 0; }
    alive "$1" || { sleep 0.3; mark "$1 exited code=$(cat "$D/logs/$1.exit" 2>/dev/null)"; return 1; }
    perl -e 'select undef,undef,undef,0.1'
  done; mark "$1 not ready after $3 s"; return 2
}
# wait_exit <名字> <超时秒>
wait_exit() {
  local n=$(( $2 * 10 ))
  for _ in $(seq 1 $n); do alive "$1" || { sleep 0.3; mark "$1 exited code=$(cat "$D/logs/$1.exit" 2>/dev/null)"; return 0; }; perl -e 'select undef,undef,undef,0.1'; done
  mark "$1 still alive after $2 s"; return 1
}
# key <sid>：单实例锁键的值与剩余 TTL（值是 token|hostname|pid|started_ms，只给运维看）。
key() { local k="$KEY_PREFIX:game:$1"; echo "$k = $($R get "$k") pttl=$($R pttl "$k")"; }
# loadtest <名字> <客户端端点> <sid> <机器人数> <首个玩家 ID> [loadtest 额外参数...]
loadtest() {
  local name=$1 ep=$2 sid=$3 cnt=$4 fp=$5; shift 5
  mark "loadtest $name start endpoint=$ep sid=$sid count=$cnt first=$fp $*"
  bin/loadtest -endpoint "$ep" -count "$cnt" -account-nats "$ROOST_DATAENGINE_IT_NATS_URL" -nats-prefix roost -server-id "$sid" -first-player-id "$fp" "$@" > "$D/logs/loadtest-$name.log" 2>&1; local rc=$?
  mark "loadtest $name end rc=$rc $(grep -o 'runner: stop.*' "$D/logs/loadtest-$name.log" | head -1)"
}
mq() { mongosh --quiet "$ROOST_DATAENGINE_IT_MONGO_URI" --eval "$1"; }
# players <输出文件>：DAO 库里全部玩家的关键字段，按 _id 排序，用来对比停机前后的快照。
players() {
  mq "db.getSiblingDB('$DAO_DB').player.find().sort({_id:1}).forEach(d=>print(JSON.stringify({id:d._id.toString(),level:d.level,exp:d.exp&&d.exp.toString(),gold:d.gold&&d.gold.toString(),items:d.items})))" > "$1"
}
# sagasince <ISO 时间>：此后创建的赠礼 saga 按 from_sid / 状态 / 阶段计数，nonterminal 是还没终结的数量。
sagasince() {
  mq "const c=db.getSiblingDB('saga').getCollection('_sagas'); const since=new Date('$1'); const m={}; let maxA=0, n=0, last=null, nt=[];
  c.find({created_at:{\$gte:since}}).forEach(d=>{n++; const s=JSON.parse(d.data.buffer.toString()); const k=(s.from_sid)+'/st'+d.status+(d.status<4?'/ph'+d.phase+'/step'+d.step:''); m[k]=(m[k]||0)+1; const a=Number(d.attempt); if(a>maxA)maxA=a; if(d.status>=4 && (!last||d.updated_at>last)) last=d.updated_at; if(d.status<4) nt.push(d._id)});
  print('total='+n+' max_attempt='+maxA+' last_terminal='+(last?last.toISOString():'-')+' nonterminal='+nt.length+' '+JSON.stringify(m))"
}
