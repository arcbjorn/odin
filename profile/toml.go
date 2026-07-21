package profile

import (
	"fmt"
	"strconv"
	"strings"
)

// parseConfig reads the subset of TOML that config.toml uses: top-level
// key/value pairs, a [telegram] table, and repeated [[providers]] tables.
//
// Hand-rolled rather than pulled in as a dependency. The grammar here is a few
// dozen lines and fully covered by tests; a general TOML library would add a
// module to the build for config we control the shape of. If the config ever
// needs nested tables or inline arrays of tables, swap this for BurntSushi
// rather than growing it.
func parseConfig(src string) (Config, error) {
	var cfg Config
	section := ""

	for n, rawLine := range strings.Split(src, "\n") {
		line := strings.TrimSpace(stripComment(rawLine))
		if line == "" {
			continue
		}
		lineNo := n + 1

		// [[providers]] starts a new provider; [telegram] switches section.
		if strings.HasPrefix(line, "[[") {
			name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "[["), "]]"))
			if name != "providers" {
				return cfg, fmt.Errorf("line %d: unknown array table [[%s]]", lineNo, name)
			}
			cfg.Providers = append(cfg.Providers, ProviderConfig{})
			section = "providers"
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			switch section {
			case "telegram", "agent", "web":
			default:
				return cfg, fmt.Errorf("line %d: unknown table [%s]", lineNo, section)
			}
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return cfg, fmt.Errorf("line %d: expected key = value", lineNo)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)

		if err := assign(&cfg, section, key, value, lineNo); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}

func assign(cfg *Config, section, key, value string, lineNo int) error {
	switch section {
	case "":
		switch key {
		case "toolsets":
			list, err := parseStringArray(value)
			if err != nil {
				return fmt.Errorf("line %d: toolsets: %w", lineNo, err)
			}
			cfg.Toolsets = list
		case "timezone":
			s, err := parseString(value)
			if err != nil {
				return fmt.Errorf("line %d: timezone: %w", lineNo, err)
			}
			cfg.Timezone = s
		default:
			return fmt.Errorf("line %d: unknown key %q", lineNo, key)
		}

	case "agent":
		switch key {
		case "max_turns":
			n, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("line %d: max_turns: %w", lineNo, err)
			}
			cfg.MaxTurns = n
		case "max_tokens":
			n, err := strconv.Atoi(value)
			if err != nil {
				return fmt.Errorf("line %d: max_tokens: %w", lineNo, err)
			}
			cfg.MaxTokens = n
		case "effort":
			s, err := parseString(value)
			if err != nil {
				return fmt.Errorf("line %d: effort: %w", lineNo, err)
			}
			cfg.Effort = s
		default:
			return fmt.Errorf("line %d: unknown key %q in [agent]", lineNo, key)
		}

	case "web":
		switch key {
		case "reader_url":
			s, err := parseString(value)
			if err != nil {
				return fmt.Errorf("line %d: reader_url: %w", lineNo, err)
			}
			cfg.Web.ReaderURL = s
		case "reader_key_env":
			s, err := parseString(value)
			if err != nil {
				return fmt.Errorf("line %d: reader_key_env: %w", lineNo, err)
			}
			cfg.Web.ReaderKeyEnv = s
		case "search_url":
			s, err := parseString(value)
			if err != nil {
				return fmt.Errorf("line %d: search_url: %w", lineNo, err)
			}
			cfg.Web.SearchURL = s
		default:
			return fmt.Errorf("line %d: unknown key %q in [web]", lineNo, key)
		}

	case "telegram":
		switch key {
		case "token_env":
			s, err := parseString(value)
			if err != nil {
				return fmt.Errorf("line %d: token_env: %w", lineNo, err)
			}
			cfg.Telegram.TokenEnv = s
		case "allowed_users":
			ids, err := parseIntArray(value)
			if err != nil {
				return fmt.Errorf("line %d: allowed_users: %w", lineNo, err)
			}
			cfg.Telegram.AllowedUsers = ids
		default:
			return fmt.Errorf("line %d: unknown key %q in [telegram]", lineNo, key)
		}

	case "providers":
		if len(cfg.Providers) == 0 {
			return fmt.Errorf("line %d: key %q outside any [[providers]] block", lineNo, key)
		}
		p := &cfg.Providers[len(cfg.Providers)-1]
		switch key {
		case "kind":
			s, err := parseString(value)
			if err != nil {
				return fmt.Errorf("line %d: kind: %w", lineNo, err)
			}
			p.Kind = s
		case "name":
			s, err := parseString(value)
			if err != nil {
				return fmt.Errorf("line %d: name: %w", lineNo, err)
			}
			p.Name = s
		case "model":
			s, err := parseString(value)
			if err != nil {
				return fmt.Errorf("line %d: model: %w", lineNo, err)
			}
			p.Model = s
		case "base_url":
			s, err := parseString(value)
			if err != nil {
				return fmt.Errorf("line %d: base_url: %w", lineNo, err)
			}
			p.BaseURL = s
		case "api_key_env":
			s, err := parseString(value)
			if err != nil {
				return fmt.Errorf("line %d: api_key_env: %w", lineNo, err)
			}
			p.APIKeyEnv = s
		case "api_mode":
			s, err := parseString(value)
			if err != nil {
				return fmt.Errorf("line %d: api_mode: %w", lineNo, err)
			}
			p.APIMode = s
		case "subscription":
			s, err := parseString(value)
			if err != nil {
				return fmt.Errorf("line %d: subscription: %w", lineNo, err)
			}
			p.Subscription = s
		case "accounts":
			list, err := parseStringArray(value)
			if err != nil {
				return fmt.Errorf("line %d: accounts: %w", lineNo, err)
			}
			p.Accounts = list
		case "client_id":
			s, err := parseString(value)
			if err != nil {
				return fmt.Errorf("line %d: client_id: %w", lineNo, err)
			}
			p.ClientID = s
		case "device_url":
			s, err := parseString(value)
			if err != nil {
				return fmt.Errorf("line %d: device_url: %w", lineNo, err)
			}
			p.DeviceURL = s
		case "scope":
			s, err := parseString(value)
			if err != nil {
				return fmt.Errorf("line %d: scope: %w", lineNo, err)
			}
			p.Scope = s
		case "token_url":
			s, err := parseString(value)
			if err != nil {
				return fmt.Errorf("line %d: token_url: %w", lineNo, err)
			}
			p.TokenURL = s
		case "oauth":
			b, err := parseBool(value)
			if err != nil {
				return fmt.Errorf("line %d: oauth: %w", lineNo, err)
			}
			p.OAuth = b
		case "drop_effort":
			b, err := parseBool(value)
			if err != nil {
				return fmt.Errorf("line %d: drop_effort: %w", lineNo, err)
			}
			p.DropEffort = b
		// A secret in config.toml would end up in git and in backups. The
		// only supported paths are an env var or the OAuth store.
		case "api_key", "token", "secret", "password":
			return fmt.Errorf("line %d: %q must not appear in config.toml; use api_key_env", lineNo, key)
		default:
			return fmt.Errorf("line %d: unknown key %q in [[providers]]", lineNo, key)
		}
	}
	return nil
}

// stripComment removes a trailing # comment, ignoring # inside a quoted string.
func stripComment(line string) string {
	inQuote := false
	for i, r := range line {
		switch r {
		case '"':
			inQuote = !inQuote
		case '#':
			if !inQuote {
				return line[:i]
			}
		}
	}
	return line
}

func parseString(v string) (string, error) {
	if len(v) < 2 || !strings.HasPrefix(v, `"`) || !strings.HasSuffix(v, `"`) {
		return "", fmt.Errorf("expected a quoted string, got %s", v)
	}
	return v[1 : len(v)-1], nil
}

func parseBool(v string) (bool, error) {
	switch v {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, fmt.Errorf("expected true or false, got %s", v)
	}
}

func parseStringArray(v string) ([]string, error) {
	items, err := splitArray(v)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, err := parseString(item)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func parseIntArray(v string) ([]int64, error) {
	items, err := splitArray(v)
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(items))
	for _, item := range items {
		n, err := strconv.ParseInt(item, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("expected an integer, got %s", item)
		}
		out = append(out, n)
	}
	return out, nil
}

func splitArray(v string) ([]string, error) {
	if !strings.HasPrefix(v, "[") || !strings.HasSuffix(v, "]") {
		return nil, fmt.Errorf("expected [ ... ], got %s", v)
	}
	inner := strings.TrimSpace(v[1 : len(v)-1])
	if inner == "" {
		return nil, nil
	}
	parts := strings.Split(inner, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}
