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
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/modelgateway"
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

type App struct {
	agentWorkflow       bool
	Config              ConfigStore
	Credentials         CredentialStore
	Terminal            Terminal
	Dashboard           DashboardRunner
	AgentUI             AgentUIRunner
	InputIsTTY          func() bool
	OutputIsTTY         func() bool
	Out                 io.Writer
	Err                 io.Writer
	Getenv              func(string) string
	NewClient           func(string, string, time.Duration) APIClient
	NewModel            func(*modelgateway.Model) (agentloop.Model, error)
	GatewayTransport    http.RoundTripper
	NewUUID             func() (string, error)
	OfflineRoot         func() (string, error)
	OfflineKeys         OfflineKeyStore
	AgentSessionRoot    func() (string, error)
	AgentSessionSecrets agentsession.SecretBackend
	Build               BuildInfo

	strictLearningConflicts bool
	dashboardMode           bool
	learningSpace           string
	learningSpaceName       string
	learningSessions        map[string]string
	learningDrafts          map[string][]string
	learningInputDrafts     map[string]string
	importDrafts            map[string]*importDraft
	importedScope           string
	teachingInput           io.Reader
	teachingOutput          io.Writer
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
	return &App{
		Config: configStore, Credentials: credentials.NewFileStore(credentialPath), Terminal: terminalIO,
		Dashboard: &dashboard.Runner{In: in, Out: out}, AgentUI: defaultAgentUIRunner{in: in, out: out},
		InputIsTTY: terminalIO.InputIsTTY, OutputIsTTY: terminalIO.OutputIsTTY,
		Out: out, Err: errOut, Getenv: os.Getenv, NewUUID: id.NewUUID, OfflineRoot: offline.DefaultRoot, OfflineKeys: platformOfflineKeyStore{},
		teachingInput: in, teachingOutput: out,
		AgentSessionRoot: agentsession.DefaultRoot, Build: build,
		NewClient: func(serverURL, token string, timeout time.Duration) APIClient {
			client := api.NewClient(serverURL, token, timeout, http.DefaultClient)
			return client
		},
	}, nil
}

func (a *App) Run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		if a.interactiveDashboardAvailable() {
			return a.runDashboard(ctx)
		}
		return a.fail(commandError("usage", "a command is required", "使用 edu-agent version, pair, device, config, space, knowledge, goal, study, learn, assessment, route, progress, evidence, reviews, logout 或 clear", ExitInput))
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
	if runner, ok := a.Dashboard.(*dashboard.Runner); ok {
		runner.Workbench = workbenchService{app: *a}
		runner.LearningSpaceID = a.learningSpace
	}
	lastExit := ExitOK
	for {
		args, quit, err := a.Dashboard.Run(ctx, a.dashboardSnapshot())
		if runner, ok := a.Dashboard.(*dashboard.Runner); ok {
			if selected, name := runner.SelectedLearningSpace(); selected != "" {
				a.learningSpace, a.learningSpaceName = selected, name
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				return lastExit
			}
			return a.fail(commandError("terminal_error", "交互式主控制台无法启动", "检查终端能力，或改用显式子命令", ExitInternal))
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
		if len(args) > 2 && args[0] == "knowledge" && args[1] == "import" && args[2] == "wizard" && lastExit == ExitOK {
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
		LearningSpaceName: safeText(a.learningSpaceName),
		ServerURL:         config.DefaultServerURL,
		Timeout:           config.DefaultTimeout.String(),
		LocalState:        dashboard.LocalStateUnpaired,
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
		snapshot.AgentContextCompaction = value.Agent.ContextCompaction
		snapshot.AgentReasoningEffort = value.Agent.ReasoningEffort
		snapshot.AgentSessionHistory = value.Agent.SessionHistory
	}
	if value.Gateway != nil && value.Gateway.ServerURL == value.ServerURL && value.Gateway.DeviceID == value.DeviceID {
		snapshot.SelectedTarget = value.Gateway.SelectedTarget
		snapshot.PrivacyConfirmed = value.Gateway.NoticeVersion == modelgateway.PrivacyVersion
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
	if snapshot.PrivacyConfirmed {
		client, e := a.gatewayClient(value, record)
		if e == nil {
			cfg, fetchErr := client.Config(context.Background())
			if fetchErr == nil {
				snapshot.Models = cfg.Models
				for _, m := range cfg.Models {
					if m.Target == snapshot.SelectedTarget {
						snapshot.AgentModel = safeText(m.ModelName)
						snapshot.AgentContextWindow = m.ContextTokens
						snapshot.AgentMaxTokens = m.OutputTokens
					}
				}
			} else {
				snapshot.GatewayError = safeText(fetchErr.Error())
			}
		}
	}
	return snapshot
}

// dispatch keeps command parsing centralized while workflows remain in focused files.
func (a *App) dispatch(ctx context.Context, args []string) error {
	var err error
	args, restore, err := a.parseSpaceFlag(args)
	if err != nil {
		return err
	}
	defer restore()
	if len(args) == 0 {
		return commandError("usage", "a command is required", "use space help", ExitInput)
	}
	switch args[0] {
	case "study":
		return a.runStudy(ctx, args[1:])
	case "space":
		return a.runSpace(ctx, args[1:])
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
		return a.runScopedProgress(ctx, args[1:], false)
	case "evidence":
		return a.runEvidence(ctx, args[1:])
	case "reviews":
		return a.runScopedProgress(ctx, args[1:], true)
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
		return commandError("usage", "unknown command "+args[0], "使用 edu-agent version, pair, device, config, space, knowledge, goal, study, learn, assessment, route, progress, evidence, reviews, logout 或 clear", ExitInput)
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
