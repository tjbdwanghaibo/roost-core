# Roost Skill generation contract (`roost.skill/v2`)

Return either one Direct Root `roost.skill/v2` Skill JSON object or a concise rejection. Do not wrap output in `skill_json`, Markdown, or an envelope.

The only top-level authoring concepts are Skill, Phase, Flow, Select, Effect. Every generated skill declares `input_schema` explicitly. Use strict canonical variants and omit unknown or speculative fields.

Use a fixed Motion pipeline only inside a bounded Process Effect. Numeric tracks use the catalog Numeric whitelist; never invent runtime fields. Attribute reads declare supported snapshot points; `cast_start`, `phase_start` and `process_start` are captured before flow locals exist, so their entity is never a `$local` value nor an optional value such as `$primary_target` or a null-default memory (an `exists` guard at the read does not apply at capture time), and `process_start` appears only inside a spawn's process callbacks. Only a `spawn` effect may carry `process` or `on`. Gameplay Tag declarations, Gameplay Element, damage type, Status, ability, and owned entity references must be catalog-backed; a Visual element never implies a Gameplay Element.

Respect CastWindow, passive EventFilter and ProcPolicy bounds (a passive declares `input_schema` `none` or `entity` and `proc_policy.max_depth` of at least 1), and the Area enter/leave callbacks. The Host Combat Resolver owns combat resolution; JSON may request typed effects but may not define combat hooks.

Casting semantics. An active activation may declare `"concurrent": true` to opt out of caster cast-window exclusivity (by default a caster inside a windup/commit/recovery window rejects further root activations). Top-level `"global_cooldown_ticks"` puts the caster on a shared global cooldown starting at commit. `cast_window` supports `windup_ticks`, `commit_tick`, `recovery_ticks`, `movement` (allowed|slow|locked), `turning` (allowed|locked), `interrupt_tags`, `refund_before_commit`. Windup and recovery may instead be expressions: use `windup_ticks_expression`/`recovery_ticks_expression` (ticks-quantity expressions, e.g. `{"op":"scale_bp","args":[10, {"read_attribute":{"entity":"$caster","attribute":"attack_haste_bp","snapshot":"current"}}]}`) together with the mandatory `*_ticks_min`/`*_ticks_max` bounds; never emit both the literal and the expression for the same window side, and keep `commit_tick <= windup_ticks_min`.

Private persistent state is skill-scoped; shared persistent state is catalog-scoped. A memory default never reads another `$memory` value or a `$local`; a persistent state default is evaluated wherever the state is read or written (including process callbacks), so it uses only literals and `$caster` / `$caster.position` / `$cast.mode`, never `$input`, `$memory`, `$local`, `$primary_target`, `$ability.self`, other `$cast.*` values or `cast_start` / `phase_start` reads; write such values with `modify_state` in the cast flow instead. A memory default never reads `phase_start` (use `cast_start`). A status, ability, and owned entity select uses a singular Select kind. Select results are consumed only by `consume.one` or `consume.each`, with `on_empty` when needed. Dynamic bindings use only `$local.<name>`.

Typed effect results are scoped through `result.success` and `result.failure`. Entity-scoped processes cannot read expired Cast memory or input: this covers both their callbacks and the process fields re-evaluated on every step (area selection, follow / tracking / carry targets, path points, orbit anchor, parabola destination), which may use only `$caster`, `$caster.position` and `$cast.mode` — never `$input`, `$memory`, `$local`, `$primary_target`, `$ability.self`, other `$cast.*` values, or `cast_start` / `phase_start` reads (put a cast-time value into a numeric track's initial value, or use the lifecycle entity in callbacks). Process callbacks use `$owner`, not `$caster`. Paths and all input shapes obey declared bounds. A temporal snapshot restores only its authorized entity fields and is never a world rollback.

Visual metadata is passive: it may be mounted only on Skill presentation or supported Effects, is catalog-resolved, and cannot change Gameplay. Do not emit Quantity facts, optional facts, random seeds/sites, revisions, digests, trace settings, replay settings, resource paths, or client package keys.

The following is a complete canonical example. It is intentionally small; use
the same direct-root form rather than wrapping it in prose or another JSON
object.

```json
{
  "schema": "roost.skill/v2",
  "id": "skill.prompt.example_damage",
  "name": "Prompt Example Damage",
  "description": "Deals bounded physical damage to the selected target.",
  "input_schema": {"type": "entity"},
  "activation": {"type": "active", "policy": {"mode": "tap"}},
  "cooldown_ticks": 0,
  "costs": [],
  "memory": {},
  "initial_phase": "start",
  "phases": [{
    "id": "start",
    "timeout_ticks": 0,
    "on": {
      "enter": {"flow": "sequence", "steps": [
        {"flow": "effect", "effect": {"type": "damage", "target": "$input.target", "amount": 10, "damage_type": "physical"}},
        {"flow": "finish", "reason": "done"}
      ]}
    }
  }]
}
```
