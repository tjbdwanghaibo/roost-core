package skill

import "testing"

func TestHostPayloadCarriesSuccessDataUsesTypedZeroChecks(t *testing.T) {
	if hostPayloadCarriesSuccessData(DamageEffectResult{}) || hostPayloadCarriesSuccessData(StatusEffectResult{}) {
		t.Fatal("zero typed results must not carry success data")
	}
	if !hostPayloadCarriesSuccessData(DamageEffectResult{Result: DamageResult{HealthDamage: 1}}) {
		t.Fatal("damage result must carry success data")
	}
	if !hostPayloadCarriesSuccessData(StatusEffectResult{Result: StatusResult{Created: StatusInstanceRef{ID: StatusInstanceID{opaque: 1}}}}) {
		t.Fatal("status result must carry success data")
	}
}
