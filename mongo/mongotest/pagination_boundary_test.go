package mongotest

// RR-20261004-NC-20：页边界、排序后分页与int64大值在Find/Stream两入口一致。

import (
	"context"
	"math"
	"reflect"
	"testing"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

func TestPaginationBoundaryPromises(t *testing.T) {
	cases := []struct {
		name        string
		skip, limit int64
		want        []int64
	}{
		{"all", 0, 0, []int64{1, 2, 3}},
		{"middle", 1, 1, []int64{2}},
		{"last", 2, 2, []int64{3}},
		{"outside", 3, 1, nil},
		{"huge_skip", math.MaxInt64, 1, nil},
		{"huge_limit", 1, math.MaxInt64, []int64{2, 3}},
	}
	for _, method := range []string{"find", "stream"} {
		for _, tc := range cases {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				coll := NewClient().Collection("pagination", "boundary")
				for _, id := range []int64{3, 1, 2} {
					if err := coll.Seed(bson.M{"_id": id}); err != nil {
						t.Fatal(err)
					}
				}
				option := fmongo.FindOption{Sort: bson.D{{Key: "_id", Value: 1}}, Skip: tc.skip, Limit: tc.limit}
				var got []int64
				if method == "find" {
					var rows []bson.M
					if err := coll.Find(context.Background(), bson.M{}, &rows, option); err != nil {
						t.Fatal(err)
					}
					for _, row := range rows {
						got = append(got, row["_id"].(int64))
					}
				} else {
					if err := coll.StreamFind(context.Background(), bson.M{}, func(raw []byte) error {
						var row bson.M
						if err := bson.Unmarshal(raw, &row); err != nil {
							return err
						}
						got = append(got, row["_id"].(int64))
						return nil
					}, option); err != nil {
						t.Fatal(err)
					}
				}
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("%s skip=%d limit=%d got=%v want=%v", method, tc.skip, tc.limit, got, tc.want)
				}
			})
		}
	}
}
