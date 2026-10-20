package gift_item

// The gift saga's step consumers are not registered here. roost add saga
// generates this file with a StartSteps / RegisterSteps pair for the common
// case (one process per service, every replica may take any command, the step
// bodies call the generated Nest senders); this demo overwrites it because its
// steps need two things that shape cannot know:
//
//   - players are bound to a sid and one process runs per sid, so the debit and
//     its refund may only run on the process that owns the sender: they carry
//     an admission check (kitsaga.WithAdmit) and a handoff to the owner
//     (Steps.RunHandedOver over the bus), built from the service's player
//     ownership table and bus that only exist after the service's Init;
//   - deliver is a mail call through the mail service's typed client.
//
// So the registration lives with the service, in
// internal/service/game/gift_saga.go (startGiftSaga: NewSteps, two
// Native and two Mongo registrations against the topic constants in
// definition.go, Start in Init, Drain in Shutdown), and the native steps'
// Nest handlers are game/handler/gift_debit.go and gift_refund.go. A project
// without sid-bound entities can use the generated file as it comes: see
// roost help saga, and roost add saga ... -native.
