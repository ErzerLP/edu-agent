//go:build !web_release

package webassets

import "embed"

// 开发也嵌入真实构建产物；未构建时仅允许关闭 Web 的 Go 开发检查。
//
//go:embed dist
var files embed.FS
