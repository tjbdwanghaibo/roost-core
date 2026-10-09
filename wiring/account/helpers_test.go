package account

import domain "github.com/tjbdwanghaibo/roost-core/service/account"

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
)

// durableAllocator mints ids from a shared counter, standing in for a real
// durable allocator. Note what it is not: a per-process counter starting at
// one, which is what the implementation this replaces defaulted to.
type durableAllocator struct{ next atomic.Int64 }

func (a *durableAllocator) Allocate(context.Context, int32) (int64, error) {
	return a.next.Add(1) + 1_000_000, nil
}

func acceptingVerifier() domain.IdentityVerifier {
	return domain.VerifierFunc(func(_ context.Context, identity domain.Identity) (domain.Verified, error) {
		if identity.Credential != "good" {
			return domain.Verified{}, fmt.Errorf("bad credential")
		}
		return domain.Verified{Channel: identity.Channel, OpenID: identity.OpenID}, nil
	})
}

func simpleNameRules() domain.NameValidator {
	return domain.NameValidatorFunc(func(raw string) error {
		trimmed := strings.TrimSpace(raw)
		if len(trimmed) < 2 || len(trimmed) > 16 {
			return fmt.Errorf("name must be 2-16 characters")
		}
		return nil
	})
}
