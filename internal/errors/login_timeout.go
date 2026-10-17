package errors

import "github.com/tjbdwanghaibo/roost-core/infra/base/errcode"

// ErrLoginTimeout: taking the player into service (ownership claim + load)
// did not finish within player_access.tcp.login_timeout. Most often the
// player's last changes have not reached the store yet because the store is
// unavailable. Nothing was undone; the client retries the login.
var ErrLoginTimeout = errcode.Define(100021, "login_timeout", "login timed out, try again")
