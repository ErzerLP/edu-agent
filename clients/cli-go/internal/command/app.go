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

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentsession"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentui"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/config"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/credentials"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/dashboard"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/id"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelclient"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelsecret"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/offline"
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

type OfflineKeyStore interface {
	Available(string) bool
	Generate() ([]byte, error)
	Load(string) ([]byte, error)
	Store(string, []byte) error
	Delete(string) error
}

type CredentialStore interface {
	Load() (credentials.Record, error)
	Save(credentials.Record) error
	Delete() error
}

type ModelSecretStore interface {
	Load(string) (string, error)
	Save(string, string) error
	Delete(string) error
}

type AgentUIRunner interface {
	Run(context.Context, agentui.Conversation, string) error
}

type AgentSessionPickerRunner interface {
	Pick(context.Context, agentui.SessionPickerSource, bool) (agentui.PickerChoice, error)
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
	NotesyncStatus(context.Context) (api.NotesyncStatus, error)
	NotesyncPreview(context.Context, api.NotesyncPreviewRequest) (api.NotesyncPreviewResult, error)
	NotesyncReviews(context.Context, string, string, int) (api.NotesyncReviewPage, error)
	NotesyncReview(context.Context, string) (api.NotesyncReview, error)
	ResolveNotesyncReview(context.Context, string, api.NotesyncResolutionRequest) (api.NotesyncResolutionResult, error)
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
	MemoryCandidates(context.Context, string, int) (api.MemoryCandidatePage, error)
	MemoryCandidate(context.Context, string) (api.MemoryCandidateView, error)
	ExportMemory(context.Context, string, int) (api.MemoryExportPage, error)
	CreateMemoryCandidate(context.Context, api.MemoryCandidateRequest) (api.MemoryOperationResponse, error)
	DecideMemoryCandidate(context.Context, string, api.MemoryCandidateDecisionRequest) (api.MemoryOperationResponse, error)
	ProjectionStatus(context.Context) (api.ProjectionStatus, error)
	PrepareOffline(context.Context, api.OfflinePrepareRequest) (api.OfflinePrepareResponse, int, error)
	SyncOfflineCanonical(context.Context, []byte) (api.OfflineSyncResponse, error)
	OfflineOperationStatus(context.Context, string) (api.OfflineOperationStatus, error)
	OfflineAssessments(context.Context, string, int, string) (api.OfflineAssessmentPage, error)
	OfflineAssessment(context.Context, string) (api.OfflineAssessmentView, error)
	DecideOfflineAssessment(context.Context, string, api.OfflineAssessmentDecisionRequest) (api.OfflineAssessmentDecisionReceipt, error)
}

type BuildInfo struct {
	Version string
	Commit  string
}

type DashboardRunner interface {
	Run(context.Context, dashboard.Snapshot) ([]string, bool, error)
}

type dashboardModelKeySource interface {
	TakeModelKey() (string, bool)
}

type App struct {
	Config              ConfigStore
	Credentials         CredentialStore
	ModelSecrets        ModelSecretStore
	Terminal            Terminal
	Dashboard           DashboardRunner
	AgentUI             AgentUIRunner
	InputIsTTY          func() bool
	OutputIsTTY         func() bool
	Out                 io.Writer
	Err                 io.Writer
	Getenv              func(string) string
	NewClient           func(string, string, time.Duration) APIClient
	NewModel            func(config.AgentConfig, string) (agentloop.Model, error)
	NewUUID             func() (string, error)
	OfflineRoot         func() (string, error)
	OfflineKeys         OfflineKeyStore
	AgentSessionRoot    func() (string, error)
	AgentSessionSecrets agentsession.SecretBackend
	Build               BuildInfo

	dashboardMode bool
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
	terminalIO := terminal.New(in, out, errOut)
	modelSecrets := modelsecret.New()
	return &App{
		Config: configStore, Credentials: credentials.NewFileStore(credentialPath), ModelSecrets: modelSecrets, Terminal: terminalIO,
		Dashboard: &dashboard.Runner{In: in, Out: out}, AgentUI: defaultAgentUIRunner{in: in, out: out},
		InputIsTTY: terminalIO.InputIsTTY, OutputIsTTY: terminalIO.OutputIsTTY,
		Out: out, Err: errOut, Getenv: os.Getenv, NewUUID: id.NewUUID, OfflineRoot: offline.DefaultRoot, OfflineKeys: platformOfflineKeyStore{},
		AgentSessionRoot: agentsession.DefaultRoot, Build: build,
		NewClient: func(serverURL, token string, timeout time.Duration) APIClient {
			client := api.NewClient(serverURL, token, timeout, http.DefaultClient)
			return client
		},
		NewModel: func(value config.AgentConfig, apiKey string) (agentloop.Model, error) {
			timeout, err := config.ParseTimeout(value.Timeout)
			if err != nil {
				return nil, err
			}
			return modelclient.New(value.BaseURL, value.Model, apiKey, timeout, http.DefaultClient)
		},
	}, nil
}

func (a *App) Run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		if a.interactiveDashboardAvailable() {
			return a.runDashboard(ctx)
		}
		return a.fail(commandError("usage", "a command is required", "use edu-agent version, pair, device, config, knowledge, goal, learn, assessment, route, progress, evidence, reviews, logout, or clear", ExitInput))
	}
	if err := a.dispatch(ctx, args); err != nil {
		return a.fail(err)
	}
	return ExitOK
}

func (a *App) interactiveTerminalAvailable() bool {
	if a.Getenv != nil && strings.EqualFold(strings.TrimSpace(a.Getenv("TERM")), "dumb") {
		return false
	}
	return a.InputIsTTY != nil && a.OutputIsTTY != nil && a.InputIsTTY() && a.OutputIsTTY()
}

func (a *App) interactiveDashboardAvailable() bool {
	return a.Dashboard != nil && a.interactiveTerminalAvailable()
}

func (a *App) runDashboard(ctx context.Context) int {
	lastExit := ExitOK
	for {
		args, quit, err := a.Dashboard.Run(ctx, a.dashboardSnapshot())
		if err != nil {
			if ctx.Err() != nil {
				return lastExit
			}
			return a.fail(commandError("terminal_error", "交互式主控制台无法启动", "检查终端能力，或改用显式子命令", ExitInternal))
		}
		if source, ok := a.Dashboard.(dashboardModelKeySource); ok {
			if modelKey, present := source.TakeModelKey(); present {
				if err := a.saveDashboardAgentKey(modelKey); err != nil {
					lastExit = a.fail(err)
				} else {
					lastExit = ExitOK
				}
				if ctx.Err() != nil {
					return lastExit
				}
				if err := a.waitForDashboardReturn(ctx); err != nil {
					return lastExit
				}
				continue
			}
		}
		if quit {
			return lastExit
		}
		if len(args) == 0 {
			return a.fail(commandError("terminal_error", "交互式主控制台没有返回可执行操作", "重新启动客户端，或改用显式子命令", ExitInternal))
		}
		lastExit = a.runDashboardCommand(ctx, args)
		if len(args) > 0 && args[0] == "agent" && lastExit == ExitOK {
			continue
		}
		if ctx.Err() != nil {
			return lastExit
		}
		if err := a.waitForDashboardReturn(ctx); err != nil {
			return lastExit
		}
	}
}

func (a *App) runDashboardCommand(ctx context.Context, args []string) int {
	originalOut, originalErr := a.Out, a.Err
	a.Out = dashboardOutputWriter{target: originalOut}
	a.Err = dashboardOutputWriter{target: originalErr}
	a.dashboardMode = true
	defer func() {
		a.dashboardMode = false
		a.Out, a.Err = originalOut, originalErr
	}()
	if err := a.dispatch(ctx, args); err != nil {
		return a.fail(err)
	}
	return ExitOK
}

func (a *App) dashboardText(plain, localized string) string {
	if a.dashboardMode {
		return localized
	}
	return plain
}

func (a *App) waitForDashboardReturn(ctx context.Context) error {
	result := make(chan error, 1)
	go func() {
		_, err := a.Terminal.ReadLine("\n按 Enter 返回主控制台...")
		result <- err
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-result:
		return err
	}
}

func (a *App) dashboardSnapshot() dashboard.Snapshot {
	snapshot := dashboard.Snapshot{
		ServerURL:  config.DefaultServerURL,
		Timeout:    config.DefaultTimeout.String(),
		LocalState: dashboard.LocalStateUnpaired,
	}
	value, configErr := a.Config.Load()
	if configErr == nil {
		if strings.TrimSpace(value.ServerURL) != "" {
			snapshot.ServerURL = safeText(value.ServerURL)
		}
		if strings.TrimSpace(value.Timeout) != "" {
			snapshot.Timeout = safeText(value.Timeout)
		}
		snapshot.Color = safeText(value.Color)
		snapshot.DeviceName = safeText(value.DisplayName)
	}
	if value.Agent != nil {
		snapshot.AgentProvider = safeText(value.Agent.Provider)
		snapshot.AgentBaseURL = safeText(value.Agent.BaseURL)
		snapshot.AgentModel = safeText(value.Agent.Model)
		snapshot.AgentContextWindow = value.Agent.ContextWindow
		snapshot.AgentMaxTokens = value.Agent.MaxTokens
		snapshot.AgentContextCompaction = safeText(value.Agent.ContextCompaction)
		snapshot.AgentReasoningEffort = safeText(value.Agent.ReasoningEffort)
		snapshot.AgentSessionHistory = safeText(value.Agent.SessionHistory)
		snapshot.AgentTimeout = safeText(value.Agent.Timeout)
		snapshot.AgentMaxToolRounds = value.Agent.MaxToolRounds
	}
	if a.ModelSecrets != nil && value.Agent != nil {
		binding := modelsecret.Binding(value.Agent.Provider, value.Agent.BaseURL)
		if key, err := a.ModelSecrets.Load(binding); err == nil && strings.TrimSpace(key) != "" {
			snapshot.AgentKeyConfigured = true
		} else if err != nil && !errors.Is(err, modelsecret.ErrNotFound) {
			snapshot.AgentKeyBackendUnavailable = true
		}
	}
	record, credentialErr := a.Credentials.Load()
	_, journalErr := a.Config.LoadPairingJournal()

	configMissing := errors.Is(configErr, config.ErrNotFound)
	credentialMissing := errors.Is(credentialErr, credentials.ErrNotFound)
	journalMissing := errors.Is(journalErr, config.ErrJournalNotFound)
	if configMissing && credentialMissing && journalMissing {
		return snapshot
	}
	if configErr == nil && !value.HasPairingBinding() && credentialMissing && journalMissing {
		return snapshot
	}
	if configErr != nil || credentialErr != nil || !journalMissing {
		snapshot.LocalState = dashboard.LocalStateIncomplete
		return snapshot
	}
	validated := value
	if validated.Validate() != nil || strings.TrimSpace(record.Token) == "" || record.ServerURL != validated.ServerURL || record.DeviceID != validated.DeviceID {
		snapshot.LocalState = dashboard.LocalStateIncomplete
		return snapshot
	}
	snapshot.LocalState = dashboard.LocalStatePaired
	return snapshot
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
	case "config":
		return a.runClientConfig(args[1:])
	case "model":
		return a.runModel(ctx, args[1:])
	case "agent":
		return a.runAgent(ctx, args[1:])
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
	case "offline":
		return a.runOffline(ctx, args[1:])
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
		return commandError("usage", "unknown command "+args[0], "use edu-agent version, pair, device, config, knowledge, goal, learn, assessment, route, progress, evidence, reviews, logout, or clear", ExitInput)
	}
}

func (a *App) fail(err error) int {
	var commandErr *Error
	if !errors.As(err, &commandErr) {
		commandErr = mapAPIError(err)
	}
	if a.dashboardMode {
		_, _ = fmt.Fprintln(a.Err, safeText(formatDashboardError(commandErr)))
	} else {
		_, _ = fmt.Fprintln(a.Err, safeText(commandErr.Error()))
	}
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
