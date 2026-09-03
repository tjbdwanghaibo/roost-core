// robotdemo 是机器人框架的端到端演示：内置一个回环 echo 服务器（真实 TCP +
// 长度前缀帧），用 YAML 剧本驱动一组 bot 做闭环压测，SLO 阈值判定后输出含
// 分位数的 Markdown 报告。
//
// 运行：go run ./robotdemo
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"time"

	"github.com/tjbdwanghaibo/roost-core/robot"
	"github.com/tjbdwanghaibo/roost-core/robot/action"
	"github.com/tjbdwanghaibo/roost-core/robot/loadtest"
	"github.com/tjbdwanghaibo/roost-core/robot/protocol"
	"github.com/tjbdwanghaibo/roost-core/robot/runner"
	"github.com/tjbdwanghaibo/roost-core/robot/scenario"
	"github.com/tjbdwanghaibo/roost-core/robot/transport"
)

const msgLogin, msgBuy = 1, 2

type loginReq struct {
	PlayerID int64 `json:"player_id"`
}
type loginResp struct {
	Code    int32 `json:"code"`
	SceneID int64 `json:"scene_id"`
}

func (r *loginResp) GetCode() int32 { return r.Code }

type buyReq struct {
	ItemID int32 `json:"item_id"`
	Count  int32 `json:"count"`
}
type buyResp struct {
	Code int32 `json:"code"`
	Gold int64 `json:"gold"`
}

func (r *buyResp) GetCode() int32 { return r.Code }

// startServer 是被压测的“游戏服”：login 返回场景号，buy 扣钱。
func startServer() (string, func()) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				for {
					packet, err := transport.ReadPacketFrom(conn, 0)
					if err != nil {
						return
					}
					var payload []byte
					switch packet.MsgID {
					case msgLogin:
						payload, _ = json.Marshal(&loginResp{SceneID: 7})
					case msgBuy:
						var req buyReq
						_ = json.Unmarshal(packet.Payload, &req)
						payload, _ = json.Marshal(&buyResp{Gold: int64(req.ItemID) * int64(req.Count)})
					}
					_ = transport.WritePacketsTo(conn, []*transport.Packet{{MsgID: packet.MsgID, Seq: packet.Seq, Payload: payload}})
				}
			}()
		}
	}()
	return listener.Addr().String(), func() { _ = listener.Close() }
}

// 剧本是 YAML：热改无需重新编译。
const spec = `
scenarios:
  - name: shopping
    node:
      sequence:
        - action: connect
        - action: login
        - loop:
            times: 5
            node:
              random:
                - {weight: 8, node: {action: buy, param: {item_id: 3, count: 2}}}
                - {weight: 2, node: {action: buy, param: {item_id: 9, count: 1}}}
        - wait: 5ms
`

var sceneID = robot.Key[int64]("scene_id")

func main() {
	endpoint, stop := startServer()
	defer stop()

	// —— 业务注册面：两条泛型 call + YAML 剧本，零模板代码 ——
	protocols := protocol.NewRegistry(protocol.JSONCodec{})
	actions := action.NewRegistry()
	action.MustRegisterCall[loginReq, loginResp](actions, protocols, "login", msgLogin,
		action.OnResp(func(rb *robot.Context, resp *loginResp) error {
			sceneID.Set(rb, resp.SceneID) // 唯一的业务代码：记住场景
			return nil
		}))
	action.MustRegisterCall[buyReq, buyResp](actions, protocols, "buy", msgBuy)
	scenarios := scenario.NewRegistry()
	if err := scenario.RegisterSpec(scenarios, []byte(spec)); err != nil {
		log.Fatal(err)
	}

	manager := loadtest.New(loadtest.Config{
		AllowAdminStart: true,
		RunnerOptions: []runner.Option{
			runner.WithActionRegistry(actions),
			runner.WithScenarioRegistry(scenarios),
			runner.WithProtocolRegistry(protocols),
		},
		Profiles: map[string]loadtest.Profile{
			"demo": {
				Run: runner.Config{
					Executor:  runner.ExecutorPool,
					Count:     200,
					Scenario:  "shopping",
					Ramp:      runner.Ramp{Step: 50, Interval: 10 * time.Millisecond},
					Transport: transport.Config{Endpoint: endpoint},
				},
				Thresholds: []loadtest.Threshold{
					{Metric: "error_rate", Max: 0.01},
					{Metric: "p99", Max: 1.0},
				},
			},
		},
	})

	if _, err := manager.Start(context.Background(), loadtest.StartRequest{Profile: "demo"}); err != nil {
		log.Fatal(err)
	}
	for {
		if len(manager.History(loadtest.HistoryRequest{Limit: 1}).Runs) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	report, err := manager.Report(loadtest.ReportRequest{})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(report["markdown"])
}
