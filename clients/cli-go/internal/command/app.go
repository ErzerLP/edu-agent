package command

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/credentials"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/id"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/terminal"
)

type ConfigStore interface {
	Load() (config.Config, error)
	Save(config.Config) error
	Delete() error
	LoadPairingJournal() (config.PairingJournal, error)
	SavePairingJournal(config.PairingJournal) error
	DeletePairingJournal() error
}

type CredentialStore interface {
	Load() (credentials.Record, error)
	Save(credentials.Record) error
	Delete() error
}

type Terminal interface {
	ReadSecret(string) (string, error)
	ReadLine(string) (string, error)
	Confirm(string) (bool, error)
	Clear() error
}

type APIClient interface {
	Pair(context.Context, string, string) (api.IssuedCredential, error)
	Devices(context.Context) (api.DevicesResponse, error)
	Readiness(context.Context) (api.Readiness, error)
	ModelCapabilities(context.Context) (api.ModelCapabilities, error)
	RevokeDevice(context.Context, string) error
	KnowledgeHead(context.Context) (api.KnowledgeRevision, error)
	ImportKnowledge(context.Context, api.ImportRequest) (api.ImportResult, error)
	RetrieveKnowledge(context.Context, api.KnowledgeRetrievalRequest) (api.KnowledgeRetrievalResult, error)
	CreateGoal(context.Context, api.LearningGoalRequest) (api.GoalOperationResult, error)
	CreateSession(context.Context, api.TutoringSessionRequest) (api.SessionOperationResult, error)
	CreateProposal(context.Context, api.TutoringProposalRequest) (api.TutoringProposal, error)
	ApplySessionAction(context.Context, string, api.TutoringAction) (api.SessionOperationResult, error)
	DecideAssessment(context.Context, string, api.AssessmentDecisionRequest) (api.AssessmentDecisionOperationResult, error)
	CurrentSession(context.Context) (api.SessionView, error)
	Session(context.Context, string) (api.SessionView, error)
	Timeline(context.Context, string, int, string) (api.TimelinePage, error)
	Routes(context.Context, string, int, bool) (api.RoutesPage, error)
	Node(context.Context, string) (api.NodeView, error)
	Evidence(context.Context, string, int, string) (api.EvidencePage, error)
	Reviews(context.Context, string, int, *time.Time) (api.ReviewsPage, error)
	ProjectionStatus(context.Context) (api.ProjectionStatus, error)
}

type BuildInfo struct {
	Version string
	Commit  string
}

type App struct {
	Config      ConfigStore
	Credentials CredentialStore
	Terminal    Terminal
	Out         io.Writer
	Err         io.Writer
	Getenv      func(string) string
	NewClient   func(string, string, time.Duration) APIClient
	NewUUID     func() (string, error)
	Build       BuildInfo
}

func NewDefault(in io.Reader, out, errOut io.Writer, build BuildInfo) (*App, error) {
	configStore, err := config.DefaultStore()
	if err != nil {
		return nil, err
	}
	credentialPath, err := credentials.DefaultPath()
	if err != nil {
		return nil, err
	}
	return &App{
		Config: configStore, Credentials: credentials.NewFileStore(credentialPath), Terminal: terminal.New(in, out, errOut),
		Out: out, Err: errOut, Getenv: os.Getenv, NewUUID: id.NewUUID, Build: build,
		NewClient: func(serverURL, token string, timeout time.Duration) APIClient {
			client := api.NewClient(serverURL, token, timeout, http.DefaultClient)
			return client
		},
	}, nil
}

func (a *App) Run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return a.fail(commandError("usage", "a command is required", "use edu-agent version, pair, device, knowledge, goal, learn, assessment, route, progress, evidence, reviews, logout, or clear", ExitInput))
	}
	if err := a.dispatch(ctx, args); err != nil {
		return a.fail(err)
	}
	return ExitOK
}

// dispatch keeps command parsing centralized while workflows remain in focused files.
func (a *App) dispatch(ctx context.Context, args []string) error {
	switch args[0] {
	case "version":
		if len(args) != 1 {
			return commandError("usage", "version accepts no arguments", "run edu-agent version", ExitInput)
		}
		version, commit := a.Build.Version, a.Build.Commit
		if version == "" {
			version = "dev"
		}
		if commit == "" {
			commit = "unknown"
		}
		_, err := fmt.Fprintf(a.Out, "edu-agent %s commit=%s go=%s %s/%s\n", safeText(version), safeText(commit), safeText(runtime.Version()), safeText(runtime.GOOS), safeText(runtime.GOARCH))
		return err
	case "pair":
		return a.runPair(ctx, args[1:])
	case "device":
		return a.runDevice(ctx, args[1:])
	case "logout":
		return a.runLogout(ctx, args[1:])
	case "knowledge":
		return a.runKnowledge(ctx, args[1:])
	case "goal":
		return a.runGoal(ctx, args[1:])
	case "learn":
		return a.runLearn(ctx, args[1:])
	case "assessment":
		return a.runAssessment(ctx, args[1:])
	case "route":
		return a.runRoute(ctx, args[1:])
	case "progress":
		return a.runProgress(ctx, args[1:])
	case "evidence":
		return a.runEvidence(ctx, args[1:])
	case "reviews":
		return a.runReviews(ctx, args[1:])
	case "clear":
		if len(args) != 1 {
			return commandError("usage", "clear accepts no arguments", "run edu-agent clear", ExitInput)
		}
		if clearErr := a.Terminal.Clear(); clearErr != nil {
			if errors.Is(clearErr, terminal.ErrNotTerminal) {
				return commandError("not_a_terminal", "clear emits no control sequence when stdout is not a terminal", "run clear only in an interactive terminal", ExitInput)
			}
			return commandError("terminal_error", "the visible viewport could not be cleared", "check terminal capabilities", ExitInternal)
		}
		return nil
	default:
		return commandError("usage", "unknown command "+args[0], "use edu-agent version, pair, device, knowledge, goal, learn, assessment, route, progress, evidence, reviews, logout, or clear", ExitInput)
	}
}

func (a *App) fail(err error) int {
	var commandErr *Error
	if !errors.As(err, &commandErr) {
		commandErr = mapAPIError(err)
	}
	_, _ = fmt.Fprintln(a.Err, safeText(commandErr.Error()))
	return commandErr.ExitCode
}

type optionalBool struct {
	set   bool
	value bool
}

func (v *optionalBool) String() string { return fmt.Sprintf("%t", v.value) }
func (v *optionalBool) Set(input string) error {
	parsed, err := parseBool(input)
	if err != nil {
		return err
	}
	v.set, v.value = true, parsed
	return nil
}
func (v *optionalBool) IsBoolFlag() bool { return true }

func parseBool(value string) (bool, error) {
	switch strings.ToLower(value) {
	case "true", "1", "yes":
		return true, nil
	case "false", "0", "no":
		return false, nil
	default:
		return false, errors.New("value must be true or false")
	}
}

type onlineFlags struct {
	serverURL string
	timeout   string
	color     string
	insecure  optionalBool
}

func addOnlineFlags(set *flag.FlagSet, values *onlineFlags) {
	set.StringVar(&values.serverURL, "server", "", "server URL")
	set.StringVar(&values.timeout, "timeout", "", "total request timeout")
	set.StringVar(&values.color, "color", "", "never, auto, or always")
	set.Var(&values.insecure, "allow-insecure-http", "allow plaintext HTTP to a non-loopback server")
}

func (f onlineFlags) overrides() config.Overrides {
	result := config.Overrides{ServerURL: f.serverURL, Timeout: f.timeout, Color: f.color}
	if f.insecure.set {
		result.AllowInsecureHTTP = &f.insecure.value
	}
	return result
}

func newFlagSet(name string) *flag.FlagSet {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(io.Discard)
	return set
}

func safeText(value string) string { return terminal.EscapeText(value) }
