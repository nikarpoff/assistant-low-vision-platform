// Package configs embeds the default configuration files of the gateway:
// the hazard rules (PLT-6) and the intent phrases (PLT-4). The files are
// replaced by APP_HAZARD_RULES_PATH and APP_INTENTS_PATH.
package configs

import (
	_ "embed"
)

// HazardRules is the default hazard_rules.yaml.
//
//go:embed hazard_rules.yaml
var HazardRules []byte

// Intents is the default intents.yaml.
//
//go:embed intents.yaml
var Intents []byte
