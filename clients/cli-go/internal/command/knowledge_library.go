package command

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"strconv"
	"strings"
)

func (a *App) runKnowledgeLibrary(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "help" {
		_, err := fmt.Fprintln(a.Out, "knowledge library list [--shared]\nknowledge library create --name 名称 --source 来源\nknowledge library share --id 集合ID --version 版本\nknowledge library link|unlink --id 集合ID\nknowledge library tree|preview --id 版本ID --collection 集合ID\nknowledge library tree|preview --scope --id 冻结范围ID\nknowledge library freeze --entries '[{\"collection_id\":\"...\",\"revision_id\":\"...\",\"document_id\":\"可省略\",\"node_id\":\"可省略\"}]'\nknowledge library browse\n导入使用 knowledge import --collection 集合ID 路径。所有命令使用 --space 指定学习区。解除引用不删除正文或历史范围。")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(a.Out, "knowledge library scope --id 范围ID（查看冻结版本及更新提示）\nknowledge library retrieve --id 范围ID 查询词")
		return err
	}
	set := newFlagSet("knowledge library")
	var flags onlineFlags
	addOnlineFlags(set, &flags)
	id := set.String("id", "", "资料/版本 ID")
	collection := set.String("collection", "", "集合 ID")
	name := set.String("name", "", "显示名称")
	source := set.String("source", "", "来源")
	shared := set.Bool("shared", false, "包括可共享集合")
	version := set.Int64("version", 0, "预期版本")
	entries := set.String("entries", "", "冻结条目 JSON")
	scope := set.Bool("scope", false, "读取冻结范围")
	if err := set.Parse(args[1:]); err != nil {
		return err
	}
	bound, timeout, err := a.loadBinding(flags.overrides())
	if err != nil {
		return err
	}
	client, ok := a.scopedClient(bound.Config.ServerURL, bound.Token, timeout).(*api.Client)
	if !ok {
		return commandError("unsupported", "客户端不支持资料集合", "更新客户端", ExitUnavailable)
	}
	if *collection != "" {
		client = client.WithCollection(*collection)
	}
	var result any
	switch args[0] {
	case "list":
		result, err = client.KnowledgeCollections(ctx, *shared)
	case "browse":
		return a.browseKnowledgeLibrary(ctx, client)
	case "create", "share", "link", "unlink":
		if args[0] == "create" && *id == "" {
			*id, err = a.operationID()
			if err != nil {
				return err
			}
		}
		result, err = client.ChangeKnowledgeCollection(ctx, api.KnowledgeCollectionCommand{ID: *id, Action: args[0], Name: *name, Source: *source, Shared: args[0] == "share" || *shared, ExpectedVersion: *version})
	case "freeze":
		var selected []api.KnowledgeScopeEntry
		if err = json.Unmarshal([]byte(*entries), &selected); err != nil {
			return commandError("usage", "范围 JSON 无效", "使用 knowledge library help", ExitInput)
		}
		if *id == "" {
			*id, err = a.operationID()
			if err != nil {
				return err
			}
		}
		result, err = client.FreezeKnowledgeScope(ctx, api.KnowledgeScopeSnapshot{ID: *id, Entries: selected})
	case "retrieve":
		result, err = client.RetrieveKnowledge(ctx, api.KnowledgeRetrievalRequest{Query: strings.Join(set.Args(), " "), ScopeSnapshotID: *id})
	case "scope":
		result, err = client.KnowledgeLibraryView(ctx, *id, "", true)
	case "tree", "preview":
		kind := "tree"
		if args[0] == "preview" {
			kind = "export"
		}
		result, err = client.KnowledgeLibraryView(ctx, *id, kind, *scope)
	default:
		return commandError("usage", "未知资料操作", "使用 knowledge library help", ExitInput)
	}
	if err != nil {
		return mapAPIError(err)
	}
	return json.NewEncoder(a.Out).Encode(result)
}

func (a *App) browseKnowledgeLibrary(ctx context.Context, client *api.Client) error {
	if !a.interactiveTerminalAvailable() {
		return commandError("not_a_terminal", "资料浏览需要终端", "使用 knowledge library list", ExitInput)
	}
	for {
		items, err := client.KnowledgeCollections(ctx, false)
		if err != nil {
			return mapAPIError(err)
		}
		_, _ = fmt.Fprintln(a.Out, "本区资料集合（浏览最新版本；冻结范围不会自动更新）")
		if len(items) == 0 {
			_, _ = fmt.Fprintln(a.Out, "本区尚未关联资料。使用 library create 或 link 添加。")
		}
		for i, c := range items {
			_, _ = fmt.Fprintf(a.Out, "%d. %s / %s [%s]\n", i+1, safeText(c.Name), safeText(c.Source), c.ID)
		}
		input, err := a.Terminal.ReadLine("序号：预览；q：返回 > ")
		if err != nil {
			return err
		}
		if strings.TrimSpace(input) == "q" {
			return nil
		}
		i, err := strconv.Atoi(input)
		if err != nil || i < 1 || i > len(items) {
			continue
		}
		c := items[i-1]
		if c.HeadRevisionID == nil {
			_, _ = fmt.Fprintln(a.Out, "尚未导入正文。")
			continue
		}
		view, err := client.WithCollection(c.ID).KnowledgeLibraryView(ctx, *c.HeadRevisionID, "export", false)
		if err != nil {
			return mapAPIError(err)
		}
		if data, ok := view.(map[string]any); ok {
			if documents, ok := data["documents"].([]any); ok {
				for _, item := range documents {
					if doc, ok := item.(map[string]any); ok {
						_, _ = fmt.Fprintln(a.Out, safeText(fmt.Sprint(doc["path"])))
						if markdown, ok := doc["markdown"].(string); ok {
							for _, line := range strings.Split(markdown, "\n") {
								_, _ = fmt.Fprintln(a.Out, safeText(line))
							}
						}
					}
				}
			}
		}
		action, err := a.Terminal.ReadLine("t：节点树；f：冻结范围；s：共享；u：解除本区引用；回车：返回 > ")
		if err != nil {
			return err
		}
		switch action {
		case "t":
			view, err = client.WithCollection(c.ID).KnowledgeLibraryView(ctx, *c.HeadRevisionID, "tree", false)
		case "s", "u":
			kind := "share"
			if action == "u" {
				kind = "unlink"
			}
			view, err = client.ChangeKnowledgeCollection(ctx, api.KnowledgeCollectionCommand{ID: c.ID, Action: kind, Shared: true, ExpectedVersion: c.Version})
		case "f":
			entry, selectErr := a.selectKnowledgeScope(ctx, client, c)
			if selectErr != nil {
				return selectErr
			}
			id, uuidErr := a.operationID()
			if uuidErr != nil {
				return uuidErr
			}
			view, err = client.FreezeKnowledgeScope(ctx, api.KnowledgeScopeSnapshot{ID: id, Entries: []api.KnowledgeScopeEntry{entry}})
		default:
			continue
		}
		if err != nil {
			return mapAPIError(err)
		}
		raw, _ := json.MarshalIndent(view, "", "  ")
		for _, line := range strings.Split(string(raw), "\n") {
			_, _ = fmt.Fprintln(a.Out, safeText(line))
		}
	}
}

func (a *App) selectKnowledgeScope(ctx context.Context, client *api.Client, c api.KnowledgeCollection) (api.KnowledgeScopeEntry, error) {
	entry := api.KnowledgeScopeEntry{CollectionID: c.ID, RevisionID: *c.HeadRevisionID}
	view, err := client.WithCollection(c.ID).KnowledgeLibraryView(ctx, *c.HeadRevisionID, "tree", false)
	if err != nil {
		return entry, mapAPIError(err)
	}
	var tree struct {
		Revision struct {
			Documents []struct {
				Path     string `json:"path"`
				Document struct {
					ID    string `json:"document_id"`
					Nodes []struct {
						ID    string `json:"node_id"`
						Title string `json:"title"`
						Level int    `json:"heading_level"`
					} `json:"nodes"`
				} `json:"document"`
			} `json:"documents"`
		} `json:"revision"`
	}
	raw, _ := json.Marshal(view)
	if err = json.Unmarshal(raw, &tree); err != nil {
		return entry, err
	}
	for i, doc := range tree.Revision.Documents {
		_, _ = fmt.Fprintf(a.Out, "%d. %s\n", i+1, safeText(doc.Path))
	}
	input, err := a.Terminal.ReadLine("文档序号（留空选择整个集合） > ")
	if err != nil {
		return entry, err
	}
	if strings.TrimSpace(input) == "" {
		return entry, nil
	}
	index, err := strconv.Atoi(input)
	if err != nil || index < 1 || index > len(tree.Revision.Documents) {
		return entry, commandError("usage", "文档序号无效", "重新选择资料范围", ExitInput)
	}
	doc := tree.Revision.Documents[index-1]
	entry.DocumentID = doc.Document.ID
	ids := []string{}
	for _, node := range doc.Document.Nodes {
		if node.Level == 0 {
			continue
		}
		ids = append(ids, node.ID)
		_, _ = fmt.Fprintf(a.Out, "%d. %s\n", len(ids), safeText(node.Title))
	}
	input, err = a.Terminal.ReadLine("章节序号（留空选择整篇文档） > ")
	if err != nil {
		return entry, err
	}
	if strings.TrimSpace(input) == "" {
		return entry, nil
	}
	index, err = strconv.Atoi(input)
	if err != nil || index < 1 || index > len(ids) {
		return entry, commandError("usage", "章节序号无效", "重新选择资料范围", ExitInput)
	}
	entry.NodeID = ids[index-1]
	return entry, nil
}
