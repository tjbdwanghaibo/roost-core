package relay

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/service/lobby"
	domain "github.com/tjbdwanghaibo/roost-core/service/relay"
)

// relay 与 lobby 两边各写一份契约（不互相导入业务包）；这里把两份钉在一起：主题、事件名、命令名、JSON 字段。
func TestRelayMatchesTheLobbyContract(t *testing.T) {
	if EventSubject("s") != lobby.EventSubject("s") || CommandSubject("s", "relay-1") != lobby.RelayCommandSubject("s", "relay-1") {
		t.Fatal("NATS subjects differ from the lobby's")
	}
	if commandLeave != lobby.CommandLeave {
		t.Fatal("leave command name differs")
	}
	for relayName, lobbyName := range map[string]string{
		domain.EventHeartbeat: lobby.EventHeartbeat, domain.EventMemberJoined: lobby.EventMemberJoined,
		domain.EventMemberLeft: lobby.EventMemberLeft, domain.EventHostChanged: lobby.EventHostChanged, domain.EventClosed: lobby.EventClosed,
	} {
		if relayName != lobbyName {
			t.Fatalf("event %q differs from the lobby's %q", relayName, lobbyName)
		}
	}

	report := domain.Report{Type: domain.EventMemberLeft, Room: "r", Relay: "relay-1", Seq: 7, Online: []string{"host", "b"}, Host: "host", Epoch: 2,
		Account: "a", Slot: 3, Reason: "kicked"}
	data, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var event lobby.RoomEvent
	if err := json.Unmarshal(data, &event); err != nil {
		t.Fatal(err)
	}
	want := lobby.RoomEvent{Type: report.Type, Room: report.Room, Relay: report.Relay, Seq: report.Seq, Online: report.Online, Host: report.Host,
		Epoch: report.Epoch, Account: report.Account}
	if !reflect.DeepEqual(event, want) {
		t.Fatalf("lobby reads %+v, want %+v", event, want)
	}

	advert := Advertisement{Instance: "relay-1", TCPAddr: "10.0.0.5:7100", WSAddr: "wss://relay.example.com/relay", Rooms: 12}
	data, err = json.Marshal(advert)
	if err != nil {
		t.Fatal(err)
	}
	var registered lobby.Relay
	if err := json.Unmarshal(data, &registered); err != nil {
		t.Fatal(err)
	}
	if registered != (lobby.Relay{Instance: advert.Instance, TCPAddr: advert.TCPAddr, WSAddr: advert.WSAddr, Rooms: advert.Rooms}) {
		t.Fatalf("lobby reads the advertisement as %+v", registered)
	}

	data, err = json.Marshal(lobby.RelayCommand{Type: lobby.CommandLeave, Room: "r", Account: "a"})
	if err != nil {
		t.Fatal(err)
	}
	var cmd command
	if err := json.Unmarshal(data, &cmd); err != nil || cmd != (command{Type: commandLeave, Room: "r", Account: "a"}) {
		t.Fatalf("relay reads the lobby command as %+v: %v", cmd, err)
	}
}
