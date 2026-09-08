module github.com/tjbdwanghaibo/roost-skill/examples

go 1.27.0

require github.com/tjbdwanghaibo/roost-skill v1.5.0

require (
	github.com/modern-go/gls v0.0.0-20250215024828-78308f6bb19d // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/tjbdwanghaibo/roost-core v1.12.0 // indirect
	go.mongodb.org/mongo-driver/v2 v2.6.0 // indirect
)

// The examples always exercise the working tree.
replace github.com/tjbdwanghaibo/roost-skill => ..
