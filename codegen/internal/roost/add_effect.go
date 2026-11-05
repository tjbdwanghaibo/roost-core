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
	delivery, order, err := parseEffectModes(options.Delivery, options.Order)
	if err != nil {
		return nil, err
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
		// 同一个 effect 的第二个消费者：沿用已有的载荷与常量，它们必须还是生成器认识的名字。持久性是生产方在 effect 文件里
		// 声明一次的（Delivery<Topic>），消费者骨架引用它；-delivery 只在新建 effect 文件时生效。
		for _, want := range []string{"Topic" + topic.pascal + " ", "type " + topic.pascal + " struct", "Delivery" + topic.pascal + " "} {
			if !strings.Contains(string(raw), want) {
				return nil, fmt.Errorf("%s exists but does not declare %q; the consumer skeleton refers to Topic%s, Delivery%s and %s", effectRel, strings.TrimSpace(want), topic.pascal, topic.pascal, topic.pascal)
			}
		}
		if options.Delivery != "" {
			return nil, fmt.Errorf("%s exists: its durability is declared there (Delivery%s); drop -delivery for another consumer", effectRel, topic.pascal)
		}
	case os.IsNotExist(err):
		formatted, err := format.Source([]byte(renderEffectDefinition(topic, delivery)))
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
	formatted, err := format.Source([]byte(renderEffectConsumer(m.Project.Module, m.servicePackage(service), service, topic, order)))
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

// parseEffectModes 校验 -delivery（reliable | best_effort，空 = reliable）与 -order（entity | none，空 = entity）。
func parseEffectModes(delivery, order string) (string, string, error) {
	switch delivery {
	case "", "reliable":
		delivery = "reliable"
	case "best_effort", "best-effort":
		delivery = "best_effort"
	default:
		return "", "", fmt.Errorf("invalid -delivery %q: reliable | best_effort", delivery)
	}
	switch order {
	case "", "entity":
		order = "entity"
	case "none":
	default:
		return "", "", fmt.Errorf("invalid -order %q: entity | none", order)
	}
	return delivery, order, nil
}

func effectDurable(service string, topic effectTopic) string {
	return service + "-" + strings.ReplaceAll(topic.topic, ".", "-")
}

func effectConsumerFunc(topic effectTopic) string { return "start" + topic.pascal + "EffectConsumer" }

func renderEffectDefinition(topic effectTopic, delivery string) string {
	deliveryConst, deliveryDoc := "nest.EffectReliable", `reliable: the transactional outbox.
// Emit%[1]s records it on the current Nest transaction: it is admitted to the WAL
// in the same record as the transaction's entity changes, published to the
// effect stream after commit, and consumed with a Mongo inbox that lets the
// consumer's work take effect once per effect. A transaction that reached the
// store always produces the effect; one that rolled back never does. Each one
// costs an outbox insert, its claim and delete, and the consumer's inbox
// transaction.`
	if delivery == "best_effort" {
		deliveryConst, deliveryDoc = "nest.EffectBestEffort", `best effort: no WAL record, no
// outbox, no inbox. Emit%[1]s registers it on the current Nest transaction and
// it is sent over NATS once, after the write reached its durable point (the
// moment the caller is answered); a transaction that rolled back, was refused
// or ended unknown never sends it. At most once: a process that stops after
// the write and before sending, a full send queue or a lost connection loses
// it, and a consumer error is not redelivered. Use it for notices whose loss
// something else makes up for.`
	}
	return fmt.Sprintf(`package effects

import (
	"encoding/json"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// An effect (roost add effect; a business file: generated once). This one is
// `+deliveryDoc+`

// Topic%[1]s is the effect topic; the subject on the wire is
// <dataengine.effects.subject_prefix>.<topic> (reliable) or
// <dataengine.effects.subject_prefix>_best_effort.<topic> (best effort).
const Topic%[1]s = %[2]q

// Delivery%[1]s is the effect's durability, declared here once: the consumer
// subscribes with the same value (EffectConsumer.Delivery). Changing it
// changes both sides.
const Delivery%[1]s = %[3]s

// %[1]s is the payload. JSON, so a consumer need not be Go. Add the fields the
// consumers need (the ids, not whole entities); a consumer reading an effect
// emitted before a field existed sees its zero value.
type %[1]s struct {
	// TODO: the effect's fields, e.g. PlayerID int64 `+"`json:\"player_id\"`"+`.
	// Inside a component the player's id is Owner().UniqueID(); Owner().ID() is
	// the encoded EntityID (entity type bits included), not the player id.
}

// Emit%[1]s registers the effect on the current Nest transaction, inside a Nest
// handler. It fails outside one: an effect with no transaction to ride on would
// be a message nobody committed to. key names what the effect is about (a
// player id, say); consumers see it as EffectEnvelope.Key.
func Emit%[1]s(key string, event %[1]s) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("effects: encode %%s: %%w", Topic%[1]s, err)
	}
	return nest.Emit(nest.Effect{Topic: Topic%[1]s, Key: key, Payload: payload, Delivery: Delivery%[1]s})
}
`, topic.pascal, topic.topic, deliveryConst)
}

func renderEffectConsumer(module, servicePackage, service string, topic effectTopic, order string) string {
	orderConst := "kitdataengine.EffectOrderEntity"
	if order == "none" {
		orderConst = "kitdataengine.EffectOrderNone"
	}
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

// %[3]sEffectDurable names the consumer of the %[4]s effects. Every replica of
// this service shares it, so each effect is delivered to one of them. Reliable
// (effects.Delivery%[3]s): the JetStream durable, its receipts
// _effect_inbox_<durable> in dataengine.database — do not rename it once
// deployed: a new durable starts at the beginning of the effect stream with no
// receipts, so effects still on the stream are handled again. Best effort: the
// NATS queue group, nothing stored.
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
// What the framework does (kitdataengine.SubscribeEffects), reliable: at-least-once
// delivery, at most once per EffectID. The handler runs inside a Mongo
// transaction (its ctx): a Mongo write made through ctx commits together with
// the effect's receipt, so a redelivered effect is recognised and skipped. A
// call into another service is outside that transaction: make it idempotent by
// envelope.EffectID (pass it as the call's request id), or a redelivery after a
// lost ack makes it twice. Return nil when done; an ordinary error rolls back
// and is redelivered with backoff; kitdataengine.PoisonEffect(err) drops an
// effect that can never be handled (an ERROR log and
// dataengine.effect_consumer.handled_total{result="poison"}; a nonzero count
// needs a person). Best effort: no inbox and no redelivery — the handler runs
// once per delivered effect, an error or a panic is only logged and counted
// (handled_total{result="failed"}), and an effect lost on the way is not seen.
//
// Order (EffectConsumer.Order) and capacity: a replica runs up to dataengine.effect_consumer.workers
// handlers at once (default 16; EffectConsumer.Workers overrides it, 1 handles
// one effect at a time). Effects with the same envelope.Key (the key given to
// Emit) are handled one at a time in arrival order, different keys
// concurrently, so the key should name what must stay ordered (the player id,
// say); an effect without a key is ordered with nothing. kitdataengine.EffectOrderNone
// drops the per-key queue: effects run in any order, up to workers at once.
// Replicas sharing the durable split the effects with no order between them.
// Its rate is about workers / the handler's latency; more load needs more replicas.
func %[6]s(ctx context.Context, registry *app.Registry) (fnats.IJetStreamSubscription, error) {
	consumer := kitdataengine.EffectConsumer{Durable: %[3]sEffectDurable, Topic: effects.Topic%[3]s,
		Delivery: effects.Delivery%[3]s, Order: %[8]s}
	return kitdataengine.SubscribeEffects(ctx, registry, consumer, func(ctx context.Context, envelope nestwal.EffectEnvelope) error {
		var event effects.%[3]s
		if err := json.Unmarshal(envelope.Payload, &event); err != nil {
			return kitdataengine.PoisonEffect(fmt.Errorf("%[4]s consumer: decode effect %%s: %%w", envelope.EffectID, err))
		}
		_ = event
		// TODO: act on the effect. Until this is written a reliable effect is
		// redelivered with backoff instead of being acknowledged unhandled (a best
		// effort one is counted as failed).
		return fmt.Errorf("%[4]s consumer is not implemented; effect %%s is not handled", envelope.EffectID)
	})
}
`, servicePackage, module, topic.pascal, topic.topic, effectDurable(service, topic), effectConsumerFunc(topic), service+"/"+effectConsumerFunc(topic), orderConst)
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
