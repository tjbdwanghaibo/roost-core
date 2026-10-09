package platform
import("context";"fmt";"sync";"testing";"encoding/json";domain "github.com/tjbdwanghaibo/roost-core/service/platform")
const testPaymentSecret = "payment-secret-please-rotate"
func acceptingVerifier() domain.Verifier { return domain.VerifierFunc(func(_ context.Context,c domain.Credential)(domain.Verified,error){ if c.Secret!="good"{return domain.Verified{},fmt.Errorf("bad credential")};return domain.Verified{Channel:c.Channel,OpenID:c.OpenID},nil }) }
func resolver() domain.PlayerResolver { var mu sync.Mutex;ids:=map[string]int64{};next:=int64(1000);return domain.PlayerResolverFunc(func(_ context.Context,v domain.Verified)(int64,error){mu.Lock();defer mu.Unlock();key:=v.Channel+":"+v.OpenID;if id,ok:=ids[key];ok{return id,nil};next++;ids[key]=next;return next,nil}) }
func newDeliverer() domain.Deliverer { return domain.DelivererFunc(func(context.Context,domain.Order)error{return nil}) }
func callback(t *testing.T,id string,player int64,amount int64)([]byte,string){t.Helper();raw,err:=json.Marshal(map[string]any{"order_id":id,"player_id":player,"channel":"store","product_id":"gems-100","amount_minor":amount,"currency":"USD"});if err!=nil{t.Fatal(err)};return raw,domain.SignPayload(raw,testPaymentSecret)}
