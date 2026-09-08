module github.com/tjbdwanghaibo/roost-core/skill/examples

go 1.27.0

require github.com/tjbdwanghaibo/roost-core v1.13.0

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/modern-go/gls v0.0.0-20250215024828-78308f6bb19d // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/redis/go-redis/v9 v9.22.0 // indirect
	go.mongodb.org/mongo-driver/v2 v2.6.0 // indirect
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

// The examples always exercise the working tree.

replace github.com/tjbdwanghaibo/roost-core => ../..
