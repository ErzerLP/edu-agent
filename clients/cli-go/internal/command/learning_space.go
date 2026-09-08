package command

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
)

type spaceClient interface {
	LearningSpacesCapabilities(context.Context) (api.LearningSpaceCapabilities, error)
	LearningSpaces(context.Context, string, string, string, int) (api.LearningSpacePage, error)
	LearningSpace(context.Context, string) (api.LearningSpace, error)
	MutateLearningSpace(context.Context, string, api.LearningSpaceCommand) (api.LearningSpace, error)
}

var spaceIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func (a *App) scopedClient(server, token string, timeout time.Duration) APIClient {
	client := a.NewClient(server, token, timeout)
	if real, ok := client.(*api.Client); ok && a.learningSpace != "" {
		return real.WithLearningSpace(a.learningSpace)
	}
	return client
}
func (a *App) parseSpaceFlag(args []string) ([]string, func(), error) {
	old := a.learningSpace
	oldName := a.learningSpaceName
	selected := old
	restore := func() {}
	filtered := []string{}
	seen := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			filtered = append(filtered, args[i:]...)
			break
		}
		if arg == "--space" || strings.HasPrefix(arg, "--space=") {
			value := strings.TrimPrefix(arg, "--space=")
			if arg == "--space" {
				i++
				if i == len(args) {
					return nil, restore, commandError("invalid_learning_space", "--space requires an ID", "use space list", ExitInput)
				}
				value = args[i]
			}
			if seen || !spaceIDPattern.MatchString(value) || value == "00000000-0000-0000-0000-000000000000" {
				return nil, func() {}, commandError("invalid_learning_space", "--space requires one canonical UUID", "use space list", ExitInput)
			}
			seen = true
			selected = value
		} else {
			filtered = append(filtered, arg)
		}
	}
	if selected != "" && selected != api.DefaultLearningSpaceID && len(filtered) > 0 && (filtered[0] == "offline" || filtered[0] == "agent") {
		return nil, func() {}, commandError("learning_space_module_unavailable", "Agent and offline workflows are available only in the default learning space", "select the default space", ExitUnavailable)
	}
	if seen {
		a.learningSpace = selected
		restore = func() { a.learningSpace = old; a.learningSpaceName = oldName }
	}
	return filtered, restore, nil
}
func (a *App) runSpace(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		_, err := fmt.Fprintln(a.Out, "space list [--search TEXT] [--status active|archived] [--limit 1..100] [--cursor TOKEN]\nspace show|select --id UUID\nspace create --name NAME [--description TEXT] [--operation-id UUID]\nspace edit|archive|restore --id UUID [--name NAME] [--description TEXT] [--expected-version N] [--operation-id UUID]\nspace browse (interactive list, details, selection and management)\nAll commands accept --space UUID for this invocation. Selection is process-local; shell scripts must pass --space on each invocation.\nKnowledge, learning, tutoring and memory are available only in the default space; Agent/offline require the default space. Names never determine ownership.")
		return err
	}
	action := args[0]
	set := newFlagSet("space " + action)
	var flags onlineFlags
	var search, status, cursor, spaceID, name, description, operation string
	var limit int
	var version int64
	addOnlineFlags(set, &flags)
	set.StringVar(&search, "search", "", "literal name/description search")
	set.StringVar(&status, "status", "", "active or archived")
	set.StringVar(&cursor, "cursor", "", "page cursor")
	set.IntVar(&limit, "limit", 50, "page size (1..100)")
	set.StringVar(&spaceID, "id", "", "stable space UUID")
	set.StringVar(&name, "name", "", "space name")
	set.StringVar(&description, "description", "", "space description")
	set.StringVar(&operation, "operation-id", "", "retry identity")
	set.Int64Var(&version, "expected-version", 0, "expected metadata version")
	if err := set.Parse(args[1:]); err != nil || len(set.Args()) != 0 {
		return commandError("usage", "invalid space arguments", "use space help", ExitInput)
	}
	switch action {
	case "list", "show", "select", "create", "edit", "archive", "restore", "browse":
	default:
		return commandError("usage", "unknown space command", "use space help", ExitInput)
	}
	online, err := a.openOnline(flags)
	if err != nil {
		return err
	}
	client, ok := online.client.(spaceClient)
	if !ok {
		return commandError("learning_spaces_unsupported", "client does not support learning spaces", "upgrade client and server", ExitUnavailable)
	}
	if action == "browse" {
		return a.browseSpaces(ctx, client)
	}
	if action == "list" {
		page, err := client.LearningSpaces(ctx, search, status, cursor, limit)
		if err != nil {
			return err
		}
		return json.NewEncoder(a.Out).Encode(page)
	}
	var item api.LearningSpace
	if action != "create" {
		item, err = client.LearningSpace(ctx, spaceID)
		if err != nil {
			return err
		}
	}
	if action == "select" {
		a.learningSpace = item.ID
		a.learningSpaceName = item.Name
		_, err = fmt.Fprintf(a.Out, "Selected %s (%s), status=%s; selection lasts for this client process.\n", safeText(item.Name), item.ID, item.Status)
		if item.ID != api.DefaultLearningSpaceID {
			_, _ = fmt.Fprintln(a.Out, "Knowledge, goals, tutoring, memory, Agent and offline workflows are unavailable in this space.")
		}
		return err
	}
	if action == "show" {
		return json.NewEncoder(a.Out).Encode(item)
	}
	if operation == "" {
		operation, err = a.operationID()
		if err != nil {
			return err
		}
	}
	if action == "create" {
		item.Name = name
		item.Description = description
		item.Status = "active"
	}
	if action != "create" {
		if version == 0 {
			version = item.Version
		}
		set.Visit(func(f *flag.Flag) {
			if f.Name == "name" {
				item.Name = name
			}
			if f.Name == "description" {
				item.Description = description
			}
		})
	}
	if action == "archive" {
		item.Status = "archived"
	}
	if action == "restore" {
		item.Status = "active"
	}
	result, err := client.MutateLearningSpace(ctx, spaceID, api.LearningSpaceCommand{OperationID: operation, ExpectedVersion: version, Name: item.Name, Description: item.Description, Status: item.Status})
	if err != nil {
		return err
	}
	return json.NewEncoder(a.Out).Encode(result)
}
func (a *App) browseSpaces(ctx context.Context, client spaceClient) error {
	if !a.interactiveTerminalAvailable() {
		return commandError("not_a_terminal", "space browse requires a terminal", "use space list", ExitInput)
	}
	search, cursor := "", ""
	for {
		page, err := client.LearningSpaces(ctx, search, "", cursor, 20)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintln(a.Out, "学习区 / Learning spaces (selection belongs to this client)")
		if len(page.Items) == 0 {
			_, _ = fmt.Fprintln(a.Out, "No learning spaces match this search.")
		}
		for i, item := range page.Items {
			marker := " "
			if item.ID == a.learningSpace || (a.learningSpace == "" && item.ID == api.DefaultLearningSpaceID) {
				marker = "*"
			}
			_, _ = fmt.Fprintf(a.Out, "%s %d. %s [%s]\n", marker, i+1, safeText(item.Name), item.Status)
		}
		input, err := a.Terminal.ReadLine("Number: details; /text: search; n: next; c: create; q: back > ")
		if err != nil {
			return err
		}
		input = strings.TrimSpace(input)
		if input == "q" {
			return nil
		}
		if strings.HasPrefix(input, "/") {
			search = strings.TrimPrefix(input, "/")
			cursor = ""
			continue
		}
		if input == "n" {
			cursor = page.NextCursor
			continue
		}
		if input == "c" {
			name, err := a.Terminal.ReadLine("Name > ")
			if err != nil {
				return err
			}
			description, err := a.Terminal.ReadLine("Description > ")
			if err != nil {
				return err
			}
			op, err := a.operationID()
			if err != nil {
				return err
			}
			if _, err = client.MutateLearningSpace(ctx, "", api.LearningSpaceCommand{OperationID: op, Name: name, Description: description, Status: "active"}); err != nil {
				return err
			}
			cursor = ""
			continue
		}
		index, err := strconv.Atoi(input)
		if err != nil || index < 1 || index > len(page.Items) {
			continue
		}
		item, err := client.LearningSpace(ctx, page.Items[index-1].ID)
		if err != nil {
			return err
		}
		_, _ = fmt.Fprintf(a.Out, "%s\n%s\nID: %s  status: %s  version: %d\n", safeText(item.Name), safeText(item.Description), item.ID, item.Status, item.Version)
		_, _ = fmt.Fprintln(a.Out, "Business modules currently support only the default space.")
		action, err := a.Terminal.ReadLine("s: select; r: rename; a: archive; u: restore; Enter: back > ")
		if err != nil {
			return err
		}
		if action == "s" {
			a.learningSpace = item.ID
			a.learningSpaceName = item.Name
			return nil
		}
		switch action {
		case "r":
			item.Name, err = a.Terminal.ReadLine("Name > ")
			if err != nil {
				return err
			}
		case "a":
			item.Status = "archived"
		case "u":
			item.Status = "active"
		default:
			continue
		}
		op, err := a.operationID()
		if err != nil {
			return err
		}
		if _, err = client.MutateLearningSpace(ctx, item.ID, api.LearningSpaceCommand{OperationID: op, ExpectedVersion: item.Version, Name: item.Name, Description: item.Description, Status: item.Status}); err != nil {
			return err
		}
	}
}
