module github.com/tjbdwanghaibo/roost-skill/integration/sync-e2e

go 1.27.0

require (
	github.com/tjbdwanghaibo/roost-core v1.12.0
	github.com/tjbdwanghaibo/roost-kit v1.12.0
	github.com/tjbdwanghaibo/roost-skill v1.4.0
)

// The replacement is confined to this integration-test module: it exists to
// exercise the working-tree roost-skill against released roost-core/roost-kit.
replace github.com/tjbdwanghaibo/roost-skill => ../..
