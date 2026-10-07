// Package settings declares the config keys the game service's own code
// reads, the same way every framework Mod declares its keys (maintainer
// decision A4 ①): one struct, the key names and rules in its tags, read with
// app.LoadConfig. The game service returns Schema() from its ConfigSchema
// method, so the App checks these keys before any Mod starts, together with
// every Mod's, and `<bin> game --print-config` / `roost project
// doctor` know about them.
//
// Business code reads config only through the loaders below, never through
// the viper methods: a key read straight from viper is in no declaration, so
// nothing checks it at start, and doctor cannot tell it from a stray key in
// the config file. The generator's tests scan this project for exactly that.
//
// Keys a framework Mod of this process owns (dataengine.*, saga.*) are read
// through that Mod's own accessor instead (kitdataengine.EffectSettings,
// kitsaga.StreamSettings), so there is one declaration per key.
package settings

import (
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
)

// Config is every key the game service's code reads.
type Config struct {
	// sid and server_type: the App's own declaration, embedded so the two are
	// identical (two different declarations of one key refuse to start).
	app.ServiceIdentity
	Activity Activity `config:"activity"`
	Platform Platform `config:"platform"`
}

// Activity is where the activity coordinator keeps its keys and which group
// this server is in. The prefix must be the coordinator's own: the game keeps
// its contributor board beside the coordinator's keys, and a different prefix
// means a settlement reading an empty board.
type Activity struct {
	KeyPrefix  string `config:"key_prefix" required:"true" help:"the activity coordinator's Redis key prefix (activity.key_prefix in config.activity.yaml); the game keeps its contributor board beside it"`
	GroupsFile string `config:"groups_file" required:"true" help:"the activity groups file the coordinator reads too (C4): this server takes part in the group that lists its sid"`
}

// Platform is the game side of the platform service's handover: the prefix
// the paid-order grants are written under, and the secret the demo signs its
// own payment callbacks with (it plays the payment provider).
type Platform struct {
	KeyPrefix     string `config:"key_prefix" required:"true" help:"the platform service's Redis key prefix (platform.key_prefix in config.platform.yaml); paid-order grants are read from under it"`
	PaymentSecret string `config:"payment_secret" required:"true" secret:"true" help:"the platform service's payment_secret: the demo signs its simulated payment callbacks with it, and an empty one turns every callback into an invalid-signature refusal"`
}

// ValidateConfig checks what the tags cannot: a key prefix with whitespace in
// it would put this process's keys somewhere the coordinator never looks.
func (a Activity) ValidateConfig(bool) error { return mods.CheckKeyPrefix("activity", a.KeyPrefix) }

// ValidateConfig: the same rule for the platform prefix.
func (p Platform) ValidateConfig(bool) error { return mods.CheckKeyPrefix("platform", p.KeyPrefix) }

// Schema is the declaration the game service's ConfigSchema returns.
func Schema() app.ConfigSchema { return app.SchemaOf(Config{}) }

// Load reads every key the game service declares. Init calls it first, so a
// missing or malformed key stops the process before anything starts.
func Load(registry *app.Registry) (Config, error) {
	var config Config
	if err := load(registry, &config); err != nil {
		return Config{}, err
	}
	return config, nil
}

// Identity reads only sid and server_type: what most components need, and
// all a test registry carries.
func Identity(registry *app.Registry) (app.ServiceIdentity, error) {
	var identity app.ServiceIdentity
	if err := load(registry, &identity); err != nil {
		return app.ServiceIdentity{}, err
	}
	return identity, nil
}

// LoadActivity reads only the activity keys (and the identity).
func LoadActivity(registry *app.Registry) (app.ServiceIdentity, Activity, error) {
	var config struct {
		app.ServiceIdentity
		Activity Activity `config:"activity"`
	}
	if err := load(registry, &config); err != nil {
		return app.ServiceIdentity{}, Activity{}, err
	}
	return config.ServiceIdentity, config.Activity, nil
}

// LoadPlatform reads only the platform keys.
func LoadPlatform(registry *app.Registry) (Platform, error) {
	var config struct {
		Platform Platform `config:"platform"`
	}
	if err := load(registry, &config); err != nil {
		return Platform{}, err
	}
	return config.Platform, nil
}

func load(registry *app.Registry, dst any) error {
	if registry == nil {
		return fmt.Errorf("settings: app registry is required")
	}
	if err := app.LoadConfig(registry.Config(), dst); err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	return nil
}
