module github.com/tjbdwanghaibo/roost-core/gameplay/skill/integration/sync-e2e

go 1.27.0

require github.com/tjbdwanghaibo/roost-core v1.13.0

// The replacement is confined to this integration-test module: it exists to
// exercise the working-tree gameplay/skill against the rest of this checkout.
replace github.com/tjbdwanghaibo/roost-core => ../../../..
