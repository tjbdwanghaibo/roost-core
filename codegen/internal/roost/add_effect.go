package roost

import (
	"errors"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// effectTopic 是 roost add effect 的名字：点分的 effect 主题（例如 player.level_up）及其派生名字。
type effectTopic struct {
	topic  string // player.level_up：nest.Effect.Topic，线上 subject 是 <dataengine.effects.subject_prefix>.<topic>
	file   string // player_level_up：game/effects/<file>.go 与消费者文件名
	pascal string // PlayerLevelUp：载荷类型、Topic<Pascal>、Emit<Pascal>
}

// parseEffectTopic 校验 effect 主题：点分的若干段，每段蛇形化后是合法名字；saga.* 留给 saga 的 start / completion effect。
func parseEffectTopic(raw string) (effectTopic, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return effectTopic{}, errors.New("usage: roost add effect <topic> -consumer <service> (topic like player.level_up)")
	}
	var segments, pascal []string
	for _, segment := range strings.Split(raw, ".") {
		snake := toSnake(segment)
		if !validName(snake) {
			return effectTopic{}, fmt.Errorf("invalid effect topic %q: dot-separated names like player.level_up", raw)
		}
		segments = append(segments, snake)
		pascal = append(pascal, toPascal(segment))
	}
	if segments[0] == "saga" {
		return effectTopic{}, fmt.Errorf("invalid effect topic %q: saga.* carries the saga starts and completions on the same effect stream", raw)
	}
	return effectTopic{topic: strings.Join(segments, "."), file: strings.Join(segments, "_"), pascal: strings.Join(pascal, "")}, nil
}

// addEffect 是 roost add effect：载荷类型、topic 常量与 Emit 辅助（game/effects/<topic>.go），以及消费服务里的
// SubscribeEffects 骨架（internal/service/<svc>/<topic>_consumer.go）。都是业务文件、只建一次；effect 文件已存在
// （另一个服务已经消费它）时只建消费者。服务的 Init / Shutdown 归业务，只在 next 提示里给要加的两行。
func addEffect(root string, m Manifest, options AddOptions) ([]string, error) {
	topic, err := parseEffectTopic(options.Name)
	if err != nil {
		return nil, err
	}
	service := toSnake(options.Consumer)
	if service == "" {
		services := sortedServiceNames(m)
		if len(services) != 1 {
			return nil, errors.New("effect requires -consumer <service> when the project has multiple services")
		}
		service = services[0]
	}
	if _, ok := m.Services[service]; !ok {
		return nil, fmt.Errorf("unknown service %q", service)
	}
	resolved, err := resolveMods(effectiveServiceMods(m, service))
	if err != nil {
		return nil, err
	}
	if !contains(resolved, "dataengine") {
		// SubscribeEffects 按 DataEngine Mod 的声明（dataengine.effects.*、dataengine.database）找效果流与回执库，
		// 生产方与消费方不会各写一份“effect 发到哪里”。
		return nil, fmt.Errorf("effect consumer service %s needs the dataengine Mod (the consumer reads where effects go from dataengine.effects.* and keeps its receipts in dataengine.database); run: roost add mod dataengine -service %s", service, service)
	}
	effectRel := "game/effects/" + topic.file + ".go"
	consumerRel := "internal/service/" + service + "/" + topic.file + "_consumer.go"
	var changes []syncChange
	effectPath := filepath.Join(root, filepath.FromSlash(effectRel))
	switch raw, err := os.ReadFile(effectPath); {
	case err == nil:
		// 同一个 effect 的第二个消费者：沿用已有的载荷与常量，它们必须还是生成器认识的名字。
		for _, want := range []string{"Topic" + topic.pascal + " ", "type " + topic.pascal + " struct"} {
			if !strings.Contains(string(raw), want) {
				return nil, fmt.Errorf("%s exists but does not declare %q; the consumer skeleton refers to Topic%s and %s", effectRel, strings.TrimSpace(want), topic.pascal, topic.pascal)
			}
		}
	case os.IsNotExist(err):
		formatted, err := format.Source([]byte(renderEffectDefinition(topic)))
		if err != nil {
			return nil, fmt.Errorf("format %s: %w", effectRel, err)
		}
		changes = append(changes, syncChange{rel: effectRel, path: effectPath, body: formatted})
	default:
		return nil, err
	}
	consumerPath := filepath.Join(root, filepath.FromSlash(consumerRel))
	if _, err := os.Stat(consumerPath); err == nil {
		return nil, fmt.Errorf("%s already exists", consumerRel)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(root, "internal", "service", service)); err != nil {
		return nil, fmt.Errorf("service %s has no package at internal/service/%s; run roost project sync first: %w", service, service, err)
	}
	formatted, err := format.Source([]byte(renderEffectConsumer(m.Project.Module, m.servicePackage(service), service, topic)))
	if err != nil {
		return nil, fmt.Errorf("format %s: %w", consumerRel, err)
	}
	changes = append(changes, syncChange{rel: consumerRel, path: consumerPath, body: formatted})
	if err := commitSyncChanges(changes); err != nil {
		return nil, err
	}
	paths := make([]string, len(changes))
	for i, change := range changes {
		paths[i] = change.rel
	}
	return paths, nil
}

func effectDurable(service string, topic effectTopic) string {
	return service + "-" + strings.ReplaceAll(topic.topic, ".", "-")
}

func effectConsumerFunc(topic effectTopic) string { return "start" + topic.pascal + "EffectConsumer" }

func renderEffectDefinition(topic effectTopic) string {
	return fmt.Sprintf(`package effects

import (
	"encoding/json"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// An effect is the transactional outbox (roost add effect; a business file:
// generated once). Emit%[1]s records it on the current Nest transaction: it is
// admitted to the WAL in the same record as the transaction's entity changes,
// published to the effect stream after commit, and consumed with a Mongo inbox
// that lets the consumer's work take effect once per effect. A transaction that
// reached the store always produces the effect; one that rolled back never does.

// Topic%[1]s is the effect topic; the subject on the wire is
// <dataengine.effects.subject_prefix>.<topic>.
const Topic%[1]s = %[2]q

// %[1]s is the payload. JSON, so a consumer need not be Go. Add the fields the
// consumers need (the ids, not whole entities); a consumer reading an effect
// emitted before a field existed sees its zero value.
type %[1]s struct {
	// TODO: the effect's fields, e.g. PlayerID int64 `+"`json:\"player_id\"`"+`.
}

// Emit%[1]s records the effect on the current Nest transaction, inside a Nest
// handler. It fails outside one: an effect with no transaction to ride on would
// be a message nobody committed to. key names what the effect is about (a
// player id, say); consumers see it as EffectEnvelope.Key.
func Emit%[1]s(key string, event %[1]s) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("effects: encode %%s: %%w", Topic%[1]s, err)
	}
	return nest.Emit(nest.Effect{Topic: Topic%[1]s, Key: key, Payload: payload})
}
`, topic.pascal, topic.topic)
}

func renderEffectConsumer(module, servicePackage, service string, topic effectTopic) string {
	return fmt.Sprintf(`package %[1]s

import (
	"context"
	"encoding/json"
	"fmt"

	"%[2]s/game/effects"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	kitdataengine "github.com/tjbdwanghaibo/roost-core/wiring/dataengine"
)

// %[3]sEffectDurable names the JetStream consumer of the %[4]s effects. Every
// replica of this service shares it, so each effect is delivered to one of them;
// its receipts are _effect_inbox_<durable> in dataengine.database. Do not rename
// it once deployed: a new durable starts at the beginning of the effect stream
// with no receipts, so effects still on the stream are handled again.
const %[3]sEffectDurable = %[5]q

// %[6]s subscribes to the effects.Topic%[3]s effects (roost add effect; a
// business file: generated once, never rewritten by roost). The service owns
// Init and Shutdown; start it in Init and drain it in Shutdown:
//
//	// Init
//	subscription, err := %[6]s(context.Background(), registry)
//	// Shutdown
//	subscription.Drain()
//
// roost project doctor warns (effect-consumer:%[7]s) while nothing calls it.
//
// What the framework does (kitdataengine.SubscribeEffects): at-least-once
// delivery, at most once per EffectID. The handler runs inside a Mongo
// transaction (its ctx): a Mongo write made through ctx commits together with
// the effect's receipt, so a redelivered effect is recognised and skipped. A
// call into another service is outside that transaction: make it idempotent by
// envelope.EffectID (pass it as the call's request id), or a redelivery after a
// lost ack makes it twice. Return nil when done; an ordinary error rolls back
// and is redelivered with backoff; kitdataengine.PoisonEffect(err) drops an
// effect that can never be handled (an ERROR log and
// dataengine.effect_consumer.handled_total{result="poison"}; a nonzero count
// needs a person).
//
// Capacity: a replica handles one effect at a time from the durable (the
// JetStream callback is serial), so its rate is about 1 / the handler's latency;
// the effect consumer has not been measured on its own (a native saga step's
// callback, a Mongo transaction plus a Nest commit, takes roughly 30-35ms). More
// load needs more replicas sharing the durable.
func %[6]s(ctx context.Context, registry *app.Registry) (fnats.IJetStreamSubscription, error) {
	consumer := kitdataengine.EffectConsumer{Durable: %[3]sEffectDurable, Topic: effects.Topic%[3]s}
	return kitdataengine.SubscribeEffects(ctx, registry, consumer, func(ctx context.Context, envelope nestwal.EffectEnvelope) error {
		var event effects.%[3]s
		if err := json.Unmarshal(envelope.Payload, &event); err != nil {
			return kitdataengine.PoisonEffect(fmt.Errorf("%[4]s consumer: decode effect %%s: %%w", envelope.EffectID, err))
		}
		_ = event
		// TODO: act on the effect. Until this is written the effect is redelivered
		// with backoff instead of being acknowledged unhandled.
		return fmt.Errorf("%[4]s consumer is not implemented; effect %%s will be redelivered", envelope.EffectID)
	})
}
`, servicePackage, module, topic.pascal, topic.topic, effectDurable(service, topic), effectConsumerFunc(topic), service+"/"+effectConsumerFunc(topic))
}

// printEffectNext 是 add effect 之后的提示：在 Nest handler 里 Emit，在消费服务的 Init / Shutdown 里加两行。
func printEffectNext(stdout io.Writer, name string) {
	topic, err := parseEffectTopic(name)
	if err != nil {
		return
	}
	fmt.Fprintf(stdout, "next: add the payload fields to game/effects/%s.go and write the consumer body in internal/service/<service>/%s_consumer.go\n", topic.file, topic.file)
	fmt.Fprintf(stdout, "next: emit it inside a Nest handler: effects.Emit%s(key, effects.%s{...})\n", topic.pascal, topic.pascal)
	fmt.Fprintf(stdout, "next: start the consumer in the service (roost does not rewrite Init / Shutdown):\n")
	fmt.Fprintf(stdout, "  Init:     subscription, err := %s(context.Background(), registry)\n", effectConsumerFunc(topic))
	fmt.Fprintf(stdout, "  Shutdown: subscription.Drain()\n")
	fmt.Fprintln(stdout, "guide: "+sagaGuideURL)
}
