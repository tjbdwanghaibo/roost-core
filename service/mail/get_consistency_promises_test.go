package mail

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// U-0145 · C8（快慢路径不对称）· classscan 记录的 `mail` Get / GetMany 对。
//
// 单读与批读对同一存储状态必须给出同一答案：都命中且信封相同、都不存在、或
// 都以错误拒绝。这里把每种存储形状（正常、不存在、损坏 JSON、缺 id、键在但值
// 为空）分别走两条路。此前 Get 把"键在但值为空"当作不存在，GetMany 对同一
// 个值报解码错误——同一条损坏数据，List 会失败而单读说邮件没了。

func TestGetAndGetManyAgreeOnEveryStoredShape(t *testing.T) {
	ctx := context.Background()
	store, fake := newRedisEnvelopeStore(t)
	valid := testEnvelope("m-valid", time.Hour)
	if created, err := store.Create(ctx, valid); err != nil || !created {
		t.Fatalf("seed: created=%v err=%v", created, err)
	}
	fake.mu.Lock()
	fake.values["mailtest:env:m-malformed"] = []byte("{not json")
	fake.values["mailtest:env:m-noid"] = []byte(`{}`)
	fake.values["mailtest:env:m-empty"] = []byte{}
	fake.mu.Unlock()

	for _, id := range []string{"m-valid", "m-absent", "m-malformed", "m-noid", "m-empty"} {
		t.Run(id, func(t *testing.T) {
			single, found, err := store.Get(ctx, id)
			many, manyErr := store.GetMany(ctx, []string{id})
			if (err == nil) != (manyErr == nil) {
				t.Fatalf("verdicts differ: Get=(found=%v, %v) GetMany=%v", found, err, manyErr)
			}
			if err != nil {
				return
			}
			batch, inBatch := many[id]
			if found != inBatch {
				t.Fatalf("presence differs: Get found=%v, GetMany has it=%v", found, inBatch)
			}
			if found && !reflect.DeepEqual(single, batch) {
				t.Fatalf("envelopes differ:\nGet     %+v\nGetMany %+v", single, batch)
			}
		})
	}
}
