// Package token 是 relay 令牌：lobby 签发、relay 校验的短期房间凭证（方案 FEATURE-2026-11-16-lobby-relay §4.3）。
//
// 格式（点号分隔，八段）：
//
//	rt1.<过期秒>.<relay 实例>.<房间号>.<账号>.<角色 host|member>.<最大人数>.<签名>
//
// 各段的写法：
//   - relay 实例、房间号、账号三段是 base64url（无填充），原文里可以有点号；
//   - 过期秒是 Unix 秒（系统时间），到点即失效（now >= 过期秒）；
//   - 最大人数是 lobby 建房时定的上限（含房主），relay 在房主连接建房时用它（§4.4“房间最大人数，以 lobby 给的为准，
//     放在令牌里”）；方案 §4.3 的格式没有列这一段，这里补上；
//   - 签名是 HMAC-SHA256(key, "roost.relay.token\n" + 前面七段原文)，base64url。
//
// 只有签名：令牌在有效期内可以重复使用，不能单张吊销。重放的影响由 relay 收住：同一账号在同一房间里只保留最新的
// 一条连接（§4.3“一次性”）。令牌里的角色只在建房时用一次，房间建好以后 relay 按“账号是不是当前房主”定角色
// （§4.6.5），所以旧房主手里没过期的 host 令牌在换 host 之后只能当成员。
//
// 密钥由 lobby 和 relay 共同配置（relay.token_key_env 指向的环境变量），两边用 KeyFromEnv 读，规则只在这里一处。
package token

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// Version 是令牌的版本前缀。
	Version = "rt1"
	// MaxBytes 是一张令牌的长度上限；更长的不做任何计算，直接无效。
	MaxBytes = 1024
	// DefaultTTL 是缺省有效期：令牌只用于建立连接，连上以后与它无关（§4.3）。
	DefaultTTL = 30 * time.Second
	// MinKeyBytes 是密钥的最短字节数。
	MinKeyBytes = 16
	// MaxMembersLimit 是令牌里最大人数的上界，防止一个笔误让 relay 按无界人数开房。
	MaxMembersLimit = 1024

	domain   = "roost.relay.token\n"
	segments = 8
)

// Role 是令牌里的角色。
type Role string

const (
	RoleHost   Role = "host"
	RoleMember Role = "member"
)

// Claims 是令牌的内容。
type Claims struct {
	// Relay 是房间所在的 relay 实例名；relay 只认写着自己名字的令牌。
	Relay string
	// Room 是房间号。
	Room string
	// Account 是持有者的账号（来自 account 的游戏账号 token，不是客户端自报）。
	Account string
	// Role 是签发时 lobby 认定的角色。只有建房时的第一条 host 连接用它（§4.6.5）。
	Role Role
	// MaxMembers 是房间人数上限（含房主），1..MaxMembersLimit。
	MaxMembers int
	// ExpiresAt 是过期时刻，按 Unix 秒取整（向下）。
	ExpiresAt time.Time
}

var (
	// ErrInvalid 是每一种无效令牌的同一个回答：过期、被改、别的密钥签的、格式不对、不是给这个 relay 实例的。
	// 错误里不说差在哪里。
	ErrInvalid = errors.New("relay token: the token is not valid")
	// ErrKey 报告密钥不可用（太短或环境变量没设）。
	ErrKey = errors.New("relay token: key is not usable")
)

// Sign 按 claims 签一张令牌。claims 的每个字段都必须有值、角色必须是 host 或 member、人数在 1..MaxMembersLimit。
func Sign(key []byte, claims Claims) (string, error) {
	if len(key) < MinKeyBytes {
		return "", fmt.Errorf("%w: %d bytes, need at least %d", ErrKey, len(key), MinKeyBytes)
	}
	switch {
	case claims.Relay == "" || claims.Room == "" || claims.Account == "":
		return "", errors.New("relay token: relay, room and account are required")
	case claims.Role != RoleHost && claims.Role != RoleMember:
		return "", fmt.Errorf("relay token: role %q is neither host nor member", claims.Role)
	case claims.MaxMembers < 1 || claims.MaxMembers > MaxMembersLimit:
		return "", fmt.Errorf("relay token: max members %d is outside 1..%d", claims.MaxMembers, MaxMembersLimit)
	case claims.ExpiresAt.IsZero():
		return "", errors.New("relay token: expiry is required")
	}
	body := strings.Join([]string{
		Version,
		strconv.FormatInt(claims.ExpiresAt.Unix(), 10),
		encode(claims.Relay), encode(claims.Room), encode(claims.Account),
		string(claims.Role),
		strconv.Itoa(claims.MaxMembers),
	}, ".")
	signed := body + "." + base64.RawURLEncoding.EncodeToString(mac(key, body))
	if len(signed) > MaxBytes {
		return "", fmt.Errorf("relay token: %d bytes exceeds %d; shorten the relay, room or account names", len(signed), MaxBytes)
	}
	return signed, nil
}

// Verify 校验令牌并返回内容。relay 是校验方自己的实例名：令牌写的实例不是它就无效——发给别的 relay 的令牌
// 不能拿到这里用。now 是校验方的系统时间。
//
// 顺序是先看形状、再验签名（定长时间比较）、再看过期与各字段：没验过签名的字段不值得解析。
func Verify(key []byte, token, relay string, now time.Time) (Claims, error) {
	if len(key) < MinKeyBytes {
		return Claims{}, fmt.Errorf("%w: %d bytes, need at least %d", ErrKey, len(key), MinKeyBytes)
	}
	if len(token) == 0 || len(token) > MaxBytes {
		return Claims{}, ErrInvalid
	}
	parts := strings.Split(token, ".")
	if len(parts) != segments || parts[0] != Version {
		return Claims{}, ErrInvalid
	}
	presented, err := base64.RawURLEncoding.DecodeString(parts[segments-1])
	if err != nil {
		return Claims{}, ErrInvalid
	}
	body := token[:len(token)-len(parts[segments-1])-1]
	if !hmac.Equal(presented, mac(key, body)) {
		return Claims{}, ErrInvalid
	}
	expiresAt, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || now.Unix() >= expiresAt {
		return Claims{}, ErrInvalid
	}
	claims := Claims{Role: Role(parts[5]), ExpiresAt: time.Unix(expiresAt, 0)}
	var ok1, ok2, ok3 bool
	claims.Relay, ok1 = decode(parts[2])
	claims.Room, ok2 = decode(parts[3])
	claims.Account, ok3 = decode(parts[4])
	if !ok1 || !ok2 || !ok3 || claims.Relay != relay {
		return Claims{}, ErrInvalid
	}
	if claims.Role != RoleHost && claims.Role != RoleMember {
		return Claims{}, ErrInvalid
	}
	claims.MaxMembers, err = strconv.Atoi(parts[6])
	if err != nil || claims.MaxMembers < 1 || claims.MaxMembers > MaxMembersLimit {
		return Claims{}, ErrInvalid
	}
	return claims, nil
}

// KeyFromEnv 读 name 指向的环境变量作为密钥：变量必须已设置，去掉两端空白后至少 MinKeyBytes 字节。
// 错误不回显变量的值。lobby 与 relay 的接线都经它读 relay.token_key_env，两边规则一致。
func KeyFromEnv(name string) ([]byte, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("%w: the key environment variable name is empty", ErrKey)
	}
	value, set := os.LookupEnv(name)
	if !set {
		return nil, fmt.Errorf("%w: environment variable %s is not set", ErrKey, name)
	}
	key := []byte(strings.TrimSpace(value))
	if len(key) < MinKeyBytes {
		return nil, fmt.Errorf("%w: environment variable %s holds %d bytes, need at least %d", ErrKey, name, len(key), MinKeyBytes)
	}
	return key, nil
}

func encode(value string) string { return base64.RawURLEncoding.EncodeToString([]byte(value)) }

func decode(segment string) (string, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil || len(raw) == 0 {
		return "", false
	}
	return string(raw), true
}

func mac(key []byte, body string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(domain))
	h.Write([]byte(body))
	return h.Sum(nil)
}
