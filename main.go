package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ================= 数据模型 =================
type Node struct {
	Type     string   `json:"type"`
	Name     string   `json:"name"`
	Children []string `json:"children,omitempty"`
	Size     int64    `json:"size"`
	Created  string   `json:"created"`
	Modified string   `json:"modified"`
	Owner    string   `json:"owner"`
	Perms    string   `json:"perms"`
}

// FileSystem 内存虚拟文件系统
type FileSystem struct {
	mu   sync.RWMutex
	data map[string]*Node
}

var FS = &FileSystem{data: make(map[string]*Node)}

// ================= 工具函数 =================
func normalizePath(parts ...string) string {
	p := strings.Join(parts, "/")
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	return strings.TrimSuffix(p, "/")
}

func nowStr() string {
	return time.Now().Format("2006/1/2 15:04:05")
}

func parentOf(path string) (string, string) {
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return "C:", path
	}
	parent := path[:idx]
	if parent == "" {
		parent = "C:"
	}
	return parent, path[idx+1:]
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ================= 初始化虚拟文件系统 =================
func initFS() {
	ts := nowStr()
	addNode := func(path, typ, name string, size int64) {
		FS.data[path] = &Node{
			Type: typ, Name: name, Size: size,
			Created: ts, Modified: ts,
			Owner: "Administrator", Perms: "读/写/执行",
		}
	}

	addNode("C:", "folder", "C:", 0)
	addNode("C:/Windows", "folder", "Windows", 0)
	addNode("C:/Users", "folder", "Users", 0)
	addNode("C:/MassiveData", "folder", "MassiveData", 0)
	FS.data["C:"].Children = []string{"Windows", "Users", "MassiveData"}

	// Windows 目录
	addNode("C:/Windows/explorer.exe", "file", "explorer.exe", int64(rand.Intn(500000)+100000))
	FS.data["C:/Windows"].Children = []string{"explorer.exe"}

	// Users 目录
	addNode("C:/Users/readme.md", "file", "readme.md", int64(rand.Intn(50000)+100))
	FS.data["C:/Users"].Children = []string{"readme.md"}

	// MassiveData 大量测试文件（用于演示虚拟滚动）
	for i := 1; i <= 1000; i++ {
		fn := fmt.Sprintf("File_%d.txt", i)
		p := "C:/MassiveData/" + fn
		addNode(p, "file", fn, int64(rand.Intn(500000)+1))
		FS.data["C:/MassiveData"].Children = append(FS.data["C:/MassiveData"].Children, fn)
	}

	fmt.Printf("[FS] 初始化完成，共 %d 个节点\n", len(FS.data))
}

// ================= 处理器 =================

// GET /api/folders?path=C:
func handleFolderContents(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "C:"
	}

	FS.mu.RLock()
	defer FS.mu.RUnlock()

	node, ok := FS.data[path]
	if !ok || node.Type != "folder" {
		writeError(w, 404, "目录不存在")
		return
	}

	items := make([]map[string]interface{}, 0, len(node.Children))
	for _, childName := range node.Children {
		cp := normalizePath(path, childName)
		child := FS.data[cp]
		if child == nil {
			continue
		}
		// 计算当前文件夹是否含有子文件夹（用于前端决定是否可展开）
		sub := false
		if child.Type == "folder" {
			for _, cc := range child.Children {
				if c2, ok := FS.data[normalizePath(cp, cc)]; ok && c2.Type == "folder" {
					sub = true
					break
				}
			}
		}
		items = append(items, map[string]interface{}{
			"path":          cp,
			"type":          child.Type,
			"name":          child.Name,
			"size":          child.Size,
			"created":       child.Created,
			"modified":      child.Modified,
			"owner":         child.Owner,
			"perms":         child.Perms,
			"hasSubFolders": sub,
		})
	}
	writeJSON(w, 200, items)
}

// GET /api/ancestors?path=C:/Users
func handleAncestors(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "C:"
	}
	parts := strings.Split(path, "/")
	current := ""
	result := make([]map[string]string, 0, len(parts))
	for _, p := range parts {
		if current == "" {
			current = p
		} else {
			current += "/" + p
		}
		result = append(result, map[string]string{"path": current, "name": p})
	}
	writeJSON(w, 200, result)
}

// DELETE /api/items  body: {"paths":["C:/Users/readme.md"]}
func handleDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "无效请求")
		return
	}

	FS.mu.Lock()
	defer FS.mu.Unlock()

	pathSet := make(map[string]bool)
	prefixes := make([]string, 0, len(req.Paths))
	for _, p := range req.Paths {
		pathSet[p] = true
		prefixes = append(prefixes, p+"/")
	}

	toDelete := make([]string, 0)
	for k := range FS.data {
		if pathSet[k] {
			toDelete = append(toDelete, k)
			continue
		}
		for _, pref := range prefixes {
			if strings.HasPrefix(k, pref) {
				toDelete = append(toDelete, k)
				break
			}
		}
	}
	for _, k := range toDelete {
		delete(FS.data, k)
	}

	// 从父节点的 children 中移除
	for _, p := range req.Paths {
		parentPath, name := parentOf(p)
		if parent, ok := FS.data[parentPath]; ok && parent.Children != nil {
			nc := make([]string, 0, len(parent.Children))
			for _, c := range parent.Children {
				if c != name {
					nc = append(nc, c)
				}
			}
			parent.Children = nc
		}
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// POST /api/items  body: {"parentPath":"C:","type":"folder","name":"新建文件夹"}
func handleCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ParentPath string `json:"parentPath"`
		Type       string `json:"type"`
		Name       string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "无效请求")
		return
	}

	FS.mu.Lock()
	defer FS.mu.Unlock()

	parent, ok := FS.data[req.ParentPath]
	if !ok || parent.Type != "folder" {
		writeError(w, 404, "父目录不存在")
		return
	}

	// 生成不重名的名称
	finalName := req.Name
	counter := 1
	for {
		p := normalizePath(req.ParentPath, finalName)
		if _, exists := FS.data[p]; !exists {
			break
		}
		base := req.Name
		ext := ""
		if dot := strings.LastIndex(req.Name, "."); dot >= 0 {
			base = req.Name[:dot]
			ext = req.Name[dot:]
		}
		finalName = fmt.Sprintf("%s(%d)%s", base, counter, ext)
		counter++
	}

	newPath := normalizePath(req.ParentPath, finalName)
	node := &Node{
		Type: req.Type, Name: finalName, Created: nowStr(), Modified: nowStr(),
		Owner: "Admin", Perms: "读/写/执行",
	}
	if req.Type == "folder" {
		node.Children = []string{}
	} else {
		node.Size = 1024
	}
	FS.data[newPath] = node
	parent.Children = append(parent.Children, finalName)

	writeJSON(w, 200, map[string]string{"path": newPath})
}

// PUT /api/items/rename  body: {"oldPath":"C:/a.txt","newName":"b.txt"}
func handleRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldPath string `json:"oldPath"`
		NewName string `json:"newName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "无效请求")
		return
	}

	FS.mu.Lock()
	defer FS.mu.Unlock()

	parentPath, oldName := parentOf(req.OldPath)
	newPath := normalizePath(parentPath, req.NewName)
	if _, exists := FS.data[newPath]; exists {
		writeError(w, 409, "文件名已存在")
		return
	}

	prefix := req.OldPath + "/"
	toMove := make([]string, 0)
	for k := range FS.data {
		if k == req.OldPath || strings.HasPrefix(k, prefix) {
			toMove = append(toMove, k)
		}
	}
	for _, k := range toMove {
		newK := strings.Replace(k, req.OldPath, newPath, 1)
		node := FS.data[k]
		if k == req.OldPath {
			node.Name = req.NewName
			node.Modified = nowStr()
		}
		FS.data[newK] = node
		delete(FS.data, k)
	}

	if parent, ok := FS.data[parentPath]; ok {
		for i, c := range parent.Children {
			if c == oldName {
				parent.Children[i] = req.NewName
				break
			}
		}
	}
	writeJSON(w, 200, map[string]string{"path": newPath})
}

// PUT /api/items/move  body: {"sources":["C:/a"],"targetFolder":"C:/Users"}
func handleMove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Sources      []string `json:"sources"`
		TargetFolder string   `json:"targetFolder"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "无效请求")
		return
	}

	FS.mu.Lock()
	defer FS.mu.Unlock()

	target, ok := FS.data[req.TargetFolder]
	if !ok || target.Type != "folder" {
		writeError(w, 404, "目标目录不存在")
		return
	}

	moved := 0
	for _, oldPath := range req.Sources {
		parentPath, name := parentOf(oldPath)
		newPath := normalizePath(req.TargetFolder, name)

		if _, exists := FS.data[newPath]; exists {
			continue
		}
		if oldPath == req.TargetFolder {
			continue
		}
		if strings.HasPrefix(req.TargetFolder, oldPath+"/") {
			continue // 不能移到自己子目录
		}

		prefix := oldPath + "/"
		toMove := make([]string, 0)
		for k := range FS.data {
			if k == oldPath || strings.HasPrefix(k, prefix) {
				toMove = append(toMove, k)
			}
		}
		for _, k := range toMove {
			newK := strings.Replace(k, oldPath, newPath, 1)
			FS.data[newK] = FS.data[k]
			delete(FS.data, k)
		}
		if parent, ok := FS.data[parentPath]; ok {
			nc := make([]string, 0, len(parent.Children))
			for _, c := range parent.Children {
				if c != name {
					nc = append(nc, c)
				}
			}
			parent.Children = nc
		}
		target.Children = append(target.Children, name)
		moved++
	}
	if moved == 0 {
		writeError(w, 400, "无法移动到当前路径")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// GET /api/properties?path=C:/Users/readme.md
func handleProperties(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	FS.mu.RLock()
	defer FS.mu.RUnlock()
	node, ok := FS.data[path]
	if !ok {
		writeError(w, 404, "文件不存在")
		return
	}
	writeJSON(w, 200, node)
}

// GET /api/download?path=C:/Users/readme.md
func handleDownload(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")

	FS.mu.RLock()
	node, ok := FS.data[path]
	FS.mu.RUnlock()
	if !ok || node.Type != "file" {
		writeError(w, 404, "文件不存在")
		return
	}

	// 限制生成上限，防止内存爆掉
	size := node.Size
	if size > 20*1024*1024 {
		size = 20 * 1024 * 1024
	}
	if size < 4096 {
		size = 4096
	}

	header := fmt.Sprintf(
		"==========================================\n"+
			"文件名: %s\n"+
			"路径:   %s\n"+
			"大小:   %d 字节\n"+
			"所有者: %s\n"+
			"修改:   %s\n"+
			"==========================================\n\n",
		node.Name, path, node.Size, node.Owner, node.Modified)

	bodyLen := size - int64(len(header))
	if bodyLen < 0 {
		bodyLen = 0
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, node.Name))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", int64(len(header))+bodyLen))

	_, _ = w.Write([]byte(header))

	// 分块写入占位内容，避免一次性大内存
	chunk := make([]byte, 32*1024)
	for i := range chunk {
		chunk[i] = byte('A' + (i % 26))
	}
	remaining := bodyLen
	for remaining > 0 {
		n := int64(len(chunk))
		if n > remaining {
			n = remaining
		}
		if _, err := w.Write(chunk[:int(n)]); err != nil {
			return
		}
		remaining -= n
	}
}

// ================= CORS 中间件 =================
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Disposition")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ================= 主入口 =================
func main() {
	initFS()

	mux := http.NewServeMux()

	// API 路由
	mux.HandleFunc("/api/folders", handleFolderContents)
	mux.HandleFunc("/api/ancestors", handleAncestors)
	mux.HandleFunc("/api/items", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodDelete:
			handleDelete(w, r)
		case http.MethodPost:
			handleCreate(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "方法不允许")
		}
	})
	mux.HandleFunc("/api/items/rename", handleRename)
	mux.HandleFunc("/api/items/move", handleMove)
	mux.HandleFunc("/api/properties", handleProperties)
	mux.HandleFunc("/api/download", handleDownload)

	// 静态页面
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			http.ServeFile(w, r, "file_explorer.html")
			return
		}
		http.FileServer(http.Dir(".")).ServeHTTP(w, r)
	})

	addr := ":8080"
	fmt.Printf("🚀 Windows File Explorer 服务端启动: http://localhost%s\n", addr)
	if err := http.ListenAndServe(addr, corsMiddleware(mux)); err != nil {
		fmt.Println("服务器启动失败:", err)
	}
}