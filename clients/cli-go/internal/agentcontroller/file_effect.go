package agentcontroller

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/edu-agent/edu-agent/clients/cli-go/internal/agentloop"
	"github.com/edu-agent/edu-agent/clients/cli-go/internal/fileeffects"
)

func checkpointFileEffect(checkpoint agentloop.SessionCheckpoint, callID string) (fileeffects.Effect, bool) {
	decode := func(text string) (fileeffects.Effect, bool) {
		var value struct {
			Effect *fileeffects.Effect `json:"file_effect"`
		}
		if json.Unmarshal([]byte(text), &value) == nil && value.Effect != nil {
			return *value.Effect, true
		}
		return fileeffects.Effect{}, false
	}
	// Recall is the bounded full result, never the budget-shortened model text.
	for _, source := range checkpoint.Context.Sources {
		if source.ModelMessage.ToolCallID == callID {
			if e, ok := decode(source.RecallText); ok {
				return e, true
			}
		}
	}
	for _, message := range checkpoint.Messages {
		if message.Role == "tool" && message.ToolCallID == callID {
			if e, ok := decode(message.Content); ok {
				return e, true
			}
		}
	}
	for _, entry := range checkpoint.ToolHistory {
		if entry.Key == callID {
			return decode(entry.Value)
		}
	}
	return fileeffects.Effect{}, false
}
func fileEffectRecoveryLabel(e fileeffects.Effect) string {
	switch e.Operation {
	case "archive":
		return "归档结果未知：检查源 " + e.Source.Path + " 与归档目标 " + e.Target.Path + "；不会自动重试或清理"
	case "move":
		return "移动结果未知：源 " + e.Source.Path + " → 目标 " + e.Target.Path + "；请核查两端，不会自动重试、恢复重放或删除回滚"
	case "copy":
		return "复制结果未知：源 " + e.Source.Path + " → 目标 " + e.Target.Path + "；本操作未修改源；请核查目标及临时项，不会自动重试或恢复重放"
	case "mkdir":
		known := "无已确认创建项（不代表未发生）"
		if paths := e.CreatedPaths(); len(paths) > 0 {
			known = strings.Join(paths, "、")
		}
		return fmt.Sprintf("目录创建结果未知：目标 %s；父锚点 %s；计划 %d 层，已知创建 %d 层：%s。其余计划路径可能已创建；不会自动重试或删除回滚。", e.Target.Path, e.Directories.Anchor, e.Directories.Count, e.Directories.Created, known)
	default:
		return "文件发布结果未知：" + e.Target.Path + "；必须重新核查、预览并授权，不会自动重试"
	}
}
