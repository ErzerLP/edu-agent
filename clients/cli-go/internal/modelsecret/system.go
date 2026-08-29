package modelsecret

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/darwinkeychain"
)

var errBackendNotFound = errors.New("system secret not found")

func (systemBackend) Get(service, account string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var command *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		command = exec.CommandContext(ctx, "secret-tool", "lookup", "service", service, "account", account)
	case "darwin":
		value, err := darwinkeychain.Load(ctx, service, account)
		if errors.Is(err, darwinkeychain.ErrNotFound) {
			return "", errBackendNotFound
		}
		if err != nil {
			return "", ErrUnavailable
		}
		return value, nil
	case "windows":
		command = modelPowerShell(ctx, `$path=Join-Path $env:LOCALAPPDATA ('EduAgent\\model-keys\\'+$env:EDU_AGENT_KEY_ACCOUNT+'.txt'); if(!(Test-Path -LiteralPath $path)){exit 44}; $cipher=[IO.File]::ReadAllText($path); $secure=ConvertTo-SecureString $cipher; $ptr=[Runtime.InteropServices.Marshal]::SecureStringToBSTR($secure); try{[Console]::Out.Write([Runtime.InteropServices.Marshal]::PtrToStringBSTR($ptr))}finally{[Runtime.InteropServices.Marshal]::ZeroFreeBSTR($ptr)}`, account)
	default:
		return "", ErrUnavailable
	}
	output, err := command.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			switch runtime.GOOS {
			case "linux":
				if exit.ExitCode() == 1 && len(bytes.TrimSpace(exit.Stderr)) == 0 {
					return "", errBackendNotFound
				}
			case "windows":
				if exit.ExitCode() == 44 {
					return "", errBackendNotFound
				}
			}
		}
		return "", ErrUnavailable
	}
	value := strings.TrimSpace(string(output))
	if value == "" {
		return "", errBackendNotFound
	}
	return value, nil
}

func (systemBackend) Set(service, account, value string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var command *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		command = exec.CommandContext(ctx, "secret-tool", "store", "--label=Edu Agent model key", "service", service, "account", account)
		command.Stdin = strings.NewReader(value + "\n")
	case "darwin":
		if err := darwinkeychain.Store(ctx, service, account, value); err != nil {
			return ErrUnavailable
		}
		return nil
	case "windows":
		command = modelPowerShell(ctx, `$directory=Join-Path $env:LOCALAPPDATA 'EduAgent\\model-keys'; [IO.Directory]::CreateDirectory($directory) | Out-Null; $secret=[Console]::In.ReadToEnd().Trim(); $secure=ConvertTo-SecureString $secret -AsPlainText -Force; $cipher=ConvertFrom-SecureString $secure; $path=Join-Path $directory ($env:EDU_AGENT_KEY_ACCOUNT+'.txt'); [IO.File]::WriteAllText($path,$cipher); $identity=[Security.Principal.WindowsIdentity]::GetCurrent().User; $acl=New-Object Security.AccessControl.FileSecurity; $acl.SetOwner($identity); $acl.SetAccessRuleProtection($true,$false); $rule=New-Object Security.AccessControl.FileSystemAccessRule($identity,'FullControl','Allow'); $acl.AddAccessRule($rule); Set-Acl -LiteralPath $path -AclObject $acl`, account)
		command.Stdin = strings.NewReader(value)
	default:
		return ErrUnavailable
	}
	if err := command.Run(); err != nil {
		return ErrUnavailable
	}
	return nil
}

func (backend systemBackend) Delete(service, account string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var command *exec.Cmd
	switch runtime.GOOS {
	case "linux":
		command = exec.CommandContext(ctx, "secret-tool", "clear", "service", service, "account", account)
	case "darwin":
		err := darwinkeychain.Delete(ctx, service, account)
		if errors.Is(err, darwinkeychain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return ErrUnavailable
		}
		return nil
	case "windows":
		command = modelPowerShell(ctx, `$path=Join-Path $env:LOCALAPPDATA ('EduAgent\\model-keys\\'+$env:EDU_AGENT_KEY_ACCOUNT+'.txt'); if(Test-Path -LiteralPath $path){Remove-Item -LiteralPath $path -Force}`, account)
	default:
		return ErrUnavailable
	}
	if err := command.Run(); err != nil {
		if _, loadErr := backend.Get(service, account); errors.Is(loadErr, errBackendNotFound) {
			return nil
		}
		return ErrUnavailable
	}
	return nil
}

func modelPowerShell(ctx context.Context, script, account string) *exec.Cmd {
	command := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	command.Env = append(os.Environ(), "EDU_AGENT_KEY_ACCOUNT="+account)
	return command
}
