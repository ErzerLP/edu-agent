package keybackend

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const service = "edu-agent-offline-v1"

var (
	ErrNotFound    = errors.New("offline system key not found")
	ErrUnavailable = errors.New("offline system key backend unavailable")
)

// Account returns a non-secret stable keyring account bound to one server and device.
func Account(normalizedServerURL, deviceID string) string {
	digest := sha256.Sum256([]byte(normalizedServerURL + "\x00" + deviceID))
	return "profile-" + hex.EncodeToString(digest[:])
}

func Available(_ string) bool {
	switch runtime.GOOS {
	case "linux":
		_, err := exec.LookPath("secret-tool")
		return err == nil && strings.TrimSpace(os.Getenv("DBUS_SESSION_BUS_ADDRESS")) != ""
	case "darwin":
		_, err := exec.LookPath("security")
		return err == nil
	case "windows":
		_, err := exec.LookPath("powershell.exe")
		return err == nil && strings.TrimSpace(os.Getenv("LOCALAPPDATA")) != ""
	default:
		return false
	}
}

func Generate() ([]byte, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("generate offline system wrapping key: %w", err)
	}
	return secret, nil
}

func Load(account string) ([]byte, error) {
	encoded, err := loadEncoded(account)
	if err != nil {
		return nil, err
	}
	secret, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(secret) != 32 || base64.RawURLEncoding.EncodeToString(secret) != strings.TrimSpace(encoded) {
		zero(secret)
		return nil, ErrUnavailable
	}
	return secret, nil
}

func Store(account string, secret []byte) error {
	if len(secret) != 32 {
		return ErrUnavailable
	}
	return storeEncoded(account, base64.RawURLEncoding.EncodeToString(secret))
}

func Delete(account string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.CommandContext(ctx, "secret-tool", "clear", "service", service, "account", account)
	case "darwin":
		cmd = exec.CommandContext(ctx, "security", "delete-generic-password", "-s", service, "-a", account)
	case "windows":
		cmd = powershell(ctx, `$path=Join-Path $env:LOCALAPPDATA ('EduAgent\\offline-keys\\'+$env:EDU_AGENT_KEY_ACCOUNT+'.txt'); if(Test-Path -LiteralPath $path){Remove-Item -LiteralPath $path -Force}`, account)
	default:
		return ErrUnavailable
	}
	if err := cmd.Run(); err != nil {
		if runtime.GOOS != "windows" {
			if _, loadErr := Load(account); errors.Is(loadErr, ErrNotFound) {
				return nil
			}
		}
		return ErrUnavailable
	}
	return nil
}

func loadEncoded(account string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.CommandContext(ctx, "secret-tool", "lookup", "service", service, "account", account)
	case "darwin":
		cmd = exec.CommandContext(ctx, "security", "find-generic-password", "-s", service, "-a", account, "-w")
	case "windows":
		cmd = powershell(ctx, `$path=Join-Path $env:LOCALAPPDATA ('EduAgent\\offline-keys\\'+$env:EDU_AGENT_KEY_ACCOUNT+'.txt'); if(!(Test-Path -LiteralPath $path)){exit 44}; $cipher=[IO.File]::ReadAllText($path); $secure=ConvertTo-SecureString $cipher; $ptr=[Runtime.InteropServices.Marshal]::SecureStringToBSTR($secure); try{[Console]::Out.Write([Runtime.InteropServices.Marshal]::PtrToStringBSTR($ptr))}finally{[Runtime.InteropServices.Marshal]::ZeroFreeBSTR($ptr)}`, account)
	default:
		return "", ErrUnavailable
	}
	output, err := cmd.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			switch runtime.GOOS {
			case "linux":
				if exit.ExitCode() == 1 && len(bytes.TrimSpace(exit.Stderr)) == 0 {
					return "", ErrNotFound
				}
			case "darwin", "windows":
				if exit.ExitCode() == 44 {
					return "", ErrNotFound
				}
			}
		}
		return "", ErrUnavailable
	}
	if len(bytes.TrimSpace(output)) == 0 {
		return "", ErrNotFound
	}
	return string(output), nil
}

func storeEncoded(account, encoded string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		cmd = exec.CommandContext(ctx, "secret-tool", "store", "--label=Edu Agent offline key", "service", service, "account", account)
		cmd.Stdin = strings.NewReader(encoded + "\n")
	case "darwin":
		cmd = darwinStoreCommand(ctx, account, encoded)
	case "windows":
		cmd = powershell(ctx, `$directory=Join-Path $env:LOCALAPPDATA 'EduAgent\\offline-keys'; [IO.Directory]::CreateDirectory($directory) | Out-Null; $secret=[Console]::In.ReadToEnd().Trim(); $secure=ConvertTo-SecureString $secret -AsPlainText -Force; $cipher=ConvertFrom-SecureString $secure; $path=Join-Path $directory ($env:EDU_AGENT_KEY_ACCOUNT+'.txt'); [IO.File]::WriteAllText($path,$cipher); $identity=[Security.Principal.WindowsIdentity]::GetCurrent().User; $acl=New-Object Security.AccessControl.FileSecurity; $acl.SetOwner($identity); $acl.SetAccessRuleProtection($true,$false); $rule=New-Object Security.AccessControl.FileSystemAccessRule($identity,'FullControl','Allow'); $acl.AddAccessRule($rule); Set-Acl -LiteralPath $path -AclObject $acl`, account)
		cmd.Stdin = strings.NewReader(encoded)
	default:
		return ErrUnavailable
	}
	if err := cmd.Run(); err != nil {
		return ErrUnavailable
	}
	return nil
}

func darwinStoreCommand(ctx context.Context, account, encoded string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "security", "add-generic-password", "-U", "-s", service, "-a", account, "-w")
	cmd.Stdin = strings.NewReader(encoded + "\n")
	return cmd
}

func powershell(ctx context.Context, script, account string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(os.Environ(), "EDU_AGENT_KEY_ACCOUNT="+account)
	return cmd
}

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
