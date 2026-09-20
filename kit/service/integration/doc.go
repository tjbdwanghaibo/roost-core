// Package integration holds the cross-service tests that run against real
// backends.
//
// It exists because of a claim this repository makes and could otherwise not
// support: that seven of its nine packages need NO storage code of their own,
// because their state is `versionstore.Store` and roost-kit already ships a
// production Redis implementation of that contract. Each package's own tests
// run against in-process doubles, so on their own they prove the logic and say
// nothing about whether the reuse is real.
//
// The tests here assemble every package on one live Redis and drive a path
// through each. What they are checking is narrow and deliberate:
//
//   - the thin per-package constructors produce stores that actually work,
//     not stores that merely typecheck;
//   - the key namespaces do not collide, which is the one thing those
//     constructors exist to own;
//   - the compare-and-set contract holds against a real backend under real
//     concurrency, which an in-process double cannot establish.
//
// They are not a second copy of the unit suites and should not grow into one.
// A behaviour that a package's own tests can pin belongs there, where it runs
// on every commit without a Redis.
//
//	docker run --rm -p 6379:6379 redis:7
//	REDIS_ADDR=127.0.0.1:6379 go test -tags integration ./integration/
package integration
