//go:build web_release

package webassets

import "embed"

// 发行缺少真实入口或打包资源时，Go 编译直接失败。
//
//go:embed dist/index.html dist/assets dist/offline-sw.js
var files embed.FS
