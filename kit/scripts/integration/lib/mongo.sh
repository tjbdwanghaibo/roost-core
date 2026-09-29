#!/usr/bin/env bash

readonly ROOST_IT_MONGO_REPLICA_SET="roost-it"

# 基准端口 27117-27119，经 roost_it_port 统一平移（ROOST_IT_PORT_OFFSET）。
mongo_node_port() {
	roost_it_port "$((27116 + $1))"
}

readonly ROOST_IT_MONGO_URI="mongodb://127.0.0.1:$(mongo_node_port 1),127.0.0.1:$(mongo_node_port 2),127.0.0.1:$(mongo_node_port 3)/?replicaSet=$ROOST_IT_MONGO_REPLICA_SET"

mongo_node_dir() {
	printf '%s/mongo-%d\n' "$ROOST_IT_ROOT" "$1"
}

mongo_node_pid_file() {
	printf '%s/mongod.pid\n' "$(mongo_node_dir "$1")"
}

mongo_ping_port() {
	mongosh --quiet --host 127.0.0.1 --port "$1" --eval \
		'const r=db.adminCommand({ping:1}); quit(r.ok===1?0:1)' >/dev/null 2>&1
}

mongo_start_node() {
	local index="$1" port node pid_file status=0
	port="$(mongo_node_port "$index")"
	node="$(mongo_node_dir "$index")"
	pid_file="$(mongo_node_pid_file "$index")"
	if read_owned_pid "$pid_file" >/dev/null 2>&1; then
		return 0
	fi
	require_port_available_or_owned "$port" "$pid_file" "mongo-$index"
	mkdir -p "$node/data"
	rm -f -- "$pid_file"
	roost_it_log "starting mongo-$index on 127.0.0.1:$port"
	mongod \
		--replSet "$ROOST_IT_MONGO_REPLICA_SET" \
		--port "$port" \
		--bind_ip 127.0.0.1 \
		--dbpath "$node/data" \
		--logpath "$node/mongod.log" \
		--pidfilepath "$pid_file" \
		--oplogSize 128 \
		--setParameter shutdownTimeoutMillisForSignaledShutdown=1000 \
		--fork >/dev/null
	wait_until 30 "mongo-$index ping" mongo_ping_port "$port" || status=$?
	return "$status"
}

mongo_replica_initialized() {
	mongosh --quiet --host 127.0.0.1 --port "$(mongo_node_port 1)" --eval \
		'try { const s=rs.status(); quit(s.ok===1?0:1) } catch (e) { quit(1) }' >/dev/null 2>&1
}

mongo_initiate_replica() {
	if mongo_replica_initialized; then
		return 0
	fi
	roost_it_log "initializing Mongo replica set $ROOST_IT_MONGO_REPLICA_SET"
	# 成员地址就是驱动发现后要拨的地址，必须与平移后的端口一致。
	mongosh --quiet --host 127.0.0.1 --port "$(mongo_node_port 1)" --eval "
const config={_id:\"$ROOST_IT_MONGO_REPLICA_SET\",members:[
  {_id:0,host:\"127.0.0.1:$(mongo_node_port 1)\",priority:3},
  {_id:1,host:\"127.0.0.1:$(mongo_node_port 2)\",priority:2},
  {_id:2,host:\"127.0.0.1:$(mongo_node_port 3)\",priority:1}
]};
const result=rs.initiate(config);"'
if (result.ok !== 1 && result.codeName !== "AlreadyInitialized") { printjson(result); quit(1); }
' >/dev/null
}

mongo_replica_ready() {
	mongosh "$ROOST_IT_MONGO_URI" --quiet --eval '
try {
  const s=rs.status();
  const primary=s.members.filter(m => m.health===1 && m.stateStr==="PRIMARY").length;
  const healthy=s.members.filter(m => m.health===1 && (m.stateStr==="PRIMARY" || m.stateStr==="SECONDARY")).length;
  quit(primary===1 && healthy===3 ? 0 : 1);
} catch (e) { quit(1); }
' >/dev/null 2>&1
}

mongo_up() {
	local index
	for index in 1 2 3; do
		mongo_start_node "$index"
	done
	mongo_initiate_replica
	wait_until 60 "Mongo replica set ready" mongo_replica_ready
}

mongo_primary_node() {
	local primary
	primary="$(mongosh "$ROOST_IT_MONGO_URI" --quiet --eval 'const h=db.adminCommand({hello:1}); if(h.primary) print(h.primary)' 2>/dev/null)"
	case "$primary" in
		*":$(mongo_node_port 1)") printf 'mongo-1\n' ;;
		*":$(mongo_node_port 2)") printf 'mongo-2\n' ;;
		*":$(mongo_node_port 3)") printf 'mongo-3\n' ;;
		*) return 1 ;;
	esac
}

mongo_fault_primary() {
	local node index
	node="$(mongo_primary_node)"
	index="${node#mongo-}"
	stop_owned_pid "$(mongo_node_pid_file "$index")" "$node"
	printf '%s\n' "$node"
}

mongo_heal() {
	local index
	for index in 1 2 3; do
		mongo_start_node "$index"
	done
	wait_until 90 "Mongo replica set healed" mongo_replica_ready
}

mongo_down() {
	local index
	for index in 3 2 1; do
		stop_owned_pid "$(mongo_node_pid_file "$index")" "mongo-$index"
	done
}

mongo_status() {
	if ! mongo_replica_ready; then
		roost_it_error "Mongo replica set is not ready"
		return 1
	fi
	mongosh "$ROOST_IT_MONGO_URI" --quiet --eval '
const s=rs.status();
print(s.members.map(m => m.name+"="+m.stateStr).join(" "));
' 2>/dev/null
}
