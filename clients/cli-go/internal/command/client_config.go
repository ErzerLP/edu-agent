package command

import (
	"errors"
	"fmt"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/credentials"
)

func (a *App) runClientConfig(args []string) error {
	if len(args) == 0 {
		return clientConfigUsage("config requires show or set")
	}
	switch args[0] {
	case "show":
		if len(args) != 1 {
			return clientConfigUsage("config show accepts no arguments")
		}
		value, err := a.loadModelConfig()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(a.Out, "Server: %s\nTimeout: %s\nColor: %s\nDevice: %s\n", safeText(value.ServerURL), safeText(value.Timeout), safeText(value.Color), safeText(value.DisplayName))
		return err
	case "set":
		set := newFlagSet("config set")
		var timeout, color string
		set.StringVar(&timeout, "timeout", "", "positive request timeout")
		set.StringVar(&color, "color", "", "never, auto, or always")
		if err := set.Parse(args[1:]); err != nil || len(set.Args()) != 0 || (strings.TrimSpace(timeout) == "" && strings.TrimSpace(color) == "") {
			return clientConfigUsage("config set requires --timeout and/or --color")
		}
		value, err := a.loadModelConfig()
		if err != nil {
			return err
		}
		if strings.TrimSpace(timeout) != "" {
			value.Timeout = strings.TrimSpace(timeout)
		}
		if strings.TrimSpace(color) != "" {
			value.Color = strings.ToLower(strings.TrimSpace(color))
		}
		if err := value.Validate(); err != nil {
			return commandError("invalid_configuration", "client settings are invalid", "use a positive timeout and color never, auto, or always", ExitInput)
		}
		if err := a.Config.Save(value); err != nil {
			return commandError("configuration_write_failed", "client settings could not be saved", "check configuration directory permissions and retry", ExitInternal)
		}
		_, err = fmt.Fprintf(a.Out, "Client settings updated. Timeout: %s Color: %s\n", safeText(value.Timeout), safeText(value.Color))
		return err
	default:
		return clientConfigUsage("unknown config command " + args[0])
	}
}

func (a *App) loadMutableClientConfig() (config.Config, error) {
	if _, err := a.Config.LoadPairingJournal(); err == nil {
		return config.Config{}, pendingPairingError("an unfinished local pairing or cleanup is present")
	} else if !errors.Is(err, config.ErrJournalNotFound) {
		return config.Config{}, pendingPairingError("the local pairing journal cannot be safely read")
	}
	value, configErr := a.Config.Load()
	if configErr != nil {
		if errors.Is(configErr, config.ErrNotFound) {
			return config.Config{}, commandError("not_paired", "no local client configuration exists", "pair this device before changing client settings", ExitAuth)
		}
		return config.Config{}, commandError("local_state_invalid", "configuration cannot be safely read", "run edu-agent device forget-local", ExitInput)
	}
	if err := value.Validate(); err != nil {
		return config.Config{}, commandError("invalid_configuration", "client configuration is invalid or unsafe", "run edu-agent device forget-local", ExitInput)
	}
	if !value.HasPairingBinding() {
		if _, credentialErr := a.Credentials.Load(); errors.Is(credentialErr, credentials.ErrNotFound) {
			return config.Config{}, commandError("not_paired", "local client settings exist without a device binding", "pair this device before starting the Agent", ExitAuth)
		} else if credentialErr != nil {
			return config.Config{}, commandError("local_state_invalid", "the credential store cannot be safely read", "run edu-agent device forget-local", ExitInput)
		}
		return config.Config{}, commandError("local_state_orphaned", "a credential exists without a device binding", "run edu-agent device forget-local", ExitInput)
	}
	record, credentialErr := a.Credentials.Load()
	if credentialErr != nil {
		if errors.Is(credentialErr, credentials.ErrNotFound) {
			return config.Config{}, commandError("local_state_orphaned", "configuration exists without a credential", "run edu-agent device forget-local", ExitInput)
		}
		return config.Config{}, commandError("local_state_invalid", "the credential store cannot be safely read", "run edu-agent device forget-local", ExitInput)
	}
	if strings.TrimSpace(record.Token) == "" || record.ServerURL != value.ServerURL || record.DeviceID != value.DeviceID {
		return config.Config{}, commandError("device_mismatch", "configuration and credential bindings disagree", "run edu-agent device forget-local", ExitConflict)
	}
	return value, nil
}

func clientConfigUsage(message string) error {
	return commandError("usage", message, "run edu-agent config show or edu-agent config set --timeout DURATION --color never|auto|always", ExitInput)
}
