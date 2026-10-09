package etcd

import (
	"github.com/spf13/viper"
	"testing"
)

func TestEtcdRegistrationHasOneExplicitOwner(t *testing.T) {
	config := viper.New()
	config.Set("server_type", "game")
	config.Set("sid", 22)
	automatic := NewEtcdMod()
	if err := automatic.Init(config); err != nil {
		t.Fatal(err)
	}
	if automatic.serviceInfo == nil || automatic.serviceInfo.Sid != 22 {
		t.Fatal("default registration missing")
	}
	ingressOwns := NewEtcdMod(WithoutServiceRegistration())
	if err := ingressOwns.Init(config); err != nil {
		t.Fatal(err)
	}
	if ingressOwns.serviceInfo != nil {
		t.Fatal("two owners would register same Discovery")
	}
}
