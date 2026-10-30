// Package routes 是 webroute 生成器的 fixture：webroute_test.go 的 TestGoldenRoutes 用它锁住生成物的全文，
// webroute_test.go 的 TestGeneratedRoutesRunAgainstTheRuntime 把它和生成物、runtime/routes_test.go 放进一次性模块，对当前 roost-core 跑 httptest 端到端。
package routes

import (
	"context"
	"errors"
	"net/http"
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

// statusError 与 legacyErrors 模拟下游自带的错误契约（D6）：错误链上有 HTTPStatus() 时用它，否则 400；
// 错误体是 {"ok":false,"error":<文本>}。errors=legacyErrors 让生成代码改用这个映射。
type statusError struct {
	status int
	err    error
}

func (e *statusError) Error() string   { return e.err.Error() }
func (e *statusError) Unwrap() error   { return e.err }
func (e *statusError) HTTPStatus() int { return e.status }

var legacyErrors webroute.ErrorMapper = func(err error) (int, any) {
	status := http.StatusBadRequest
	var withStatus interface{ HTTPStatus() int }
	if errors.As(err, &withStatus) {
		status = withStatus.HTTPStatus()
	}
	return status, map[string]any{"ok": false, "error": err.Error()}
}

type FusionRequest struct {
	Fail   bool `json:"fail"`
	Status int  `json:"status"`
}

// FusionResponse 用 HTTPSuccessStatus 决定成功状态（status=response）：例如排队中回 202。
type FusionResponse struct {
	Status int `json:"status"`
}

func (r FusionResponse) HTTPSuccessStatus() int { return r.Status }

//roost:web method=POST path=/fusion/start body=json errors=legacyErrors status=response
func handleFusionStart(ctx context.Context, svc *Service, request FusionRequest) (FusionResponse, error) {
	if request.Fail {
		return FusionResponse{}, &statusError{status: http.StatusConflict, err: errors.New("fusion already running")}
	}
	return FusionResponse{Status: request.Status}, nil
}

//roost:web method=POST path=/fusion/create body=json status=201
func handleFusionCreate(ctx context.Context, svc *Service, request FusionRequest) (FusionResponse, error) {
	if request.Fail {
		return FusionResponse{}, errors.New("database down")
	}
	return FusionResponse{Status: http.StatusAccepted}, nil
}

type FusionList struct {
	Status int      `json:"status"`
	Items  []string `json:"items"`
}

func (l FusionList) HTTPSuccessStatus() int { return l.Status }

//roost:web method=GET path=/fusion/list body=raw compress=gzip errors=legacyErrors status=response
func handleFusionList(ctx context.Context, svc *Service, request webroute.RawRequest) (FusionList, error) {
	if request.Query.Get("missing") != "" {
		return FusionList{}, &statusError{status: http.StatusNotFound, err: errors.New("no such list")}
	}
	list := FusionList{Status: http.StatusPartialContent}
	for i := 0; i < 100; i++ {
		list.Items = append(list.Items, strings.Repeat("item", 4))
	}
	return list, nil
}
