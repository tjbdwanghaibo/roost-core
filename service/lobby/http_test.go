package lobby

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newHTTPHarness(t *testing.T, origins ...string) (*harness, *httptest.Server) {
	t.Helper()
	clock := &manualClock{now: time.Now()}
	rooms, err := NewMemoryRooms(time.Hour, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, rooms, clock, "")
	server := httptest.NewServer(h.svc.Handler(origins))
	t.Cleanup(server.Close)
	return h, server
}

func call(t *testing.T, server *httptest.Server, method, path, gameToken string, body any) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if gameToken != "" {
		req.Header.Set("Authorization", "Bearer "+gameToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s %s: response is not JSON: %v", method, path, err)
	}
	return resp.StatusCode, out
}

// HTTP 全程：建房 → relay 报告 → 列房 → 加入 → 改状态 → 离开，答复形状是 {room, relay_addr, token, expires_at}；
// 错误答 {error, message} 与对应状态码。
func TestHTTPRoomLifecycle(t *testing.T) {
	h, server := newHTTPHarness(t)
	status, created := call(t, server, http.MethodPost, "/rooms", "gt-host", map[string]any{
		"game": "party", "mode": "duel", "name": "fun", "max_members": 2, "password": "pw", "attrs": map[string]any{"map": "x"},
	})
	if status != http.StatusOK {
		t.Fatalf("create: %d %v", status, created)
	}
	room := created["room"].(map[string]any)
	id := room["id"].(string)
	if created["relay_addr"] != "10.0.0.2:7100" || created["token"] == "" || created["expires_at"] == nil ||
		room["has_password"] != true || room["password_hash"] != nil {
		t.Fatalf("create reply = %v", created)
	}
	h.report(t, RoomEvent{Type: EventHeartbeat, Room: id, Relay: "relay-b", Seq: 1, Host: "acc-host", Online: []string{"acc-host"}})

	status, listed := call(t, server, http.MethodGet, "/rooms?game=party&mode=duel", "gt-alice", nil)
	if rooms := listed["rooms"].([]any); status != http.StatusOK || len(rooms) != 1 || rooms[0].(map[string]any)["id"] != id {
		t.Fatalf("list: %d %v", status, listed)
	}
	if status, reply := call(t, server, http.MethodPost, "/rooms/"+id+"/join", "gt-alice", map[string]any{"game": "party", "password": "nope"}); status != http.StatusForbidden || reply["error"] != "wrong_password" {
		t.Fatalf("wrong password: %d %v", status, reply)
	}
	status, joined := call(t, server, http.MethodPost, "/rooms/"+id+"/join", "gt-alice", map[string]any{"game": "party", "password": "pw"})
	if status != http.StatusOK || joined["token"] == "" {
		t.Fatalf("join: %d %v", status, joined)
	}
	if status, reply := call(t, server, http.MethodPost, "/rooms/"+id+"/join", "gt-bob", map[string]any{"game": "party", "password": "pw"}); status != http.StatusConflict || reply["error"] != "room_full" {
		t.Fatalf("full: %d %v", status, reply)
	}
	if status, reply := call(t, server, http.MethodPost, "/rooms/"+id+"/state", "gt-alice", map[string]any{"game": "party", "state": "started"}); status != http.StatusForbidden || reply["error"] != "not_host" {
		t.Fatalf("state by member: %d %v", status, reply)
	}
	if status, reply := call(t, server, http.MethodPost, "/rooms/"+id+"/state", "gt-host", map[string]any{"game": "party", "state": "started"}); status != http.StatusOK || reply["room"].(map[string]any)["state"] != "started" {
		t.Fatalf("state by host: %d %v", status, reply)
	}
	if status, reply := call(t, server, http.MethodPost, "/rooms/"+id+"/leave", "gt-alice", map[string]any{"game": "party"}); status != http.StatusOK || reply["token"] != nil {
		t.Fatalf("leave: %d %v", status, reply)
	}
	if status, reply := call(t, server, http.MethodPost, "/rooms/nope/join", "gt-alice", map[string]any{"game": "party"}); status != http.StatusNotFound || reply["error"] != "room_not_found" {
		t.Fatalf("missing room: %d %v", status, reply)
	}
}

func TestHTTPRefusesMissingOrForeignTokensAndBadBodies(t *testing.T) {
	_, server := newHTTPHarness(t)
	cases := []struct {
		name, method, path, token string
		body                      any
		status                    int
		errName                   string
	}{
		{"no token", http.MethodPost, "/rooms", "", map[string]any{"game": "party", "mode": "duel"}, 401, "unauthorized"},
		{"other game's token", http.MethodPost, "/rooms", "gt-dave", map[string]any{"game": "party", "mode": "duel"}, 401, "unauthorized"},
		{"game not served", http.MethodGet, "/rooms?game=chess&mode=duel", "gt-host", nil, 403, "game_not_served"},
		{"bad limit", http.MethodGet, "/rooms?game=party&mode=duel&limit=x", "gt-host", nil, 400, "bad_request"},
		{"bad json", http.MethodPost, "/rooms", "gt-host", "not an object", 400, "bad_request"},
		{"bad transport", http.MethodPost, "/rooms", "gt-host", map[string]any{"game": "party", "mode": "duel", "transport": "udp"}, 400, "bad_request"},
	}
	for _, tc := range cases {
		status, reply := call(t, server, tc.method, tc.path, tc.token, tc.body)
		if status != tc.status || reply["error"] != tc.errName {
			t.Errorf("%s: %d %v, want %d %s", tc.name, status, reply, tc.status, tc.errName)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/rooms", strings.NewReader(`{"game":"party","name":"`+strings.Repeat("x", MaxRequestBytes)+`"}`))
	req.Header.Set("Authorization", "Bearer gt-host")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body: %d, want 413", resp.StatusCode)
	}
}

// CORS：配置里的 Origin 拿到 CORS 头、预检答 204；别的 Origin 拿不到头、预检 403。
func TestHTTPCORSAllowsOnlyConfiguredOrigins(t *testing.T) {
	_, server := newHTTPHarness(t, "https://play.example.com")
	preflight := func(origin string) *http.Response {
		req, _ := http.NewRequest(http.MethodOptions, server.URL+"/rooms", nil)
		req.Header.Set("Origin", origin)
		req.Header.Set("Access-Control-Request-Method", "POST")
		req.Header.Set("Access-Control-Request-Headers", "authorization, content-type")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	ok := preflight("https://play.example.com")
	if ok.StatusCode != http.StatusNoContent || ok.Header.Get("Access-Control-Allow-Origin") != "https://play.example.com" ||
		!strings.Contains(ok.Header.Get("Access-Control-Allow-Headers"), "Authorization") {
		t.Fatalf("allowed preflight: %d %v", ok.StatusCode, ok.Header)
	}
	denied := preflight("https://evil.example.com")
	if denied.StatusCode != http.StatusForbidden || denied.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("denied preflight: %d %v", denied.StatusCode, denied.Header)
	}
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/rooms?game=party&mode=duel", nil)
	req.Header.Set("Origin", "https://play.example.com")
	req.Header.Set("Authorization", "Bearer gt-host")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Access-Control-Allow-Origin") != "https://play.example.com" {
		t.Fatalf("simple request from an allowed origin: %d %v", resp.StatusCode, resp.Header)
	}

	_, open := newHTTPHarness(t, "*")
	req, _ = http.NewRequest(http.MethodOptions, open.URL+"/rooms", nil)
	req.Header.Set("Origin", "https://anything.example.org")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent || resp.Header.Get("Access-Control-Allow-Origin") != "https://anything.example.org" {
		t.Fatalf("wildcard preflight: %d %v", resp.StatusCode, resp.Header)
	}
}
