package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	operations "github.com/edu-agent/edu-agent/contracttests/operations"
)

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }
func (values *stringList) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("value must not be empty")
	}
	*values = append(*values, value)
	return nil
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "verify":
			return runVerify(args[1:])
		case "verify-go-events":
			return runVerifyGoEvents(args[1:])
		case "redact-stream":
			return runRedactStream(args[1:])
		case "run":
			args = args[1:]
		}
	}
	flags := flag.NewFlagSet("operations-candidate", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var lanes stringList
	var evidenceDir, layout, root, lockFile, attestationKeyFile string
	var resume, dryRun, list bool
	flags.Var(&lanes, "lane", "run one required lane; repeat to select multiple lanes")
	flags.StringVar(&evidenceDir, "evidence-dir", "", "directory for candidate evidence")
	flags.StringVar(&layout, "nocturne-oci-layout", "", "absolute path to a verified Nocturne OCI layout")
	flags.StringVar(&root, "root", "", "repository root (normally auto-detected)")
	flags.StringVar(&lockFile, "lock-file", "", "host-wide qualification lock file")
	flags.StringVar(&attestationKeyFile, "attestation-key-file", "", "HMAC key file outside the repository and evidence directory")
	flags.BoolVar(&resume, "resume", false, "reuse only matching verified passed evidence")
	flags.BoolVar(&dryRun, "dry-run", false, "run preflight without executing gates")
	flags.BoolVar(&list, "list", false, "list required lanes and exit")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "usage: operations-candidate [run] [options]")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintf(os.Stderr, "unexpected argument: %s\n", flags.Arg(0))
		return 2
	}
	if list {
		for _, lane := range operations.LaneCatalog() {
			fmt.Printf("%s\t%s\t%s\n", lane.Name, lane.Scenario, lane.Description)
		}
		return 0
	}
	resolvedRoot, err := resolveRoot(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	index, indexPath, err := operations.RunCandidate(operations.RunOptions{
		Root:               resolvedRoot,
		EvidenceDir:        evidenceDir,
		SelectedLanes:      lanes,
		Resume:             resume,
		DryRun:             dryRun,
		NocturneOCILayout:  layout,
		CandidateID:        os.Getenv("CANDIDATE_ID"),
		HostLockPath:       lockFile,
		AttestationKeyFile: attestationKeyFile,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "operations candidate:", err)
		return 2
	}
	printIndex(index, indexPath)
	return statusExit(index.Overall)
}

func runVerify(args []string) int {
	flags := flag.NewFlagSet("operations-candidate verify", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var evidenceDir, layout, root, lockFile, attestationKeyFile string
	flags.StringVar(&evidenceDir, "evidence-dir", "", "directory containing candidate evidence")
	flags.StringVar(&layout, "nocturne-oci-layout", "", "absolute path to the same verified Nocturne OCI layout")
	flags.StringVar(&root, "root", "", "repository root (normally auto-detected)")
	flags.StringVar(&lockFile, "lock-file", "", "host-wide qualification lock file used by the original run")
	flags.StringVar(&attestationKeyFile, "attestation-key-file", "", "existing HMAC key file outside the repository and evidence directory")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		return 2
	}
	resolvedRoot, err := resolveRoot(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	index, err := operations.VerifyCandidate(operations.VerifyOptions{
		Root:               resolvedRoot,
		EvidenceDir:        evidenceDir,
		NocturneOCILayout:  layout,
		CandidateID:        os.Getenv("CANDIDATE_ID"),
		HostLockPath:       lockFile,
		AttestationKeyFile: attestationKeyFile,
	})
	if err != nil {
		if index.SchemaVersion != "" {
			printIndex(index, filepath.Join(evidenceDir, index.CandidateFingerprint, "candidate-index.json"))
		}
		fmt.Fprintln(os.Stderr, "operations evidence verification:", err)
		return statusExit(index.Overall)
	}
	printIndex(index, filepath.Join(evidenceDir, index.CandidateFingerprint, "candidate-index.json"))
	return 0
}

func runVerifyGoEvents(args []string) int {
	flags := flag.NewFlagSet("operations-candidate verify-go-events", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	var logPath, selectedPath string
	flags.StringVar(&logPath, "log", "", "redacted go test -json log")
	flags.StringVar(&selectedPath, "selected-file", "", "package<TAB>test selection file")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if logPath == "" || selectedPath == "" || flags.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "verify-go-events requires --log and --selected-file")
		return 2
	}
	selected, err := operations.ReadSelectedTests(selectedPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	logFile, err := os.Open(logPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	summary, parseErr := operations.ParseExpectedGoTestEvents(logFile, selected)
	closeErr := logFile.Close()
	if parseErr != nil {
		fmt.Fprintln(os.Stderr, parseErr)
		return 1
	}
	if closeErr != nil {
		fmt.Fprintln(os.Stderr, closeErr)
		return 1
	}
	fmt.Printf("GO_EVENT_SUMMARY selected=%d executed=%d passed=%d failed=%d skipped=%d\n", len(selected), len(summary.Executed), len(summary.Passed), len(summary.Failed), len(summary.Skipped))
	return 0
}

func runRedactStream(args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(os.Stderr, "redact-stream accepts no arguments")
		return 2
	}
	unsafe, err := operations.RedactStream(os.Stdin, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if unsafe {
		fmt.Fprintln(os.Stderr, "unsafe log content was redacted")
		return 3
	}
	return 0
}

func resolveRoot(explicit string) (string, error) {
	if explicit != "" {
		return filepath.Abs(explicit)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return operations.FindRepositoryRoot(cwd)
}

func printIndex(index operations.CandidateIndex, path string) {
	fmt.Printf("candidate_fingerprint=%s\n", index.CandidateFingerprint)
	fmt.Printf("qualification_key=%s\n", index.QualificationKey)
	for _, lane := range index.Lanes {
		fmt.Printf("lane=%s status=%s evidence_key=%s reason=%s\n", lane.Lane, lane.Status, lane.EvidenceKey, lane.Reason)
	}
	fmt.Printf("overall=%s\n", index.Overall)
	if path != "" {
		fmt.Printf("candidate_index=%s\n", path)
	}
}

func statusExit(status operations.Status) int {
	switch status {
	case operations.StatusPassed:
		return 0
	case operations.StatusBlocked:
		return 2
	default:
		return 1
	}
}
