package movement

import (
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// The same owner can expose both coordinate vocabularies. Binding one must
// neither guess which one to use nor touch the other one.
type tracedStore struct {
	scene      int64
	facing     int32
	posX, posY int64
	x, y       int64
	calls      []string
}

func (s *tracedStore) GetSceneID() int64  { return s.scene }
func (s *tracedStore) SetSceneID(v int64) { s.scene = v; s.calls = append(s.calls, "scene") }
func (s *tracedStore) GetFacing() int32   { return s.facing }
func (s *tracedStore) SetFacing(v int32)  { s.facing = v; s.calls = append(s.calls, "facing") }
func (s *tracedStore) GetPosX() int64     { return s.posX }
func (s *tracedStore) SetPosX(v int64)    { s.posX = v; s.calls = append(s.calls, "posX") }
func (s *tracedStore) GetPosY() int64     { return s.posY }
func (s *tracedStore) SetPosY(v int64)    { s.posY = v; s.calls = append(s.calls, "posY") }
func (s *tracedStore) GetX() int64        { return s.x }
func (s *tracedStore) SetX(v int64)       { s.x = v; s.calls = append(s.calls, "x") }
func (s *tracedStore) GetY() int64        { return s.y }
func (s *tracedStore) SetY(v int64)       { s.y = v; s.calls = append(s.calls, "y") }

func TestDAOFieldsResolveCurrentStoreAndHonorPublicationChoice(t *testing.T) {
	for _, tc := range []struct {
		name string
		bind func(func() *tracedStore, func()) Fields
		x, y string
	}{
		{"PosXY", BindPosXY[*tracedStore], "posX", "posY"},
		{"XY", BindXY[*tracedStore], "x", "y"},
	} {
		for _, mode := range []string{"manual", "observer"} {
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				cur := &tracedStore{}
				lookups := 0
				var publish func()
				want := []string{"scene", tc.x, tc.y, "facing"}
				if mode == "manual" {
					publish = func() { cur.calls = append(cur.calls, "publish") }
					want = []string{"scene", "publish", tc.x, tc.y, "publish", "facing", "publish"}
				}
				f := tc.bind(func() *tracedStore { lookups++; return cur }, publish)
				at := spatial.Point{X: -91, Y: 37}
				for _, scene := range []int64{45, 67} {
					cur = &tracedStore{} // e.g. the nested DAO was restored/replaced
					f.SetSceneID(scene)
					f.SetPos(at)
					f.SetFacing(32456)
					if f.SceneID() != scene || f.Pos() != at || f.Facing() != 32456 || !slices.Equal(cur.calls, want) {
						t.Fatalf("binding failed for scene %d: store %+v, want writes %v", scene, cur, want)
					}
				}
				if lookups != 12 {
					t.Errorf("resolved storage %d times, want once per Fields operation", lookups)
				}
			})
		}
	}
}
