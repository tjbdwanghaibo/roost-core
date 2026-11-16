package view

import "example.com/g/db"

type Activity interface{ Rearm() error }

type ActivityHolder interface {
	Activity() Activity
	ActivityBook() *db.ActivityBook
	ActivityScope() string
	Notes() *db.NoteBook
}
