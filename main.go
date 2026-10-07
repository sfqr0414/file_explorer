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

const TRASH_ROOT = "$TRASH"

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

type TrashMeta struct {
	OriginalPath string `json:"originalPath"`
	DeletedAt    string `json:"deletedAt"`
}

type FileSystem struct {
	mu   sync.RWMutex
	data map[string]*Node
}

var FS = &FileSystem{data: make(map[string]*Node)}

var trashMeta = make(map[string]TrashMeta)

// ================= 撤销快照 =================
type FSSnapshot struct {
	data      map[string]*Node
	trashMeta map[string]TrashMeta
}

var undoSnapshot *FSSnapshot

func snapshotFS() {
	snapData := make(map[string]*Node, len(FS.data))
	for k, v := range FS.data {
		n := *v
		if v.Children != nil {
			n.Children = make([]string, len(v.Children))
			copy(n.Children, v.Children)
		}
		snapData[k] = &n
	}
	snapMeta := make(map[string]TrashMeta, len(trashMeta))
	for k, v := range trashMeta {
		snapMeta[k] = v
	}
	undoSnapshot = &FSSnapshot{snapData, snapMeta}
}

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

// uniqueName 在 targetParent 下生成不重名的子名字
func uniqueName(targetParent, baseName string) string {
	finalName := baseName
	counter := 1
	for {
		testPath := normalizePath(targetParent, finalName)
		if _, exists := FS.data[testPath]; !exists {
			return finalName
		}
		base := baseName
		ext := ""
		if dot := strings.LastIndex(baseName, "."); dot > 0 {
			base = baseName[:dot]
			ext = baseName[dot:]
		}
		finalName = fmt.Sprintf("%s (%d)%s", base, counter, ext)
		counter++
	}
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

	addNode("C:/Windows/explorer.exe", "file", "explorer.exe", int64(rand.Intn(500000)+100000))
	FS.data["C:/Windows"].Children = []string{"explorer.exe"}

	addNode("C:/Users/readme.md", "file", "readme.md", int64(rand.Intn(50000)+100))
	FS.data["C:/Users"].Children = []string{"readme.md"}

	for i := 1; i <= 1000; i++ {
		fn := fmt.Sprintf("File_%d.txt", i)
		p := "C:/MassiveData/" + fn
		addNode(p, "file", fn, int64(rand.Intn(500000)+1))
		FS.data["C:/MassiveData"].Children = append(FS.data["C:/MassiveData"].Children, fn)
	}

	// 回收站根
	addNode(TRASH_ROOT, "folder", "回收站", 0)
	FS.data[TRASH_ROOT].Children = []string{}

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

// DELETE /api/items  → 移动到回收站
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

	canDelete := false
	for _, p := range req.Paths {
		if p == TRASH_ROOT || strings.HasPrefix(p, TRASH_ROOT+"/") {
			continue
		}
		if _, ok := FS.data[p]; ok {
			canDelete = true
			break
		}
	}
	if !canDelete {
		writeError(w, 400, "没有可删除的项目")
		return
	}

	snapshotFS()

	for _, p := range req.Paths {
		if p == TRASH_ROOT || strings.HasPrefix(p, TRASH_ROOT+"/") {
			continue
		}
		parentPath, name := parentOf(p)
		if _, ok := FS.data[p]; !ok {
			continue
		}

		trashName := uniqueName(TRASH_ROOT, name)
		trashPath := normalizePath(TRASH_ROOT, trashName)

		prefix := p + "/"
		toMove := make([]string, 0)
		for k := range FS.data {
			if k == p || strings.HasPrefix(k, prefix) {
				toMove = append(toMove, k)
			}
		}
		for _, k := range toMove {
			newK := strings.Replace(k, p, trashPath, 1)
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
		FS.data[TRASH_ROOT].Children = append(FS.data[TRASH_ROOT].Children, trashName)
		trashMeta[trashPath] = TrashMeta{OriginalPath: p, DeletedAt: nowStr()}
	}

	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// GET /api/trash
func handleTrashList(w http.ResponseWriter, r *http.Request) {
	FS.mu.RLock()
	defer FS.mu.RUnlock()

	node, ok := FS.data[TRASH_ROOT]
	if !ok {
		writeJSON(w, 200, []interface{}{})
		return
	}

	items := make([]map[string]interface{}, 0, len(node.Children))
	for _, childName := range node.Children {
		cp := normalizePath(TRASH_ROOT, childName)
		child := FS.data[cp]
		if child == nil {
			continue
		}
		meta := trashMeta[cp]
		items = append(items, map[string]interface{}{
			"path":          cp,
			"type":          child.Type,
			"name":          child.Name,
			"size":          child.Size,
			"created":       child.Created,
			"modified":      child.Modified,
			"owner":         child.Owner,
			"perms":         child.Perms,
			"hasSubFolders": false,
			"inTrash":       true,
			"originalPath":  meta.OriginalPath,
			"deletedAt":     meta.DeletedAt,
		})
	}
	writeJSON(w, 200, items)
}

// POST /api/trash/restore  body: {paths: [...]}
// 【v4 修复】parentOf 多返回值正确处理 + 简化 canRestore 判断
func handleTrashRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "无效请求")
		return
	}

	FS.mu.Lock()
	defer FS.mu.Unlock()

	// 判断有没有可还原的项（meta 存在 + 节点存在）
	canRestore := false
	for _, tp := range req.Paths {
		if _, ok := trashMeta[tp]; !ok {
			continue
		}
		if _, exists := FS.data[tp]; !exists {
			continue
		}
		canRestore = true
		break
	}
	if !canRestore {
		writeError(w, 400, "没有可还原的项目")
		return
	}

	snapshotFS()

	for _, trashPath := range req.Paths {
		if !strings.HasPrefix(trashPath, TRASH_ROOT+"/") {
			continue
		}
		meta, ok := trashMeta[trashPath]
		if !ok {
			continue
		}
		if _, ok := FS.data[trashPath]; !ok {
			continue
		}

		// 【修复】parentOf 返回两个值，用两个变量接收
		targetParent, targetName := parentOf(meta.OriginalPath)
		if _, ok := FS.data[targetParent]; !ok {
			targetParent = "C:"
		}
		finalName := uniqueName(targetParent, targetName)
		finalPath := normalizePath(targetParent, finalName)

		prefix := trashPath + "/"
		toMove := make([]string, 0)
		for k := range FS.data {
			if k == trashPath || strings.HasPrefix(k, prefix) {
				toMove = append(toMove, k)
			}
		}
		for _, k := range toMove {
			newK := strings.Replace(k, trashPath, finalPath, 1)
			FS.data[newK] = FS.data[k]
			delete(FS.data, k)
			if m, ok := trashMeta[k]; ok {
				trashMeta[newK] = m
				delete(trashMeta, k)
			}
		}

		// 【修复】从回收站 children 里移除（用 TrimPrefix 简化）
		if tpNode, ok := FS.data[TRASH_ROOT]; ok {
			baseName := strings.TrimPrefix(trashPath, TRASH_ROOT+"/")
			nc := make([]string, 0, len(tpNode.Children))
			for _, c := range tpNode.Children {
				if c != baseName {
					nc = append(nc, c)
				}
			}
			tpNode.Children = nc
		}
		// 加入目标父目录
		if fp, ok := FS.data[targetParent]; ok {
			fp.Children = append(fp.Children, finalName)
		}
		// 清理 meta
		delete(trashMeta, finalPath)
	}

	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// DELETE /api/trash  body: {paths:[...]} or {all:true}
func handleTrashPurge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths []string `json:"paths"`
		All   bool     `json:"all"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "无效请求")
		return
	}

	FS.mu.Lock()
	defer FS.mu.Unlock()

	snapshotFS()

	if req.All {
		if node, ok := FS.data[TRASH_ROOT]; ok {
			node.Children = []string{}
		}
		for k := range FS.data {
			if strings.HasPrefix(k, TRASH_ROOT+"/") {
				delete(FS.data, k)
			}
		}
		trashMeta = make(map[string]TrashMeta)
		writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	}

	for _, p := range req.Paths {
		if !strings.HasPrefix(p, TRASH_ROOT+"/") {
			continue
		}
		prefix := p + "/"
		toDelete := make([]string, 0)
		for k := range FS.data {
			if k == p || strings.HasPrefix(k, prefix) {
				toDelete = append(toDelete, k)
			}
		}
		for _, k := range toDelete {
			delete(FS.data, k)
			delete(trashMeta, k)
		}
		parentPath, name := parentOf(p)
		if parent, ok := FS.data[parentPath]; ok {
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

// POST /api/items  body: {"parentPath":"C:","type":"folder","name":"xxx"}
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

	finalName := uniqueName(req.ParentPath, req.Name)
	newPath := normalizePath(req.ParentPath, finalName)

	snapshotFS()

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

// PUT /api/items/rename
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

	snapshotFS()

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

// PUT /api/items/move
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
			continue
		}
		if _, ok := FS.data[oldPath]; !ok {
			continue
		}

		if moved == 0 {
			snapshotFS()
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

// POST /api/items/copy
func handleCopy(w http.ResponseWriter, r *http.Request) {
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

	canCopy := false
	for _, src := range req.Sources {
		if _, ok := FS.data[src]; !ok {
			continue
		}
		if strings.HasPrefix(req.TargetFolder, src+"/") || src == req.TargetFolder {
			continue
		}
		canCopy = true
		break
	}
	if !canCopy {
		writeError(w, 400, "无法复制到当前路径")
		return
	}

	snapshotFS()

	for _, src := range req.Sources {
		node, ok := FS.data[src]
		if !ok {
			continue
		}
		if strings.HasPrefix(req.TargetFolder, src+"/") || src == req.TargetFolder {
			continue
		}
		name := node.Name
		finalName := uniqueName(req.TargetFolder, name)
		newPath := normalizePath(req.TargetFolder, finalName)

		prefix := src + "/"
		toCopy := make([]string, 0)
		for k := range FS.data {
			if k == src || strings.HasPrefix(k, prefix) {
				toCopy = append(toCopy, k)
			}
		}
		for _, k := range toCopy {
			v := FS.data[k]
			newK := strings.Replace(k, src, newPath, 1)
			n := *v
			if v.Children != nil {
				n.Children = make([]string, len(v.Children))
				copy(n.Children, v.Children)
			}
			if k == src {
				n.Name = finalName
				n.Modified = nowStr()
			}
			FS.data[newK] = &n
		}
		target.Children = append(target.Children, finalName)
	}

	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// POST /api/items/undo
func handleUndo(w http.ResponseWriter, r *http.Request) {
	FS.mu.Lock()
	defer FS.mu.Unlock()

	if undoSnapshot == nil {
		writeError(w, 400, "没有可撤销的操作")
		return
	}
	FS.data = undoSnapshot.data
	trashMeta = undoSnapshot.trashMeta
	undoSnapshot = nil
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// GET /api/properties
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

// GET /api/download
func handleDownload(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")

	FS.mu.RLock()
	node, ok := FS.data[path]
	FS.mu.RUnlock()
	if !ok || node.Type != "file" {
		writeError(w, 404, "文件不存在")
		return
	}

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

// ================= CORS =================
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

// ================= main =================
func main() {
	initFS()

	mux := http.NewServeMux()

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
	mux.HandleFunc("/api/items/copy", handleCopy)
	mux.HandleFunc("/api/items/undo", handleUndo)
	mux.HandleFunc("/api/properties", handleProperties)
	mux.HandleFunc("/api/download", handleDownload)

	mux.HandleFunc("/api/trash", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			handleTrashList(w, r)
		case http.MethodDelete:
			handleTrashPurge(w, r)
		default:
			writeError(w, http.StatusMethodNotAllowed, "方法不允许")
		}
	})
	mux.HandleFunc("/api/trash/restore", handleTrashRestore)

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