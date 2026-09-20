//go:build protocoldef

package protocoldef

// ListMailRequest pages the player's mailbox, newest first. Cursor is the
// NextCursor of the previous page (empty for the first); Limit zero means the
// mail service's default page.
type ListMailRequest struct {
	Cursor string `pb:"1"`
	Limit  int32  `pb:"2"`
}

// MailEntry is one mail as the client sees it: the envelope plus this
// player's state for it. Claimable is the mail service's answer to "would a
// claim succeed right now", so the client does not guess from Status.
type MailEntry struct {
	MailID        string `pb:"1"`
	Subject       string `pb:"2"`
	Body          string `pb:"3"`
	Status        string `pb:"4"`
	HasAttachment bool   `pb:"5"`
	Claimable     bool   `pb:"6"`
	ExpiresAtUnix int64  `pb:"7"`
}

type ListMailResponse struct {
	Code       int32       `pb:"1"`
	Reason     string      `pb:"2"`
	Mails      []MailEntry `pb:"3"`
	NextCursor string      `pb:"4"`
	Unread     int32       `pb:"5"`
}

//roost:protocol group=game handler=player
type ListMailProtocol interface {
	//roost:msg id=10008
	ListMail(ListMailRequest) ListMailResponse
}
