# 演练 1b（两个 sid）：静态绑定、跨服赠礼转交、OWN-2 登录到错误 sid、在途赠礼时 kill -9 后接管（6b）。
# 接在 drill1.sh 之后跑（P7 在 sid 1000 上运行）。用法：DRILL_HOME=<私有目录> bash drill1b.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
mark "=== step 6: two sids — P7 (1000) running, start Q1 (1001)"
start_game Q1 1001 config.game1001.yaml
wait_ready Q1 36110 60
mark "keys: $(key 1000) ; $(key 1001)"
SINCE6=$(date -u +%Y-%m-%dT%H:%M:%SZ)
mark "=== step 6a: robots on both sids (since $SINCE6)"
# 只等机器人：start_game 起的游戏进程也是本 shell 的后台作业，裸 wait 会一直等下去（WIP 版卡在这里）。
loadtest C 127.0.0.1:36700 1000 10 100200 & LC=$!
loadtest D 127.0.0.1:36701 1001 10 100300
wait $LC
sleep 5
mark "sagas since 6a: $(sagasince $SINCE6)"
mark "P7 handoffs: left=$(grep -c "left to the sender's sid" $D/logs/P7.log) ran=$(grep -c 'handed over by another process' $D/logs/P7.log); Q1 left=$(grep -c "left to the sender's sid" $D/logs/Q1.log) ran=$(grep -c 'handed over by another process' $D/logs/Q1.log)"
mark "=== step 6c (OWN-2): robots whose roles are bound to 1001 log in at 1000's endpoint"
loadtest E 127.0.0.1:36700 1001 2 100400 -scenario idle -scenarios $D/scenarios-idle -duration 20s
mark "E outcome: $(grep -o 'response code [0-9]*: [^"]*' $D/logs/loadtest-E.log | sort | uniq -c | head -3 | tr '\n' ';')"
bash "$SCRIPTS/drill1-6b.sh" P7 P8
mark "=== drill 1b done"
