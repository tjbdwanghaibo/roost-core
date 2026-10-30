package nats

import "time"

// Config holds NATS connection configuration.
type Config struct {
	URL string
	// Username / Password 是写在 URL 之外的账号（wiring/nats 的 nats.username / password，可来自环境变量，R11），
	// 都为空时只用 URL 里的 userinfo（如果有）。
	Username      string
	Password      string
	ReconnectWait time.Duration // default: 1s
	MaxReconnects int           // default: -1 (unlimited)
	PingInterval  time.Duration // default: 20s
	DrainTimeout  time.Duration // default: 10s

	OnDisconnect func(error)
	OnReconnect  func()
}

func DefaultConfig(url string) *Config {
	return &Config{
		URL:           url,
		ReconnectWait: time.Second,
		MaxReconnects: -1,
		PingInterval:  20 * time.Second,
		DrainTimeout:  10 * time.Second,
	}
}
