package config

import (
	"errors"

	"gopkg.in/yaml.v3"
)

const telegramSettingsKey = "telegram_notifications"

// LoadTelegramSettings reads only the Agent's Telegram section into the
// supplied defaults. It also reports whether the existing file is private.
func LoadTelegramSettings(path string, out interface{}) (bool, error) {
	data, info, err := readConfigForUpdate(path)
	if err != nil || info == nil {
		return info == nil, err
	}
	_, mapping, err := configDocument(data)
	if err != nil {
		return false, err
	}
	value, err := telegramSettingsNode(mapping)
	if err != nil {
		return false, err
	}
	if value != nil && (value.Kind != yaml.MappingNode || value.Decode(out) != nil) {
		return false, errors.New("invalid Telegram settings section")
	}
	return info.Mode().Perm()&0o077 == 0, nil
}

// SaveTelegramSettings replaces only the Telegram section. The shared writer
// preserves other Agent settings and protects a configuration containing a token.
func SaveTelegramSettings(path string, settings interface{}, private bool) error {
	return updateAgentConfig(path, private, func(mapping *yaml.Node) error {
		current, err := telegramSettingsNode(mapping)
		if err != nil {
			return err
		}
		var value yaml.Node
		if err := value.Encode(settings); err != nil {
			return err
		}
		if current != nil {
			*current = value
		} else {
			mapping.Content = append(mapping.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: telegramSettingsKey},
				&value)
		}
		return nil
	})
}

func telegramSettingsNode(mapping *yaml.Node) (*yaml.Node, error) {
	var value *yaml.Node
	for i := 0; i < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value != telegramSettingsKey {
			continue
		}
		if value != nil {
			return nil, errors.New("duplicate Telegram settings section")
		}
		value = mapping.Content[i+1]
	}
	return value, nil
}
