package mods

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// 连接与凭据从环境变量读（R11，与下游 cube fork 的 resolveSecret 对齐；说明见 docs/framework/guide/13-wiring.md §2.1）。
//
// 连接 Mod（mongo / redis / etcd / nats）的地址与凭据键各有一个伴随键 `<键>_env`，值是环境变量名。规则只在这里实现一次：
//
//   - `<键>_env` 没写或为空：取 `<键>` 本身的值（叠加层可以写 `<键>_env: ""` 清掉下层的声明）；
//   - `<键>_env` 写了变量名：变量必须已设置且去掉两端空白后非空，它的值（去掉两端空白，与配置里字符串的读法相同）
//     取代 `<键>`。两者都写时变量优先：配置里的 `<键>` 留作本地缺省，部署由变量覆盖（下游 eks 配置就这样写，cube 同样）；
//   - 变量没设置或为空时报错，点名两个键与变量名，不带任何值；同一个 Mod 的缺失一次报全；
//   - 变量名必须是字母、数字、下划线且不以数字开头。这条只看配置本身，配置检查（App 启动前、--check-config、doctor）
//     就执行（ConfigValidator），不读环境变量：镜像构建、CI 里没有部署的密钥，检查照样通过；变量是否设置只在 Mod
//     Init（以及走同一规则的运维工具入口）时判断。名字不合法时也不回显写进去的内容（可能把口令本身写进了 `_env`）。
//
// 用法：连接配置结构体为每对键写一个“取值”方法，Init 用 ReadEnv、ValidateConfig 用 CheckEnvNames 调用它，两处的键清单因此
// 只写一遍：
//
//	func (c config) resolve(env *mods.EnvValues) (uri, password string) {
//		return env.String("mongo.uri", c.URI, c.URIEnv), env.String("mongo.password", c.Password, c.PasswordEnv)
//	}
type EnvValues struct {
	// lookup 为 nil 时只检查变量名（配置检查）：取值方法照常返回配置里的值。
	lookup func(string) (string, bool)
	errs   []error
}

// ReadEnv 返回按变量名读取进程环境变量的 EnvValues（Mod Init 用）。
func ReadEnv() *EnvValues { return &EnvValues{lookup: os.LookupEnv} }

// CheckEnvNames 返回只检查变量名、不读环境变量的 EnvValues（配置结构体的 ValidateConfig 用）。
func CheckEnvNames() *EnvValues { return &EnvValues{} }

// String 返回 key 的取值：envName 为空取 value，否则取变量的值（规则见上）。
func (e *EnvValues) String(key, value, envName string) string {
	if resolved, ok := e.lookupValue(key, envName); ok {
		return resolved
	}
	return value
}

// Strings 与 String 相同，变量的值按逗号分隔、每项去掉两端空白并丢弃空项（与配置里列表键的字符串写法相同）。
func (e *EnvValues) Strings(key string, value []string, envName string) []string {
	resolved, ok := e.lookupValue(key, envName)
	if !ok {
		return value
	}
	var out []string
	for _, item := range strings.Split(resolved, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// Err 返回攒下的全部错误（errors.Join），没有错误时为 nil。
func (e *EnvValues) Err() error { return errors.Join(e.errs...) }

// lookupValue 在 envName 非空且（读取模式下）变量有值时返回变量的值与 true。
func (e *EnvValues) lookupValue(key, envName string) (string, bool) {
	envName = strings.TrimSpace(envName)
	if envName == "" {
		return "", false
	}
	envKey := key + "_env"
	if !validEnvName(envName) {
		e.errs = append(e.errs, fmt.Errorf("config: %s must name an environment variable (letters, digits and _, not starting with a digit)", envKey))
		return "", false
	}
	if e.lookup == nil {
		return "", false
	}
	raw, _ := e.lookup(envName)
	resolved := strings.TrimSpace(raw)
	if resolved == "" {
		e.errs = append(e.errs, fmt.Errorf("config: %s names environment variable %s, which is not set or is empty; set it, or clear %s to use %s",
			envKey, envName, envKey, key))
		return "", false
	}
	return resolved, true
}

func validEnvName(name string) bool {
	for i, r := range name {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return name != ""
}
