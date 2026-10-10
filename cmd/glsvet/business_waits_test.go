package main

import "testing"

func TestBusinessWaitCheckerDistinguishesAwaitStages(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int
	}{
		{`client.Request(ctx, name, id, nil)`, 1},
		{`nest.RunLocal(ctx, func(){})`, 1},
		{`return nest.Await(func(ctx context.Context)(int,error){time.Sleep(time.Second);return 1,nil},func(p *Player,n int,err error)error{return nil})`, 0},
		{`return nest.Await(func(ctx context.Context)(int,error){return 1,nil},func(p *Player,n int,err error)error{time.Sleep(time.Second);return nil})`, 1},
	} {
		t.Run(tc.body, func(t *testing.T) {
			if got := vetSource(t, "package game\n//roost:nest durability=memory\nfunc Reward(){"+tc.body+"}\n"); got != tc.want {
				t.Fatalf("got=%d want=%d", got, tc.want)
			}
		})
	}
}
