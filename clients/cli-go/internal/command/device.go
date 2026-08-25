package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/credentials"
)

type binding struct {
	Config config.Config
	Token  string
}

func (a *App) runPair(ctx context.Context, args []string) error {
	set := newFlagSet("pair")
	var flags onlineFlags
	addOnlineFlags(set, &flags)
	name := ""
	set.StringVar(&name, "name", "", "device display name")
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 {
		return commandError("usage", "pair accepts only non-secret flags and never accepts --code", "pipe one code line or enter it at the hidden TTY prompt", ExitInput)
	}
	if err := a.requireEmptyLocalState(); err != nil {
		return err
	}
	resolved, err := config.Resolve(config.Config{}, flags.overrides(), a.Getenv)
	if err != nil {
		return commandError("invalid_server_url", "the server URL or connection settings are unsafe", "use HTTPS or explicitly approve non-loopback plaintext HTTP", ExitInput)
	}
	if name == "" {
		name, _ = os.Hostname()
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "CLI"
	}
	if len([]rune(name)) > 100 {
		return commandError("invalid_device_name", "device name exceeds 100 characters", "choose a shorter device name", ExitInput)
	}
	code, err := a.Terminal.ReadSecret("Pairing code: ")
	if err != nil || strings.TrimSpace(code) == "" {
		return commandError("invalid_pairing_input", "one non-empty pairing code is required", "create a new code on the server and enter one line", ExitInput)
	}
	if warning := config.InsecureWarning(resolved); warning != "" {
		_, _ = fmt.Fprintln(a.Err, warning)
	}
	timeout, _ := config.ParseTimeout(resolved.Timeout)
	issued, pairErr := a.NewClient(resolved.ServerURL, "", timeout).Pair(ctx, code, name)
	code = ""
	if pairErr != nil {
		var apiErr *api.APIError
		if errors.As(pairErr, &apiErr) && apiErr.Status != 502 && apiErr.Status != 503 && apiErr.Status != 504 {
			return mapAPIError(pairErr)
		}
		return commandError("pairing_result_unknown", "the pairing response was not confirmed and the one-time code was not replayed", "create a new code and use an existing device to inspect or revoke a possibly created device", ExitUnavailable)
	}
	if issued.Device.ID == "" || issued.Device.DisplayName == "" || issued.Token == "" {
		return commandError("protocol_error", "the pairing response omitted required credential fields", "check the server version", ExitInternal)
	}
	resolved.DeviceID = issued.Device.ID
	resolved.DisplayName = issued.Device.DisplayName
	record := credentials.Record{ServerURL: resolved.ServerURL, DeviceID: issued.Device.ID, Token: issued.Token}
	if err := persistPairing(a.Config, a.Credentials, resolved, record); err != nil {
		return err
	}
	_, err = fmt.Fprintf(a.Out, "Server: %s\nDevice: %s\nDevice ID: %s\n", safeText(resolved.ServerURL), safeText(issued.Device.DisplayName), safeText(issued.Device.ID))
	return err
}

func persistPairing(configStore ConfigStore, credentialStore CredentialStore, value config.Config, record credentials.Record) error {
	journal := config.PairingJournal{ServerURL: value.ServerURL, DeviceID: value.DeviceID, DisplayName: value.DisplayName}
	if err := configStore.SavePairingJournal(journal); err != nil {
		return commandError("pairing_journal_failed", "the fail-closed pairing journal was not saved", "repair local storage and revoke the remote device from another paired device", ExitInternal)
	}
	if err := credentialStore.Save(record); err != nil {
		if !compensatePairing(configStore, credentialStore) {
			return pendingPairingError("credential publication failed and local cleanup was incomplete")
		}
		return commandError("credential_save_failed", "the device credential was not saved", "repair local storage permissions and pair again", ExitInternal)
	}
	if err := configStore.Save(value); err != nil {
		if !compensatePairing(configStore, credentialStore) {
			return pendingPairingError("configuration publication failed and local cleanup was incomplete")
		}
		return commandError("config_save_failed", "configuration publication failed and the new local state was removed", "repair local storage and pair again", ExitInternal)
	}
	if err := configStore.DeletePairingJournal(); err != nil {
		return pendingPairingError("the binding was written but its pending journal could not be cleared")
	}
	return nil
}

func compensatePairing(configStore ConfigStore, credentialStore CredentialStore) bool {
	credentialErr := credentialStore.Delete()
	configErr := configStore.Delete()
	if credentialErr != nil || configErr != nil {
		return false
	}
	return configStore.DeletePairingJournal() == nil
}

func pendingPairingError(detail string) error {
	return commandError("local_state_pending", detail, "run edu-agent device forget-local before any network command", ExitInternal)
}

func (a *App) requireEmptyLocalState() error {
	if _, journalErr := a.Config.LoadPairingJournal(); journalErr == nil {
		return pendingPairingError("an unfinished pairing journal is present")
	} else if !errors.Is(journalErr, config.ErrJournalNotFound) {
		return pendingPairingError("the pairing journal cannot be safely read")
	}
	_, configErr := a.Config.Load()
	_, credentialErr := a.Credentials.Load()
	configMissing := errors.Is(configErr, config.ErrNotFound)
	credentialMissing := errors.Is(credentialErr, credentials.ErrNotFound)
	if configMissing && credentialMissing {
		return nil
	}
	if configErr != nil && !configMissing || credentialErr != nil && !credentialMissing {
		return commandError("local_state_invalid", "existing local state cannot be safely read", "run edu-agent device forget-local", ExitInput)
	}
	if configMissing != credentialMissing {
		return commandError("local_state_orphaned", "configuration and credential halves do not match", "run edu-agent device forget-local", ExitInput)
	}
	return commandError("already_paired", "a local device binding already exists", "run edu-agent logout or device forget-local before pairing again", ExitConflict)
}

func (a *App) runDevice(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return commandError("usage", "device requires status or forget-local", "run edu-agent device status or device forget-local", ExitInput)
	}
	switch args[0] {
	case "status":
		return a.runDeviceStatus(ctx, args[1:])
	case "forget-local":
		if len(args) != 1 {
			return commandError("usage", "device forget-local accepts no flags", "run edu-agent device forget-local", ExitInput)
		}
		return a.runForgetLocal()
	default:
		return commandError("usage", "unknown device command "+args[0], "run edu-agent device status or device forget-local", ExitInput)
	}
}

func (a *App) runDeviceStatus(ctx context.Context, args []string) error {
	set := newFlagSet("device status")
	var flags onlineFlags
	addOnlineFlags(set, &flags)
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 {
		return commandError("usage", "device status accepts only connection flags", "run edu-agent device status", ExitInput)
	}
	bound, timeout, err := a.loadBinding(flags.overrides())
	if err != nil {
		return err
	}
	a.printInsecureWarning(bound.Config)
	client := a.NewClient(bound.Config.ServerURL, bound.Token, timeout)
	devices, err := client.Devices(ctx)
	if err != nil {
		return mapAPIError(err)
	}
	var current *api.Device
	for index := range devices.Devices {
		if devices.Devices[index].ID == bound.Config.DeviceID {
			current = &devices.Devices[index]
			break
		}
	}
	if current == nil {
		return commandError("device_mismatch", "the authenticated device list does not contain the local device ID", "run device forget-local and pair again after inspecting remote devices", ExitConflict)
	}
	readiness, err := client.Readiness(ctx)
	if err != nil {
		return mapAPIError(err)
	}
	model, err := client.ModelCapabilities(ctx)
	if err != nil {
		return mapAPIError(err)
	}
	scopes := append([]string(nil), current.Scopes...)
	sort.Strings(scopes)
	_, _ = fmt.Fprintf(a.Out, "Server: %s\nReachability: reachable\nReadiness: %s\nDevice: %s\nDevice ID: %s\nScopes: %s\n",
		safeText(bound.Config.ServerURL), safeText(readiness.Status), safeText(current.DisplayName), safeText(current.ID), safeText(strings.Join(scopes, ", ")))
	componentNames := make([]string, 0, len(readiness.Components))
	for name := range readiness.Components {
		componentNames = append(componentNames, name)
	}
	sort.Strings(componentNames)
	for _, name := range componentNames {
		component := readiness.Components[name]
		if component.Reason == "" {
			_, _ = fmt.Fprintf(a.Out, "Component %s: %s\n", safeText(name), safeText(component.Status))
		} else {
			_, _ = fmt.Fprintf(a.Out, "Component %s: %s (%s)\n", safeText(name), safeText(component.Status), safeText(component.Reason))
		}
	}
	modelStatus := "compatible"
	if !model.Compatible {
		modelStatus = "degraded"
	}
	_, err = fmt.Fprintf(a.Out, "Model: %s\n", safeText(modelStatus))
	return err
}

func (a *App) runLogout(ctx context.Context, args []string) error {
	set := newFlagSet("logout")
	var flags onlineFlags
	addOnlineFlags(set, &flags)
	if err := set.Parse(args); err != nil || len(set.Args()) != 0 {
		return commandError("usage", "logout accepts only connection flags", "run edu-agent logout", ExitInput)
	}
	bound, timeout, err := a.loadBinding(flags.overrides())
	if err != nil {
		return err
	}
	a.printInsecureWarning(bound.Config)
	err = a.NewClient(bound.Config.ServerURL, bound.Token, timeout).RevokeDevice(ctx, bound.Config.DeviceID)
	if err != nil {
		var apiErr *api.APIError
		if !errors.As(err, &apiErr) || apiErr.Code != "not_found" {
			return mapAPIError(err)
		}
	}
	credentialErr := a.Credentials.Delete()
	configErr := a.Config.Delete()
	if credentialErr != nil || configErr != nil {
		return commandError("local_cleanup_failed", "the remote device is revoked but local state cleanup is incomplete", "run edu-agent device forget-local", ExitInternal)
	}
	_, err = fmt.Fprintln(a.Out, "Remote device revoked. Local credential and configuration removed.")
	return err
}

func (a *App) runForgetLocal() error {
	value, configErr := a.Config.Load()
	record, credentialErr := a.Credentials.Load()
	journal, journalErr := a.Config.LoadPairingJournal()
	configMissing := errors.Is(configErr, config.ErrNotFound)
	credentialMissing := errors.Is(credentialErr, credentials.ErrNotFound)
	journalMissing := errors.Is(journalErr, config.ErrJournalNotFound)
	if configMissing && credentialMissing && journalMissing {
		_, err := fmt.Fprintln(a.Out, "No local device state was found. Remote devices were not changed.")
		return err
	}
	if configErr == nil {
		_, _ = fmt.Fprintf(a.Out, "Local binding: %s %s %s\n", safeText(value.ServerURL), safeText(value.DisplayName), safeText(value.DeviceID))
	} else if credentialErr == nil {
		_, _ = fmt.Fprintf(a.Out, "Local credential binding: %s %s\n", safeText(record.ServerURL), safeText(record.DeviceID))
	} else if journalErr == nil {
		_, _ = fmt.Fprintf(a.Out, "Pending local binding: %s %s %s\n", safeText(journal.ServerURL), safeText(journal.DisplayName), safeText(journal.DeviceID))
	} else {
		_, _ = fmt.Fprintln(a.Out, "Local state is incomplete or unreadable.")
	}
	confirmed, err := a.Terminal.Confirm("Forget local device state?")
	if err != nil {
		return commandError("confirmation_failed", "local confirmation could not be read", "retry in an interactive terminal", ExitInput)
	}
	if !confirmed {
		return commandError("cancelled", "local state was not changed", "rerun the command when ready", ExitInput)
	}
	if journalErr != nil {
		journal = config.PairingJournal{}
		if configErr == nil {
			journal.ServerURL, journal.DeviceID, journal.DisplayName = value.ServerURL, value.DeviceID, value.DisplayName
		} else if credentialErr == nil {
			journal.ServerURL, journal.DeviceID = record.ServerURL, record.DeviceID
		}
	}
	if err := a.Config.SavePairingJournal(journal); err != nil {
		return commandError("local_cleanup_failed", "a fail-closed cleanup journal could not be saved", "repair file permissions and run device forget-local again", ExitInternal)
	}
	credentialDeleteErr := a.Credentials.Delete()
	configDeleteErr := a.Config.Delete()
	if credentialDeleteErr != nil || configDeleteErr != nil {
		return pendingPairingError("some local state could not be removed")
	}
	if err := a.Config.DeletePairingJournal(); err != nil {
		return pendingPairingError("local halves were removed but the cleanup journal remains")
	}
	_, err = fmt.Fprintln(a.Out, "Local state removed. The remote device may still be valid; revoke it from another paired device.")
	return err
}

func (a *App) loadBinding(overrides config.Overrides) (binding, time.Duration, error) {
	if _, journalErr := a.Config.LoadPairingJournal(); journalErr == nil {
		return binding{}, 0, pendingPairingError("an unfinished local pairing or cleanup is present")
	} else if !errors.Is(journalErr, config.ErrJournalNotFound) {
		return binding{}, 0, pendingPairingError("the local pairing journal cannot be safely read")
	}
	value, configErr := a.Config.Load()
	if configErr != nil {
		if errors.Is(configErr, config.ErrNotFound) {
			if _, credentialErr := a.Credentials.Load(); credentialErr == nil {
				return binding{}, 0, commandError("local_state_orphaned", "a credential exists without configuration", "run edu-agent device forget-local", ExitInput)
			}
			return binding{}, 0, commandError("not_paired", "no local device binding exists", "run edu-agent pair", ExitAuth)
		}
		return binding{}, 0, commandError("local_state_invalid", "configuration cannot be safely read", "run edu-agent device forget-local", ExitInput)
	}
	resolved, err := config.Resolve(value, overrides, a.Getenv)
	if err != nil {
		return binding{}, 0, commandError("invalid_configuration", "connection configuration is invalid or unsafe", "repair configuration or use safe explicit flags", ExitInput)
	}
	record, credentialErr := a.Credentials.Load()
	if credentialErr != nil && !errors.Is(credentialErr, credentials.ErrNotFound) {
		return binding{}, 0, commandError("local_state_invalid", "the credential store cannot be safely read", "run edu-agent device forget-local", ExitInput)
	}
	if errors.Is(credentialErr, credentials.ErrNotFound) {
		return binding{}, 0, commandError("local_state_orphaned", "configuration exists without a credential", "run edu-agent device forget-local", ExitInput)
	}
	if record.ServerURL != resolved.ServerURL || record.DeviceID != resolved.DeviceID {
		return binding{}, 0, commandError("device_mismatch", "configuration and credential bindings disagree", "run edu-agent device forget-local", ExitConflict)
	}
	token := record.Token
	environmentToken := strings.TrimSpace(a.Getenv("EDU_AGENT_TOKEN"))
	environmentServer := strings.TrimSpace(a.Getenv("EDU_AGENT_TOKEN_SERVER"))
	environmentDeviceID := strings.TrimSpace(a.Getenv("EDU_AGENT_TOKEN_DEVICE_ID"))
	if environmentToken == "" && (environmentServer != "" || environmentDeviceID != "") {
		return binding{}, 0, commandError("environment_token_binding_invalid", "environment token binding was provided without EDU_AGENT_TOKEN", "set all three token override variables or unset them", ExitInput)
	}
	if environmentToken != "" {
		if environmentServer == "" || environmentDeviceID == "" {
			return binding{}, 0, commandError("environment_token_binding_required", "EDU_AGENT_TOKEN requires explicit server and device bindings", "set EDU_AGENT_TOKEN_SERVER and EDU_AGENT_TOKEN_DEVICE_ID", ExitInput)
		}
		normalizedServer, normalizeErr := config.ValidateServerURL(environmentServer, resolved.AllowInsecureHTTP)
		if normalizeErr != nil || normalizedServer != resolved.ServerURL || environmentDeviceID != resolved.DeviceID {
			return binding{}, 0, commandError("environment_token_binding_mismatch", "the environment token binding does not match local configuration", "use the configured server and device ID or unset EDU_AGENT_TOKEN", ExitConflict)
		}
		token = environmentToken
	}
	if token == "" {
		return binding{}, 0, commandError("local_state_invalid", "the active credential is empty", "run edu-agent device forget-local", ExitInput)
	}
	timeout, err := config.ParseTimeout(resolved.Timeout)
	if err != nil {
		return binding{}, 0, commandError("invalid_timeout", "timeout must be a positive duration", "set a valid timeout", ExitInput)
	}
	return binding{Config: resolved, Token: token}, timeout, nil
}

func (a *App) printInsecureWarning(value config.Config) {
	if warning := config.InsecureWarning(value); warning != "" {
		_, _ = fmt.Fprintln(a.Err, warning)
	}
}
