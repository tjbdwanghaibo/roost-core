package view

type Profile interface{ Level() int32 }

type ProfileHolder interface{ Profile() Profile }

type Task interface{ Done(id int64) bool }

type TaskHolder interface{ Task() Task }
