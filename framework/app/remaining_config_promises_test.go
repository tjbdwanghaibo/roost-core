package app

import (
	"github.com/spf13/viper"
	"strings"
	"testing"
)

func TestSharedKeyErrorIsReportedOnceAcrossOwners(t *testing.T) {
	cfg := viper.New()
	cfg.Set("sid", "abc")
	schema := SchemaOf(ServiceIdentity{})
	_, err := checkConfig(cfg, []configDeclaration{{owner: "mod a", schema: schema}, {owner: "mod b", schema: schema}})
	if err == nil || strings.Count(err.Error(), "sid must be a whole number") != 1 {
		t.Fatalf("one invalid key produced repeated owner errors: %v", err)
	}
}
