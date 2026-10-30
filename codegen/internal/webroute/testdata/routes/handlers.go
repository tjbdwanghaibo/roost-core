// Package routes 是 webroute 生成器的 fixture：webroute_test.go 的 TestGoldenRoutes 用它锁住生成物的全文，
// webroute_test.go 的 TestGeneratedRoutesRunAgainstTheRuntime 把它和生成物、runtime/routes_test.go 放进一次性模块，对当前 roost-core 跑 httptest 端到端。
package routes

import (
	"context"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/infra/network/webroute"
)

type Service struct{}

// SessionHeaders 经嵌入提升：headers= 的字段可以是提升字段。
type SessionHeaders struct {
	PlayerID string `json:"-"`
}

type KeepAliveRequest struct {
	SessionHeaders
	Note  string `json:"note"`
	SID   string `json:"sid"`
	Token string `json:"-"`
}

type KeepAliveResponse struct {
	PlayerID string `json:"player_id"`
	SID      string `json:"sid"`
	Token    string `json:"token"`
	Note     string `json:"note"`
}

//roost:web method=POST path=/player/keepalive body=json headers=x-player-id:PlayerID,X-Game-Sid:SID,Authorization:Token
func handleKeepAlive(ctx context.Context, svc *Service, request KeepAliveRequest) (KeepAliveResponse, error) {
	return KeepAliveResponse{PlayerID: request.PlayerID, SID: request.SID, Token: request.Token, Note: request.Note}, nil
}

type RankResponse struct {
	Season  string   `json:"season"`
	Parts   []string `json:"parts"`
	Entries []string `json:"entries"`
}

//roost:web method=GET path=/rank/info body=raw compress=gzip
func handleRankInfo(ctx context.Context, svc *Service, request webroute.RawRequest) (RankResponse, error) {
	response := RankResponse{Season: request.Query.Get("season"), Parts: request.Query["part"]}
	for i := 0; i < 100; i++ {
		response.Entries = append(response.Entries, strings.Repeat("entry", 4))
	}
	return response, nil
}

type EchoRequest struct {
	Text string `json:"text"`
}

type EchoResponse struct {
	Text string `json:"text"`
}

//roost:web method=POST path=/echo body=json
func handleEcho(ctx context.Context, svc *Service, request EchoRequest) (EchoResponse, error) {
	return EchoResponse{Text: request.Text}, nil
}
