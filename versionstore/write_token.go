package versionstore

// 一次性写令牌（A2 ③，方案 docs/feature/A2-3-VERSIONSTORE-WRITE-TOKEN-2026-10-07.md）。
//
// 写命令的回复丢失时（读超时、发出后连接断开），脚本可能已经执行。驱动不重放写命令（A2 ②），
// 之前 store 也把传输错误原样交给调用方；调用方再调一次，Update 会把 mutate 叠在自己那次已生效的
// 写上，Create 会把自己刚建的键报成“已被占用”。
//
// 现在每条写命令带一个由 store 生成的令牌，和值在同一条 SET 里写进信封头：
//
//	<version>|<token_v>|<token_v-1>|…\n<payload>
//
// 列表是这个键最近 WriteTokenHistory 次写的令牌，新的在前，第 i 个写出 version-i。下一次写由
// Go 拼出“新令牌 + 读到的列表”，脚本比较整条字节，比较通过就说明承接的正是读到的那份，所以
// Lua 不用改。结果未知时 store 读一次当前值，按 judge 的规则下结论：认出自己的令牌就是已生效；
// 当前值与基准字节相同就是没执行、原样重发；由基准演进而来又没有自己的令牌就是没生效；
// 其余（键不存在、链断了、令牌被挤出）无法证明，返回 *UnknownOutcomeError，调用方可用 Resume
// 在下一次调用里接着核对。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"strconv"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/metrics"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

var (
	// ErrOutcomeUnknown reports a write whose command may or may not have run,
	// after the store tried and failed to find out. The returned error is a
	// *UnknownOutcomeError; pass it to Resume to let the next call finish the
	// check instead of writing again.
	ErrOutcomeUnknown = errors.New("versionstore: write outcome unknown")

	// ErrWriteTokenMismatch reports a resumed write that is not the write the
	// token was issued for: a different value, a different kind of write, or a
	// different held version. Nothing is written.
	ErrWriteTokenMismatch = errors.New("versionstore: write token reused for a different write")
)

// DefaultWriteTokenHistory is how many write tokens each key keeps. It equals
// DefaultMaxAttempts: that is the contention a single write is designed to
// survive, so a write whose reply was lost can still find its token after that
// many writes by others. Past it the store answers ErrOutcomeUnknown rather
// than guessing, so the number bounds how often a lost reply is resolved, not
// whether the answer is right.
const DefaultWriteTokenHistory = DefaultMaxAttempts

// MetricUnknownOutcome counts lost replies by what checking the token found:
// result=applied|lost|unresolved, label store. Exported as
// versionstore_unknown_outcome_total.
const MetricUnknownOutcome = "versionstore.unknown_outcome.total"

func countUnknownOutcome(store, result string) {
	metrics.IncCounter(MetricUnknownOutcome, metrics.Labels{"store": storeLabel(store), "result": result}, 1)
}

// UnknownOutcomeError is a write the store could not settle: the command may
// have run, and neither its token nor the key's history proves either way.
// errors.Is matches ErrOutcomeUnknown and the transport error.
type UnknownOutcomeError struct {
	// Key is the rendered Redis key.
	Key string
	// Token is this write's token; empty for a delete, which writes none.
	Token string
	// Err is the transport error that left the outcome open.
	Err error

	write *pendingWrite
}

func (e *UnknownOutcomeError) Error() string {
	token := e.Token
	if token == "" {
		token = "-"
	}
	return fmt.Sprintf("versionstore: outcome of the write to %s is unknown (token %s): %v", e.Key, token, e.Err)
}

func (e *UnknownOutcomeError) Unwrap() []error { return []error{ErrOutcomeUnknown, e.Err} }

type resumeKey struct{}

// Resume hands an unknown outcome to the next call. A write on the same key
// and of the same kind first checks the earlier write's token: if it landed,
// the call returns that write's result without writing again; if it provably
// did not, the call proceeds as usual. The resumed write must be the same
// write — same value for Create, a mutate that yields the same value from the
// same base for Update, the same held version for Delete — or the call returns
// ErrWriteTokenMismatch. Writes to other keys ignore it.
//
// err that is not an *UnknownOutcomeError returns ctx unchanged, so a retry
// loop can wrap every attempt. The resumed state lives in this process only.
func Resume(ctx context.Context, err error) context.Context {
	var unknown *UnknownOutcomeError
	if !errors.As(err, &unknown) || unknown.write == nil {
		return ctx
	}
	return context.WithValue(ctx, resumeKey{}, unknown.write)
}

func resumedWrite(ctx context.Context, redisKey string) *pendingWrite {
	write, _ := ctx.Value(resumeKey{}).(*pendingWrite)
	if write == nil || write.key != redisKey {
		return nil
	}
	return write
}

type writeKind uint8

const (
	writeCreate writeKind = iota + 1
	writeUpdate
	writeDelete
)

// pendingWrite is one write command exactly as sent, so it can be sent again
// byte for byte and its outcome judged later.
type pendingWrite struct {
	kind writeKind
	key  string
	// base is the envelope the command compares against; nil means the key
	// must be absent.
	base []byte
	// next is the envelope a create / update writes.
	next []byte
	// token is next's token; empty for a delete.
	token string
	// version is next's version, or base's version for a delete.
	version uint64

	index        *fredis.CompareAndSetIndex
	deleteKeys   []string
	deleteMember string
}

// newWriteToken returns 64 random bits in base64url: 11 characters, none of
// them '|' or '\n'. Uniqueness only has to hold among the writes one key
// keeps, so the process-seeded generator is enough.
func newWriteToken() string {
	var raw [8]byte
	binary.LittleEndian.PutUint64(raw[:], rand.Uint64())
	return base64.RawURLEncoding.EncodeToString(raw[:])
}

// envelopeHeader is the version line of a stored value.
type envelopeHeader struct {
	version uint64
	tokens  []string
	payload []byte
}

func parseEnvelope(raw []byte) (envelopeHeader, error) {
	newline := bytes.IndexByte(raw, '\n')
	if newline <= 0 {
		return envelopeHeader{}, fmt.Errorf("%w: no version separator", ErrMalformedRecord)
	}
	fields := strings.Split(string(raw[:newline]), "|")
	version, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return envelopeHeader{}, fmt.Errorf("%w: version is not a number: %v", ErrMalformedRecord, err)
	}
	if version == 0 {
		return envelopeHeader{}, fmt.Errorf("%w: version is zero", ErrMalformedRecord)
	}
	tokens := fields[1:]
	if len(tokens) == 0 {
		// The format before A2 ③. No compatibility: the store is cleared on
		// upgrade (CHANGELOG).
		return envelopeHeader{}, fmt.Errorf("%w: no write token (old envelope format; clear the store on upgrade)", ErrMalformedRecord)
	}
	if uint64(len(tokens)) > version {
		return envelopeHeader{}, fmt.Errorf("%w: %d write tokens for version %d", ErrMalformedRecord, len(tokens), version)
	}
	for _, token := range tokens {
		if token == "" {
			return envelopeHeader{}, fmt.Errorf("%w: empty write token", ErrMalformedRecord)
		}
	}
	return envelopeHeader{version: version, tokens: tokens, payload: raw[newline+1:]}, nil
}

// tokenFor returns the token that wrote version, while the key still keeps it.
func (h envelopeHeader) tokenFor(version uint64) (string, bool) {
	if version == 0 || version > h.version {
		return "", false
	}
	back := h.version - version
	if back >= uint64(len(h.tokens)) {
		return "", false
	}
	return h.tokens[back], true
}

// buildEnvelope frames payload as the version-th write with token in front of
// the tokens the base kept, cut to history.
func buildEnvelope(payload []byte, version uint64, token string, prior []string, history int) []byte {
	var buf bytes.Buffer
	buf.WriteString(strconv.FormatUint(version, 10))
	buf.WriteByte('|')
	buf.WriteString(token)
	for i := 0; i < len(prior) && i < history-1; i++ {
		buf.WriteByte('|')
		buf.WriteString(prior[i])
	}
	buf.WriteByte('\n')
	buf.Write(payload)
	return buf.Bytes()
}

type verdict uint8

const (
	verdictUnproven verdict = iota
	verdictApplied
	verdictLost
	verdictNotRun
)

// judge decides what became of write from what the key holds now (nil when
// absent). It concludes only what the bytes prove; see the table in the
// design doc §3.
func judge(write *pendingWrite, current []byte) verdict {
	if current == nil {
		// Absence has no identity: "never ran" and "ran, then deleted by
		// someone" look the same, and a delete leaves nothing to recognise.
		return verdictUnproven
	}
	if write.base != nil && bytes.Equal(current, write.base) {
		// Base carries random tokens, so equal bytes are the same write, not a
		// recreation: the command has not run, and since it compares whole
		// bytes it can never land on anything else.
		return verdictNotRun
	}
	now, err := parseEnvelope(current)
	if err != nil {
		return verdictUnproven
	}
	if write.token != "" {
		if token, ok := now.tokenFor(write.version); ok && token == write.token {
			return verdictApplied
		}
	}
	if write.base != nil {
		base, err := parseEnvelope(write.base)
		if err != nil {
			return verdictUnproven
		}
		// The key moved on FROM base (base's own token sits at base's version)
		// and this write's token is not at the next one: someone else's write
		// took that version, so this one cannot land any more.
		if token, ok := now.tokenFor(base.version); ok && token == base.tokens[0] && now.version > base.version {
			return verdictLost
		}
	}
	return verdictUnproven
}

type outcome uint8

const (
	outcomeApplied outcome = iota + 1
	outcomeLost
	outcomeFailed
)

// replyNotLost reports an error that is not a lost reply: the server's error
// reply arrived (the script raised or was refused), or the command never left
// (a failed dial, an invalid command). There is nothing to settle; the error
// is returned as-is, as before A2 ③. versionstore cannot import the driver,
// so it recognises these by shape; anything else is treated as a lost reply,
// which costs a read.
func replyNotLost(err error) bool {
	var reply interface{ RedisError() }
	if errors.As(err, &reply) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	return errors.Is(err, fredis.ErrCASInvalidCommand)
}

// settle sends write and, while its outcome is open, reads the key and judges
// it, resending only when the bytes prove the command has not run. ambiguous
// is true when an earlier send of the same command (a resumed write) may
// already have landed. Sends share the MaxAttempts budget; a failed read stops
// at once, because the backend is not answering.
//
// It returns outcomeApplied, outcomeLost, or outcomeFailed with the error to
// return (an *UnknownOutcomeError when the outcome stays open).
func (s *RedisStore[K, T]) settle(ctx context.Context, write *pendingWrite, ambiguous bool) (outcome, error) {
	remaining := s.cfg.MaxAttempts
	return s.settleWithBudget(ctx, write, ambiguous, &remaining)
}

// 一次 Update 的竞争重试和原命令重发共用发送预算，不能形成两层相乘的循环。
func (s *RedisStore[K, T]) settleWithBudget(ctx context.Context, write *pendingWrite, ambiguous bool, remaining *int) (outcome, error) {
	var transportErr error
	resolved := func(result string, o outcome) (outcome, error) {
		if ambiguous {
			countUnknownOutcome(s.cfg.Prefix, result)
		}
		return o, nil
	}
	unresolved := func(err error) (outcome, error) {
		countUnknownOutcome(s.cfg.Prefix, "unresolved")
		if transportErr != nil {
			err = transportErr
		}
		return outcomeFailed, &UnknownOutcomeError{Key: write.key, Token: write.token, Err: err, write: write}
	}
	for sends := 0; ; {
		if ambiguous {
			current, err := s.rawOrNil(ctx, write.key)
			if err != nil {
				return unresolved(err)
			}
			switch judge(write, current) {
			case verdictApplied:
				return resolved("applied", outcomeApplied)
			case verdictLost:
				return resolved("lost", outcomeLost)
			case verdictUnproven:
				return unresolved(errors.New("the key's history does not show this write either way"))
			}
			// verdictNotRun: send it again.
			if *remaining <= 0 {
				return unresolved(errors.New("the command has not run and the resend budget is spent"))
			}
			if err := ctx.Err(); err != nil {
				return unresolved(err)
			}
			s.backoff(sends)
		}
		sends++
		(*remaining)--
		applied, err := s.send(ctx, write)
		switch {
		case err == nil && applied:
			return resolved("applied", outcomeApplied)
		case err == nil && !ambiguous:
			// The reply itself says the compare failed: a definite loss.
			return outcomeLost, nil
		case err == nil:
			// A resend lost its compare; the earlier send is what may have
			// changed the key. Look again.
		case replyNotLost(err) && !ambiguous:
			return outcomeFailed, err
		case replyNotLost(err):
			// This resend was refused or never left; the earlier send still
			// may have landed. Look again.
		default:
			transportErr = err
		}
		ambiguous = true
	}
}

// send issues write once.
func (s *RedisStore[K, T]) send(ctx context.Context, write *pendingWrite) (bool, error) {
	if write.kind == writeDelete {
		result, err := s.client.Eval(ctx, deleteIfScript, write.deleteKeys, write.base, write.deleteMember)
		if err != nil {
			return false, err
		}
		applied, _ := result.(int64)
		return applied == 1, nil
	}
	result, err := fredis.CompareAndSet(ctx, s.client, fredis.CompareAndSetCommand{
		Key: write.key, Expected: write.base, Next: write.next, TTL: s.cfg.TTL, Index: write.index,
	})
	if err != nil {
		return false, err
	}
	return result.Applied, nil
}

// rawOrNil reads the stored bytes, nil when absent.
func (s *RedisStore[K, T]) rawOrNil(ctx context.Context, redisKey string) ([]byte, error) {
	stored, err := s.client.Get(ctx, redisKey)
	if err != nil {
		if isRedisMiss(err) {
			return nil, nil
		}
		return nil, err
	}
	return stored, nil
}

// prepareWrite builds the create / update command that writes value as the
// version-th write on top of base (nil: the key must be absent).
func (s *RedisStore[K, T]) prepareWrite(kind writeKind, key K, redisKey string, base []byte, value T, version uint64) (*pendingWrite, error) {
	payload, err := s.cfg.Codec.Encode(value)
	if err != nil {
		return nil, err
	}
	var prior []string
	if base != nil {
		header, err := parseEnvelope(base)
		if err != nil {
			return nil, err
		}
		prior = header.tokens
	}
	token := newWriteToken()
	return &pendingWrite{
		kind: kind, key: redisKey, base: base, token: token, version: version,
		next:  buildEnvelope(payload, version, token, prior, s.cfg.WriteTokenHistory),
		index: s.indexEntry(key, value),
	}, nil
}

// sameWrite reports whether value is what write carries, by encoded payload.
func (s *RedisStore[K, T]) sameWrite(write *pendingWrite, value T) (bool, error) {
	payload, err := s.cfg.Codec.Encode(value)
	if err != nil {
		return false, err
	}
	header, err := parseEnvelope(write.next)
	if err != nil {
		return false, err
	}
	return bytes.Equal(payload, header.payload), nil
}

func tokenMismatch(write *pendingWrite, why string) error {
	return fmt.Errorf("%w: %s (token %s): %s", ErrWriteTokenMismatch, write.key, write.token, why)
}
