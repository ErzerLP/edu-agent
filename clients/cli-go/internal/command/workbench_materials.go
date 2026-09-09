package command

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/api"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/workbench"
)

func decodeLibrary(value any, target any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, target)
}

func (a *App) workbenchMaterials(ctx context.Context, client APIClient, req workbench.Request, p workbench.Page) (workbench.Page, error) {
	c, ok := client.(*api.Client)
	if !ok {
		return p, fmt.Errorf("客户端不支持资料集合")
	}
	shared := req.Resource == "shared" || req.Page == "shared"
	collections, err := c.KnowledgeCollections(ctx, shared)
	if err != nil {
		return p, mapAPIError(err)
	}
	if req.Page == "materials" {
		p.Title, p.Searchable = "区内 · 资料集合", true
		if shared {
			p.Title = "全局 · 可共享资料集合（尚未关联本区）"
		} else {
			p.Entries = append(p.Entries, workbench.Entry{ID: "page:import", Label: "打开完整导入向导"}, workbench.Entry{ID: "page:materials/shared", Label: "浏览全局共享集合"})
			p.Actions = append(p.Actions, workbench.Action{ID: "create", Label: "创建资料集合", Input: "集合名称"})
		}
		if req.Action == "create" {
			created, err := c.ChangeKnowledgeCollection(ctx, api.KnowledgeCollectionCommand{ID: req.Entity, Action: "create", Name: strings.TrimSpace(req.Text), Source: "manual"})
			if err != nil {
				return p, mapAPIError(err)
			}
			p.Redirect = "collection/" + created.ID
			return p, nil
		}
		for _, item := range collections {
			if !strings.Contains(strings.ToLower(item.Name+" "+item.Source), strings.ToLower(req.Search)) {
				continue
			}
			prefix := "collection/"
			if shared {
				prefix = "shared/"
			}
			p.Entries = append(p.Entries, workbench.Entry{ID: "page:" + prefix + item.ID, Label: item.Name + " · " + item.Source})
			p.Content += item.Name + " · " + item.Source + "\n"
		}
		if p.Content == "" {
			p.Content = "没有匹配的资料集合。"
		}
		return p, nil
	}
	parts := strings.Split(req.Resource, "/")
	var collection *api.KnowledgeCollection
	for i := range collections {
		if collections[i].ID == parts[0] {
			collection = &collections[i]
			break
		}
	}
	if collection == nil {
		return p, fmt.Errorf("集合已解除关联或不可访问，请刷新资料列表")
	}
	p.Title, p.Version = "资料 · "+collection.Name, collection.Version
	if req.Action == "share" || req.Action == "unlink" || req.Action == "link" {
		if req.Version != collection.Version {
			return p, fmt.Errorf("集合版本已变化，请刷新确认")
		}
		_, err := c.ChangeKnowledgeCollection(ctx, api.KnowledgeCollectionCommand{ID: collection.ID, Action: req.Action, Shared: true, ExpectedVersion: collection.Version})
		if err != nil {
			return p, mapAPIError(err)
		}
		p.Redirect = "materials"
		return p, nil
	}
	if shared {
		p.Content = "来源：全局共享目录；关联后才纳入本区资料。"
		p.Actions = []workbench.Action{{ID: "link", Label: "关联到当前学习区", Confirmation: "关联此共享集合到当前区？"}}
		return p, nil
	}
	p.Entries = append(p.Entries, workbench.Entry{ID: "page:import/" + collection.ID, Label: "导入到此集合"})
	p.Actions = append(p.Actions, workbench.Action{ID: "share", Label: "允许其他区共享", Confirmation: "允许其他学习区显式关联此集合？"}, workbench.Action{ID: "unlink", Label: "解除本区引用", Confirmation: "解除本区引用？不会删除其他区的引用和历史范围。"})
	if collection.HeadRevisionID == nil {
		p.Content = "集合尚未导入正文。"
		return p, nil
	}
	p.Basis = *collection.HeadRevisionID
	if req.Action != "" && req.Basis != p.Basis {
		return p, fmt.Errorf("资料版本已变化，请刷新后重新选择范围")
	}
	scoped := c.WithCollection(collection.ID)
	value, err := scoped.KnowledgeLibraryView(ctx, p.Basis, "tree", false)
	if err != nil {
		return p, mapAPIError(err)
	}
	var tree struct {
		Revision api.KnowledgeRevision `json:"revision"`
	}
	if err := decodeLibrary(value, &tree); err != nil {
		return p, err
	}
	entry := api.KnowledgeScopeEntry{CollectionID: collection.ID, RevisionID: p.Basis}
	if len(parts) > 1 {
		entry.DocumentID = parts[1]
	}
	if len(parts) > 2 {
		entry.NodeID = parts[2]
	}
	if req.Action == "freeze" || req.Action == "add-scope" {
		entries := []api.KnowledgeScopeEntry{}
		if req.Action == "add-scope" && req.Scope != "" {
			value, err := c.KnowledgeLibraryView(ctx, req.Scope, "", true)
			if err != nil {
				return p, mapAPIError(err)
			}
			var previous api.KnowledgeScopeSnapshot
			if err := decodeLibrary(value, &previous); err != nil {
				return p, err
			}
			entries = previous.Entries
		}
		found := false
		for _, old := range entries {
			if old == entry {
				found = true
			}
		}
		if !found {
			entries = append(entries, entry)
		}
		scope, err := c.FreezeKnowledgeScope(ctx, api.KnowledgeScopeSnapshot{ID: req.Entity, Entries: entries})
		if err != nil {
			return p, mapAPIError(err)
		}
		p.Scope = scope.ID
		p.Content = "已冻结选择；目标页可显式绑定此范围。\n"
	}
	p.Actions = append(p.Actions, workbench.Action{ID: "freeze", Label: "以当前集合/文档/章节建立资料范围"}, workbench.Action{ID: "add-scope", Label: "追加到已选择的资料范围"})
	p.Content += "明确版本：" + p.Basis + "\n"
	if len(tree.Revision.Documents) == 0 {
		p.Content += "此版本没有文档。\n"
	}
	for _, doc := range tree.Revision.Documents {
		if entry.DocumentID != "" && entry.DocumentID != doc.Document.DocumentID {
			continue
		}
		p.Content += "文档：" + doc.Path + "\n"
		if entry.DocumentID == "" {
			p.Entries = append(p.Entries, workbench.Entry{ID: "page:collection/" + collection.ID + "/" + doc.Document.DocumentID, Label: doc.Path})
		}
		for _, node := range doc.Document.Nodes {
			if node.HeadingLevel == 0 {
				continue
			}
			if node.NodeID == entry.NodeID {
				p.Content += "当前选择章节：" + node.Title + "\n"
			}
			p.Content += strings.Repeat("  ", min(node.HeadingLevel, 6)) + node.Title + "\n"
			if entry.DocumentID != "" && entry.NodeID == "" {
				p.Entries = append(p.Entries, workbench.Entry{ID: "page:collection/" + collection.ID + "/" + doc.Document.DocumentID + "/" + node.NodeID, Label: node.Title})
			}
		}
	}
	if entry.DocumentID != "" {
		value, err := scoped.KnowledgeLibraryView(ctx, p.Basis, "export", false)
		if err != nil {
			return p, mapAPIError(err)
		}
		var exported struct {
			Documents []struct {
				Path     string `json:"path"`
				Markdown string `json:"markdown"`
			} `json:"documents"`
		}
		if err := decodeLibrary(value, &exported); err != nil {
			return p, err
		}
		for _, doc := range tree.Revision.Documents {
			if doc.Document.DocumentID == entry.DocumentID {
				for _, body := range exported.Documents {
					if body.Path == doc.Path {
						p.Content += "\n文档全文（范围选择以上方标识为准）：\n" + body.Markdown
					}
				}
			}
		}
	}
	return p, nil
}
