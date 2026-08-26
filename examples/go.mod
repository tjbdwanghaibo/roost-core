module github.com/tjbdwanghaibo/cube-core/examples

go 1.25.0

require github.com/tjbdwanghaibo/cube-core v1.7.0

require (
	github.com/modern-go/gls v0.0.0-20250215024828-78308f6bb19d // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
)

// The examples always exercise the working tree.
replace github.com/tjbdwanghaibo/cube-core => ..
