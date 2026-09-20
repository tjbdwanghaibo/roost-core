package testdata

import "time"

type EventLogin struct {
	PId int64
}

type EventLogout struct {
	PId int64
}

type EventLevelUp struct {
	PId   int64
	Level int32
	At    time.Time
}
