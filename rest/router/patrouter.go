package router

import (
	"errors"
	"net/http"
	"path"
	"strings"

	"github.com/zeromicro/go-zero/core/search"
	"github.com/zeromicro/go-zero/rest/httpx"
	"github.com/zeromicro/go-zero/rest/pathvar"
)

const (
	allowHeader          = "Allow"
	allowMethodSeparator = ", "
)

var (
	// ErrInvalidMethod is an error that indicates not a valid http method.
	ErrInvalidMethod = errors.New("not a valid http method")
	// ErrInvalidPath is an error that indicates path does not start with /.
	ErrInvalidPath = errors.New("path must begin with '/'")
)

type patRouter struct {
	trees map[string]*search.Tree
	// NEW: 存储通配符路由 {method: {wildcardPath: handler}}
	wildcardRoutes map[string]map[string]http.Handler
	notFound       http.Handler
	notAllowed     http.Handler
}

// NewRouter returns a httpx.Router.
func NewRouter() httpx.Router {
	return &patRouter{
		trees: make(map[string]*search.Tree),
		// NEW: 初始化通配符路由存储
		wildcardRoutes: make(map[string]map[string]http.Handler),
	}
}

func (pr *patRouter) Handle(method, reqPath string, handler http.Handler) error {
	if !validMethod(method) {
		return ErrInvalidMethod
	}

	if len(reqPath) == 0 || reqPath[0] != '/' {
		return ErrInvalidPath
	}

	cleanPath := path.Clean(reqPath)

	// NEW: 识别并存储通配符路由（含 * 的路径）
	if strings.Contains(cleanPath, "*") {
		// 校验通配符规则：只能有一个 *，且必须在最后一个片段
		if strings.Count(cleanPath, "*") > 1 {
			return errors.New("only one wildcard (*) is allowed in path")
		}
		parts := strings.Split(cleanPath, "/")
		lastPart := parts[len(parts)-1]
		if !strings.HasPrefix(lastPart, "*") {
			return errors.New("wildcard (*) must be in the last path segment, e.g. /member/*path")
		}

		// 初始化当前方法的通配符路由表
		if _, ok := pr.wildcardRoutes[method]; !ok {
			pr.wildcardRoutes[method] = make(map[string]http.Handler)
		}
		// 存储通配符路由（去重）
		if _, exists := pr.wildcardRoutes[method][cleanPath]; exists {
			return duplicatedItem(cleanPath)
		}
		pr.wildcardRoutes[method][cleanPath] = handler
		return nil
	}

	// 原有逻辑：处理普通路由（精确/: 参数）
	tree, ok := pr.trees[method]
	if ok {
		return tree.Add(cleanPath, handler)
	}

	tree = search.NewTree()
	pr.trees[method] = tree
	return tree.Add(cleanPath, handler)
}

func (pr *patRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqPath := path.Clean(r.URL.Path)
	method := r.Method

	// 步骤1：先尝试原生匹配（精确/: 参数）
	if tree, ok := pr.trees[method]; ok {
		if result, ok := tree.Search(reqPath); ok {
			if len(result.Params) > 0 {
				r = pathvar.WithVars(r, result.Params)
			}
			result.Item.(http.Handler).ServeHTTP(w, r)
			return
		}
	}

	// NEW: 步骤2：处理 * 通配符路由匹配
	if wildcardMap, ok := pr.wildcardRoutes[method]; ok {
		for wildcardPath, handler := range wildcardMap {
			// 拆分通配符路径：/member/*path → ["", "member", "*path"]
			parts := strings.Split(wildcardPath, "/")
			var prefix string
			var paramName string

			// 遍历找到通配符片段，拼接前缀
			for i, part := range parts {
				if strings.HasPrefix(part, "*") {
					// 拼接通配符前缀（如 /member/）
					if i == 0 {
						prefix = "/"
					} else {
						prefix = strings.Join(parts[:i], "/") + "/"
					}
					// 提取通配符参数名（*path → path）
					paramName = strings.TrimPrefix(part, "*")
					break
				}
			}

			// 检查请求路径是否以通配符前缀开头
			if strings.HasPrefix(reqPath, prefix) {
				// 提取通配符参数值（如 /member/user/123 → user/123）
				paramValue := strings.TrimPrefix(reqPath, prefix)
				// 初始化参数并存入请求上下文
				params := map[string]string{paramName: paramValue}
				r = pathvar.WithVars(r, params)
				// 执行通配符路由对应的 handler
				handler.ServeHTTP(w, r)
				return
			}
		}
	}

	// 步骤3：原有 405/404 逻辑
	allows, ok := pr.methodsAllowed(method, reqPath)
	if !ok {
		pr.handleNotFound(w, r)
		return
	}

	if pr.notAllowed != nil {
		pr.notAllowed.ServeHTTP(w, r)
	} else {
		w.Header().Set(allowHeader, allows)
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (pr *patRouter) SetNotFoundHandler(handler http.Handler) {
	pr.notFound = handler
}

func (pr *patRouter) SetNotAllowedHandler(handler http.Handler) {
	pr.notAllowed = handler
}

func (pr *patRouter) handleNotFound(w http.ResponseWriter, r *http.Request) {
	if pr.notFound != nil {
		pr.notFound.ServeHTTP(w, r)
	} else {
		http.NotFound(w, r)
	}
}

func (pr *patRouter) methodsAllowed(method, path string) (string, bool) {
	var allows []string

	for treeMethod, tree := range pr.trees {
		if treeMethod == method {
			continue
		}

		_, ok := tree.Search(path)
		if ok {
			allows = append(allows, treeMethod)
		}
	}

	// NEW: 检查通配符路由的 method allowed
	for treeMethod, wildcardMap := range pr.wildcardRoutes {
		if treeMethod == method {
			continue
		}
		for wildcardPath := range wildcardMap {
			parts := strings.Split(wildcardPath, "/")
			var prefix string
			for i, part := range parts {
				if strings.HasPrefix(part, "*") {
					if i == 0 {
						prefix = "/"
					} else {
						prefix = strings.Join(parts[:i], "/") + "/"
					}
					break
				}
			}
			if strings.HasPrefix(path, prefix) {
				allows = append(allows, treeMethod)
				break
			}
		}
	}

	if len(allows) > 0 {
		return strings.Join(allows, allowMethodSeparator), true
	}

	return "", false
}

func validMethod(method string) bool {
	return method == http.MethodDelete || method == http.MethodGet ||
		method == http.MethodHead || method == http.MethodOptions ||
		method == http.MethodPatch || method == http.MethodPost ||
		method == http.MethodPut
}

// NEW: 新增重复路由错误处理函数（和 search 包对齐）
func duplicatedItem(item string) error {
	return errors.New("duplicated item for " + item)
}
