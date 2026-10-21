package deploy_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const repoRoot = "../../.."
const sampleID = "00000000-0000-4000-8000-000000000001"

func samplePath(path string) string { return strings.ReplaceAll(path, "{id}", sampleID) }

func requestPath(t *testing.T, path string) string {
	t.Helper()
	u, err := url.ParseRequestURI(samplePath(path))
	if err != nil {
		t.Fatal(err)
	}
	return u.Path
}

// 新的 CLI 路径字面量必须在清单中有样例；请求头和错误处理中的路径前缀也一起核对。
// 这不是 Go 数据流分析：已有前缀下新增的动态后缀仍须维护 cliRoutes 中的分支。
func TestCLIRouteInventory(t *testing.T) {
	cliRoot := filepath.Join(repoRoot, "clients/cli-go")
	fset := token.NewFileSet()
	err := filepath.WalkDir(cliRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil || !strings.HasPrefix(value, "/v1/") && value != "/readyz" {
				return true
			}
			prefix, _, _ := strings.Cut(value, "?")
			prefix = strings.ReplaceAll(prefix, "%s", sampleID)
			for _, route := range cliRoutes {
				if strings.HasPrefix(requestPath(t, route.path), prefix) {
					return true
				}
			}
			t.Errorf("%s: CLI 路径 %q 没有清单样例，请维护 cliRoutes", fset.Position(literal.Pos()), value)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	registered := serverRoutes(t)
	for _, route := range cliRoutes {
		if _, err := os.Stat(filepath.Join(cliRoot, "internal", route.source)); err != nil {
			t.Errorf("清单来源 %s: %v", route.source, err)
		}
		found := false
		for _, server := range registered {
			if server.method == route.method && server.path.MatchString(requestPath(t, route.path)) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s: 服务端未注册 %s %s", route.source, route.method, route.path)
		}
	}
	t.Logf("核对 %d 条 CLI 方法/路径样例及服务端路由", len(cliRoutes))
}

type serverRoute struct {
	method string
	path   *regexp.Regexp
}

// 读取真实路由注册，包含 planning.go 的 base+"/{planID}"；不依赖服务初始化或数据库。
func serverRoutes(t *testing.T) []serverRoute {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(repoRoot, "server/internal/transport/httpapi/*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var routes []serverRoute
	parameter := regexp.MustCompile(`\{([^{}]+)\}`)
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, declaration := range file.Decls {
			fn, ok := declaration.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			constants := map[string]string{}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				if assignment, ok := node.(*ast.AssignStmt); ok {
					for i, expr := range assignment.Rhs {
						if i < len(assignment.Lhs) {
							if name, ok := assignment.Lhs[i].(*ast.Ident); ok {
								constants[name.Name] = stringExpr(expr, constants)
							}
						}
					}
				}
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) < 2 {
					return true
				}
				selector, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				method, index := strings.ToUpper(selector.Sel.Name), 0
				switch method {
				case "GET", "POST", "PUT", "DELETE", "PATCH":
				case "METHOD":
					method, index = stringExpr(call.Args[0], constants), 1
				default:
					return true
				}
				path := stringExpr(call.Args[index], constants)
				if !strings.HasPrefix(path, "/") {
					return true
				}
				pattern := parameter.ReplaceAllStringFunc(path, func(value string) string {
					if _, pattern, ok := strings.Cut(value[1:len(value)-1], ":"); ok {
						return pattern
					}
					return `[^/]+`
				})
				routes = append(routes, serverRoute{method, regexp.MustCompile("^" + pattern + "$")})
				return true
			})
		}
	}
	return routes
}

func stringExpr(expr ast.Expr, constants map[string]string) string {
	switch value := expr.(type) {
	case *ast.BasicLit:
		if value.Kind == token.STRING {
			text, _ := strconv.Unquote(value.Value)
			return text
		}
	case *ast.Ident:
		return constants[value.Name]
	case *ast.BinaryExpr:
		if value.Op == token.ADD {
			left, right := stringExpr(value.X, constants), stringExpr(value.Y, constants)
			if left != "" && right != "" {
				return left + right
			}
		}
	}
	return ""
}

type nginxLocation struct {
	modifier, path, body string
	pattern              *regexp.Regexp
}

func readNginx(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, "deploy/web/nginx.conf"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// 当前配置只使用扁平 location；遇到新增的嵌套/未知语法要失败，避免漏读规则。
func nginxLocations(t *testing.T, config string) []nginxLocation {
	t.Helper()
	declaration := regexp.MustCompile(`(?m)^\s*location\s+(?:(=|~|\^~)\s+)?("[^"]+"|\S+)\s*\{([^{}]*)\}`)
	var locations []nginxLocation
	for _, match := range declaration.FindAllStringSubmatch(config, -1) {
		location := nginxLocation{modifier: match[1], path: strings.Trim(match[2], `"`), body: match[3]}
		if location.modifier == "~" {
			var err error
			location.pattern, err = regexp.Compile(location.path)
			if err != nil {
				t.Fatalf("nginx 正则 %q 不受静态检查支持: %v", location.path, err)
			}
		}
		locations = append(locations, location)
	}
	if len(locations) == 0 || len(locations) != len(regexp.MustCompile(`(?m)^\s*location\s`).FindAllStringIndex(config, -1)) {
		t.Fatal("nginx location 语法变更，请更新静态解析器")
	}
	return locations
}

// nginx 优先选择 =，再取最长前缀；除 ^~ 外，再按配置顺序选择首个匹配正则。
func selectedLocation(locations []nginxLocation, path string) *nginxLocation {
	var prefix *nginxLocation
	for i := range locations {
		location := &locations[i]
		if location.modifier == "=" && location.path == path {
			return location
		}
		if (location.modifier == "" || location.modifier == "^~") && strings.HasPrefix(path, location.path) && (prefix == nil || len(location.path) > len(prefix.path)) {
			prefix = location
		}
	}
	if prefix != nil && prefix.modifier == "^~" {
		return prefix
	}
	for i := range locations {
		if locations[i].pattern != nil && locations[i].pattern.MatchString(path) {
			return &locations[i]
		}
	}
	return prefix
}

func TestCLINginxAllowlist(t *testing.T) {
	locations := nginxLocations(t, readNginx(t))
	for _, route := range cliRoutes {
		t.Run(route.source+"/"+route.method+route.path, func(t *testing.T) {
			location := selectedLocation(locations, requestPath(t, route.path))
			if location == nil || location.path == "/" || !strings.Contains(location.body, "proxy_pass http://127.0.0.1:8080;") || strings.Contains(location.body, "return ") {
				t.Fatalf("CLI 请求 %s %s 未转发到后端（location=%+v）", route.method, route.path, location)
			}
		})
	}
	for _, path := range rejectedPaths {
		t.Run("reject"+path, func(t *testing.T) {
			location := selectedLocation(locations, requestPath(t, path))
			if location == nil || location.path != "/" || !strings.Contains(location.body, "return 404;") {
				t.Fatalf("未知/管理路径 %s 应落入兜底 404，实际 location=%+v", path, location)
			}
		})
	}
}

func TestMentorWebSearchNginxPaths(t *testing.T) {
	locations := nginxLocations(t, readNginx(t))
	for _, path := range []string{"/v1/learning/web-documents/a", "/v1/learning/web-documents/a/pages/2", "/v1/learning/web-import-previews", "/v1/learning/web-imports", "/v1/learning/usage/tasks/a/search-calls", "/v1/learning/conversations/a/web-search"} {
		location := selectedLocation(locations, path)
		if location == nil || location.path == "/" {
			t.Errorf("网页搜索路径未转发: %s", path)
		}
	}
	for _, path := range []string{"/v1/learning/web-documents/a/delete", "/v1/learning/web-imports/force", "/v1/learning/usage/tasks/a/search-calls/force"} {
		location := selectedLocation(locations, path)
		if location == nil || location.path != "/" {
			t.Errorf("未知路径被放行: %s", path)
		}
	}
}

func TestSourceResearchNginxPaths(t *testing.T) {
	locations := nginxLocations(t, readNginx(t))
	for _, path := range []string{"/capabilities", "/jobs", "/jobs/a", "/jobs/a/events", "/jobs/a/candidates", "/jobs/a/selection", "/jobs/a/commands", "/jobs/a/rounds", "/jobs/a/reports", "/jobs/a/reports/b", "/jobs/a/import-previews", "/jobs/a/report-imports", "/jobs/a/imports"} {
		location := selectedLocation(locations, "/v1/learning/source-research"+path)
		if location == nil || location.path == "/" {
			t.Errorf("source research route not forwarded: %s", path)
		}
	}
	for _, path := range []string{"/v1/knowledge/source-discovery", "/v1/learning/source-research/jobs/a/force"} {
		location := selectedLocation(locations, path)
		if location == nil || location.path != "/" {
			t.Errorf("unexpected route forwarded: %s", path)
		}
	}
}
