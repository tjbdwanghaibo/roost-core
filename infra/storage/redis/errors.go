package redis

import "errors"

var (
	ErrNil    = errors.New("redis: nil") // key does not exist
	ErrClosed = errors.New("redis: client closed")
)
