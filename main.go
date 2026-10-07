package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ModeVFS  = "vfs"
	ModeDisk = "disk"
)

var STORAGE_MODE = ModeDisk

const CHUNK_SIZE = 4 * 1024 * 1024

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

const TRASH_ROOT = "$TRASH"

type FileSystem struct {
	mu   sync.RWMutex
	data map[string]*Node
}

var FS = &FileSystem{data: make(map[string]*Node)}
var vfsTrashMeta = make(map[string]TrashMeta)
var undoSnapshotVFS *struct {
	data      map[string]*Node
	trashMeta map[string]TrashMeta
}

const DISK_TRASH_DIR = ".file_explorer_trash"
var diskTrashMeta = map[string]TrashMeta{}

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
		return path, path
	}
	parent := path[:idx]
	if parent == "" {
		parent = "/"
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

func vfsUniqueName(targetParent, baseName string) string {
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

func snapshotVFS() {
	snapData := make(map[string]*Node, len(FS.data))
	for k, v := range FS.data {
		n := *v
		if v.Children != nil {
			n.Children = make([]string, len(v.Children))
			copy(n.Children, v.Children)
		}
		snapData[k] = &n
	}
	snapMeta := make(map[string]TrashMeta, len(vfsTrashMeta))
	for k, v := range vfsTrashMeta {
		snapMeta[k] = v
	}
	undoSnapshotVFS = &struct {
		data      map[string]*Node
		trashMeta map[string]TrashMeta
	}{snapData, snapMeta}
}

func initVFS() {
	ts := nowStr()
	add := func(path, typ, name string, size int64) {
		FS.data[path] = &Node{Type: typ, Name: name, Size: size, Created: ts, Modified: ts, Owner: "Administrator", Perms: "读/写/执行"}
	}
	add("C:", "folder", "C:", 0)
	add("C:/Windows", "folder", "Windows", 0)
	add("C:/Users", "folder", "Users", 0)
	add("C:/MassiveData", "folder", "MassiveData", 0)
	FS.data["C:"].Children = []string{"Windows", "Users", "MassiveData"}
	add("C:/Windows/explorer.exe", "file", "explorer.exe", int64(rand.Intn(500000)+100000))
	FS.data["C:/Windows"].Children = []string{"explorer.exe"}
	add("C:/Users/readme.md", "file", "readme.md", int64(rand.Intn(50000)+100))
	FS.data["C:/Users"].Children = []string{"readme.md"}
	for i := 1; i <= 1000; i++ {
		fn := fmt.Sprintf("File_%d.txt", i)
		p := "C:/MassiveData/" + fn
		add(p, "file", fn, int64(rand.Intn(500000)+1))
		FS.data["C:/MassiveData"].Children = append(FS.data["C:/MassiveData"].Children, fn)
	}
	add(TRASH_ROOT, "folder", "回收站", 0)
	FS.data[TRASH_ROOT].Children = []string{}
	fmt.Printf("[VFS] 初始化完成，共 %d 个节点\n", len(FS.data))
}

func toOSPath(p string) string {
	if runtime.GOOS == "windows" {
		if len(p) == 2 && p[1] == ':' {
			return p + "\\"
		}
		return filepath.FromSlash(p)
	}
	if p == "" {
		return "/"
	}
	return p
}

func hasSubDir(absDir string) bool {
	entries, err := os.ReadDir(absDir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() {
			return true
		}
	}
	return false
}

func listDrives() []map[string]interface{} {
	result := []map[string]interface{}{}
	if runtime.GOOS == "windows" {
		for c := 'A'; c <= 'Z'; c++ {
			drive := string(c) + ":"
			if _, err := os.Stat(drive + "\\"); err == nil {
				result = append(result, map[string]interface{}{
					"path": drive, "type": "folder", "name": drive,
					"size": int64(0), "created": "", "modified": "",
					"owner": "", "perms": "", "hasSubFolders": true,
				})
			}
		}
	} else {
		result = append(result, map[string]interface{}{
			"path": "/", "type": "folder", "name": "/",
			"size": int64(0), "created": "", "modified": "",
			"owner": "", "perms": "", "hasSubFolders": true,
		})
	}
	return result
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFileOrDir(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := os.MkdirAll(dst, info.Mode()); err != nil {
			return err
		}
		entries, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := copyFileOrDir(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func handleMode(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"mode": STORAGE_MODE})
}

func handleFolderContents(w http.ResponseWriter, r *http.Request) {
	if STORAGE_MODE == ModeDisk {
		handleFolderContentsDisk(w, r)
		return
	}
	handleFolderContentsVFS(w, r)
}

func handleFolderContentsDisk(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSON(w, 200, listDrives())
		return
	}
	absPath := toOSPath(path)
	entries, err := os.ReadDir(absPath)
	if err != nil {
		writeError(w, 403, "无法访问: "+err.Error())
		return
	}
	items := []map[string]interface{}{}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		name := e.Name()
		childPath := normalizePath(path, name)
		t := "file"
		if e.IsDir() {
			t = "folder"
		}
		sub := false
		if e.IsDir() {
			sub = hasSubDir(filepath.Join(absPath, name))
		}
		items = append(items, map[string]interface{}{
			"path": childPath, "type": t, "name": name,
			"size": info.Size(),
			"created": info.ModTime().Format("2006/1/2 15:04:05"),
			"modified": info.ModTime().Format("2006/1/2 15:04:05"),
			"owner": "user", "perms": "读/写",
			"hasSubFolders": sub,
		})
	}
	writeJSON(w, 200, items)
}

func handleFolderContentsVFS(w http.ResponseWriter, r *http.Request) {
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
			"path": cp, "type": child.Type, "name": child.Name,
			"size": child.Size, "created": child.Created, "modified": child.Modified,
			"owner": child.Owner, "perms": child.Perms, "hasSubFolders": sub,
		})
	}
	writeJSON(w, 200, items)
}

func handleAncestors(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeJSON(w, 200, []map[string]string{{"path": "", "name": "此电脑"}})
		return
	}
	if STORAGE_MODE == ModeDisk {
		result := []map[string]string{{"path": "", "name": "此电脑"}}
		if runtime.GOOS == "windows" {
			parts := strings.Split(path, "/")
			cur := ""
			for i, p := range parts {
				if i == 0 {
					cur = p
				} else {
					cur += "/" + p
				}
				result = append(result, map[string]string{"path": cur, "name": p})
			}
		} else {
			result = append(result, map[string]string{"path": "/", "name": "/"})
			parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
			cur := ""
			for _, p := range parts {
				if p == "" {
					continue
				}
				cur += "/" + p
				result = append(result, map[string]string{"path": cur, "name": p})
			}
		}
		writeJSON(w, 200, result)
		return
	}
	parts := strings.Split(path, "/")
	cur := ""
	result := []map[string]string{}
	for _, p := range parts {
		if cur == "" {
			cur = p
		} else {
			cur += "/" + p
		}
		result = append(result, map[string]string{"path": cur, "name": p})
	}
	writeJSON(w, 200, result)
}

func handleDelete(w http.ResponseWriter, r *http.Request) {
	if STORAGE_MODE == ModeDisk {
		handleDeleteDisk(w, r)
		return
	}
	handleDeleteVFS(w, r)
}

func handleDeleteDisk(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "无效请求")
		return
	}
	if err := os.MkdirAll(DISK_TRASH_DIR, 0755); err != nil {
		writeError(w, 500, "无法创建回收站: "+err.Error())
		return
	}
	for _, p := range req.Paths {
		srcPath := toOSPath(p)
		if _, err := os.Stat(srcPath); err != nil {
			continue
		}
		name := filepath.Base(srcPath)
		trashName := fmt.Sprintf("%d_%s", time.Now().UnixNano(), name)
		trashAbsPath := filepath.Join(DISK_TRASH_DIR, trashName)
		if err := os.Rename(srcPath, trashAbsPath); err != nil {
			writeError(w, 500, "无法移入回收站: "+err.Error())
			return
		}
		diskTrashMeta[trashAbsPath] = TrashMeta{OriginalPath: p, DeletedAt: nowStr()}
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func handleDeleteVFS(w http.ResponseWriter, r *http.Request) {
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
	snapshotVFS()
	for _, p := range req.Paths {
		if p == TRASH_ROOT || strings.HasPrefix(p, TRASH_ROOT+"/") {
			continue
		}
		parentPath, name := parentOf(p)
		if _, ok := FS.data[p]; !ok {
			continue
		}
		trashName := vfsUniqueName(TRASH_ROOT, name)
		trashPath := normalizePath(TRASH_ROOT, trashName)
		prefix := p + "/"
		toMove := []string{}
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
			nc := []string{}
			for _, c := range parent.Children {
				if c != name {
					nc = append(nc, c)
				}
			}
			parent.Children = nc
		}
		FS.data[TRASH_ROOT].Children = append(FS.data[TRASH_ROOT].Children, trashName)
		vfsTrashMeta[trashPath] = TrashMeta{OriginalPath: p, DeletedAt: nowStr()}
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func handleTrashList(w http.ResponseWriter, r *http.Request) {
	if STORAGE_MODE == ModeDisk {
		handleTrashListDisk(w, r)
		return
	}
	handleTrashListVFS(w, r)
}

func handleTrashListDisk(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(DISK_TRASH_DIR)
	if err != nil {
		writeJSON(w, 200, []interface{}{})
		return
	}
	items := []map[string]interface{}{}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		absPath := filepath.Join(DISK_TRASH_DIR, e.Name())
		meta := diskTrashMeta[absPath]
		t := "file"
		if e.IsDir() {
			t = "folder"
		}
		items = append(items, map[string]interface{}{
			"path": absPath, "type": t, "name": e.Name(),
			"size": info.Size(),
			"created": info.ModTime().Format("2006/1/2 15:04:05"),
			"modified": info.ModTime().Format("2006/1/2 15:04:05"),
			"owner": "", "perms": "", "hasSubFolders": false,
			"inTrash": true,
			"originalPath": meta.OriginalPath,
			"deletedAt": meta.DeletedAt,
		})
	}
	writeJSON(w, 200, items)
}

func handleTrashListVFS(w http.ResponseWriter, r *http.Request) {
	FS.mu.RLock()
	defer FS.mu.RUnlock()
	node, ok := FS.data[TRASH_ROOT]
	if !ok {
		writeJSON(w, 200, []interface{}{})
		return
	}
	items := []map[string]interface{}{}
	for _, childName := range node.Children {
		cp := normalizePath(TRASH_ROOT, childName)
		child := FS.data[cp]
		if child == nil {
			continue
		}
		meta := vfsTrashMeta[cp]
		items = append(items, map[string]interface{}{
			"path": cp, "type": child.Type, "name": child.Name,
			"size": child.Size, "created": child.Created, "modified": child.Modified,
			"owner": child.Owner, "perms": child.Perms, "hasSubFolders": false,
			"inTrash": true, "originalPath": meta.OriginalPath, "deletedAt": meta.DeletedAt,
		})
	}
	writeJSON(w, 200, items)
}

func handleTrashRestore(w http.ResponseWriter, r *http.Request) {
	if STORAGE_MODE == ModeDisk {
		handleTrashRestoreDisk(w, r)
		return
	}
	handleTrashRestoreVFS(w, r)
}

func handleTrashRestoreDisk(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "无效请求")
		return
	}
	for _, trashPath := range req.Paths {
		if !strings.Contains(trashPath, DISK_TRASH_DIR) {
			continue
		}
		meta, ok := diskTrashMeta[trashPath]
		if !ok {
			continue
		}
		if _, err := os.Stat(trashPath); err != nil {
			continue
		}
		targetAbs := toOSPath(meta.OriginalPath)
		if _, err := os.Stat(targetAbs); err == nil {
			dir := filepath.Dir(targetAbs)
			base := filepath.Base(targetAbs)
			ext := filepath.Ext(base)
			nameOnly := strings.TrimSuffix(base, ext)
			for i := 1; ; i++ {
				targetAbs = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", nameOnly, i, ext))
				if _, err := os.Stat(targetAbs); err != nil {
					break
				}
			}
		}
		os.MkdirAll(filepath.Dir(targetAbs), 0755)
		if err := os.Rename(trashPath, targetAbs); err != nil {
			writeError(w, 500, "还原失败: "+err.Error())
			return
		}
		delete(diskTrashMeta, trashPath)
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func handleTrashRestoreVFS(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "无效请求")
		return
	}
	FS.mu.Lock()
	defer FS.mu.Unlock()
	canRestore := false
	for _, tp := range req.Paths {
		if _, ok := vfsTrashMeta[tp]; !ok {
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
	snapshotVFS()
	for _, trashPath := range req.Paths {
		if !strings.HasPrefix(trashPath, TRASH_ROOT+"/") {
			continue
		}
		meta, ok := vfsTrashMeta[trashPath]
		if !ok {
			continue
		}
		if _, ok := FS.data[trashPath]; !ok {
			continue
		}
		targetParent, targetName := parentOf(meta.OriginalPath)
		if _, ok := FS.data[targetParent]; !ok {
			targetParent = "C:"
		}
		finalName := vfsUniqueName(targetParent, targetName)
		finalPath := normalizePath(targetParent, finalName)
		prefix := trashPath + "/"
		toMove := []string{}
		for k := range FS.data {
			if k == trashPath || strings.HasPrefix(k, prefix) {
				toMove = append(toMove, k)
			}
		}
		for _, k := range toMove {
			newK := strings.Replace(k, trashPath, finalPath, 1)
			FS.data[newK] = FS.data[k]
			delete(FS.data, k)
			if m, ok := vfsTrashMeta[k]; ok {
				vfsTrashMeta[newK] = m
				delete(vfsTrashMeta, k)
			}
		}
		if tpNode, ok := FS.data[TRASH_ROOT]; ok {
			baseName := strings.TrimPrefix(trashPath, TRASH_ROOT+"/")
			nc := []string{}
			for _, c := range tpNode.Children {
				if c != baseName {
					nc = append(nc, c)
				}
			}
			tpNode.Children = nc
		}
		if fp, ok := FS.data[targetParent]; ok {
			fp.Children = append(fp.Children, finalName)
		}
		delete(vfsTrashMeta, finalPath)
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func handleTrashPurge(w http.ResponseWriter, r *http.Request) {
	if STORAGE_MODE == ModeDisk {
		handleTrashPurgeDisk(w, r)
		return
	}
	handleTrashPurgeVFS(w, r)
}

func handleTrashPurgeDisk(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths []string `json:"paths"`
		All   bool     `json:"all"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "无效请求")
		return
	}
	if req.All {
		os.RemoveAll(DISK_TRASH_DIR)
		os.MkdirAll(DISK_TRASH_DIR, 0755)
		diskTrashMeta = map[string]TrashMeta{}
		writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	for _, p := range req.Paths {
		if err := os.RemoveAll(p); err != nil {
			writeError(w, 500, "删除失败: "+err.Error())
			return
		}
		delete(diskTrashMeta, p)
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func handleTrashPurgeVFS(w http.ResponseWriter, r *http.Request) {
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
	snapshotVFS()
	if req.All {
		if node, ok := FS.data[TRASH_ROOT]; ok {
			node.Children = []string{}
		}
		for k := range FS.data {
			if strings.HasPrefix(k, TRASH_ROOT+"/") {
				delete(FS.data, k)
			}
		}
		vfsTrashMeta = make(map[string]TrashMeta)
		writeJSON(w, 200, map[string]string{"status": "ok"})
		return
	}
	for _, p := range req.Paths {
		if !strings.HasPrefix(p, TRASH_ROOT+"/") {
			continue
		}
		prefix := p + "/"
		toDelete := []string{}
		for k := range FS.data {
			if k == p || strings.HasPrefix(k, prefix) {
				toDelete = append(toDelete, k)
			}
		}
		for _, k := range toDelete {
			delete(FS.data, k)
			delete(vfsTrashMeta, k)
		}
		parentPath, name := parentOf(p)
		if parent, ok := FS.data[parentPath]; ok {
			nc := []string{}
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

func handleCreate(w http.ResponseWriter, r *http.Request) {
	if STORAGE_MODE == ModeDisk {
		handleCreateDisk(w, r)
		return
	}
	handleCreateVFS(w, r)
}

func handleCreateDisk(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ParentPath string `json:"parentPath"`
		Type       string `json:"type"`
		Name       string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "无效请求")
		return
	}
	if req.ParentPath == "" {
		writeError(w, 400, "无法在根创建文件")
		return
	}
	newPath := normalizePath(req.ParentPath, req.Name)
	absPath := toOSPath(newPath)
	if _, err := os.Stat(absPath); err == nil {
		writeError(w, 409, "已存在同名文件")
		return
	}
	if req.Type == "folder" {
		if err := os.Mkdir(absPath, 0755); err != nil {
			writeError(w, 500, err.Error())
			return
		}
	} else {
		f, err := os.Create(absPath)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		f.Close()
	}
	writeJSON(w, 200, map[string]string{"path": newPath})
}

func handleCreateVFS(w http.ResponseWriter, r *http.Request) {
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
	finalName := vfsUniqueName(req.ParentPath, req.Name)
	newPath := normalizePath(req.ParentPath, finalName)
	snapshotVFS()
	node := &Node{Type: req.Type, Name: finalName, Created: nowStr(), Modified: nowStr(), Owner: "Admin", Perms: "读/写/执行"}
	if req.Type == "folder" {
		node.Children = []string{}
	} else {
		node.Size = 1024
	}
	FS.data[newPath] = node
	parent.Children = append(parent.Children, finalName)
	writeJSON(w, 200, map[string]string{"path": newPath})
}

func handleRename(w http.ResponseWriter, r *http.Request) {
	if STORAGE_MODE == ModeDisk {
		handleRenameDisk(w, r)
		return
	}
	handleRenameVFS(w, r)
}

func handleRenameDisk(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldPath string `json:"oldPath"`
		NewName string `json:"newName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "无效请求")
		return
	}
	oldAbs := toOSPath(req.OldPath)
	parentPath, _ := parentOf(req.OldPath)
	newPath := normalizePath(parentPath, req.NewName)
	newAbs := toOSPath(newPath)
	if _, err := os.Stat(newAbs); err == nil {
		writeError(w, 409, "已存在同名文件")
		return
	}
	if err := os.Rename(oldAbs, newAbs); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"path": newPath})
}

func handleRenameVFS(w http.ResponseWriter, r *http.Request) {
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
	snapshotVFS()
	prefix := req.OldPath + "/"
	toMove := []string{}
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

func handleMove(w http.ResponseWriter, r *http.Request) {
	if STORAGE_MODE == ModeDisk {
		handleMoveDisk(w, r)
		return
	}
	handleMoveVFS(w, r)
}

func handleMoveDisk(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Sources      []string `json:"sources"`
		TargetFolder string   `json:"targetFolder"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "无效请求")
		return
	}
	targetAbs := toOSPath(req.TargetFolder)
	info, err := os.Stat(targetAbs)
	if err != nil || !info.IsDir() {
		writeError(w, 404, "目标目录不存在")
		return
	}
	moved := 0
	for _, src := range req.Sources {
		srcAbs := toOSPath(src)
		if _, err := os.Stat(srcAbs); err != nil {
			continue
		}
		name := filepath.Base(srcAbs)
		newAbs := filepath.Join(targetAbs, name)
		if _, err := os.Stat(newAbs); err == nil {
			continue
		}
		if strings.HasPrefix(req.TargetFolder, src+"/") || req.TargetFolder == src {
			continue
		}
		if err := os.Rename(srcAbs, newAbs); err != nil {
			writeError(w, 500, err.Error())
			return
		}
		moved++
	}
	if moved == 0 {
		writeError(w, 400, "无法移动")
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func handleMoveVFS(w http.ResponseWriter, r *http.Request) {
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
			snapshotVFS()
		}
		prefix := oldPath + "/"
		toMove := []string{}
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
			nc := []string{}
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

func handleCopy(w http.ResponseWriter, r *http.Request) {
	if STORAGE_MODE == ModeDisk {
		handleCopyDisk(w, r)
		return
	}
	handleCopyVFS(w, r)
}

func handleCopyDisk(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Sources      []string `json:"sources"`
		TargetFolder string   `json:"targetFolder"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "无效请求")
		return
	}
	targetAbs := toOSPath(req.TargetFolder)
	info, err := os.Stat(targetAbs)
	if err != nil || !info.IsDir() {
		writeError(w, 404, "目标目录不存在")
		return
	}
	for _, src := range req.Sources {
		srcAbs := toOSPath(src)
		if _, err := os.Stat(srcAbs); err != nil {
			continue
		}
		if strings.HasPrefix(req.TargetFolder, src+"/") || req.TargetFolder == src {
			continue
		}
		name := filepath.Base(srcAbs)
		newAbs := filepath.Join(targetAbs, name)
		if _, err := os.Stat(newAbs); err == nil {
			ext := filepath.Ext(name)
			nameOnly := strings.TrimSuffix(name, ext)
			for i := 1; ; i++ {
				newAbs = filepath.Join(targetAbs, fmt.Sprintf("%s (%d)%s", nameOnly, i, ext))
				if _, err := os.Stat(newAbs); err != nil {
					break
				}
			}
		}
		if err := copyFileOrDir(srcAbs, newAbs); err != nil {
			writeError(w, 500, err.Error())
			return
		}
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func handleCopyVFS(w http.ResponseWriter, r *http.Request) {
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
		writeError(w, 400, "无法复制")
		return
	}
	snapshotVFS()
	for _, src := range req.Sources {
		node, ok := FS.data[src]
		if !ok {
			continue
		}
		if strings.HasPrefix(req.TargetFolder, src+"/") || src == req.TargetFolder {
			continue
		}
		name := node.Name
		finalName := vfsUniqueName(req.TargetFolder, name)
		newPath := normalizePath(req.TargetFolder, finalName)
		prefix := src + "/"
		toCopy := []string{}
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

func handleUndo(w http.ResponseWriter, r *http.Request) {
	if STORAGE_MODE == ModeDisk {
		writeError(w, 400, "物理磁盘模式暂不支持撤销")
		return
	}
	FS.mu.Lock()
	defer FS.mu.Unlock()
	if undoSnapshotVFS == nil {
		writeError(w, 400, "没有可撤销的操作")
		return
	}
	FS.data = undoSnapshotVFS.data
	vfsTrashMeta = undoSnapshotVFS.trashMeta
	undoSnapshotVFS = nil
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func handleProperties(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if STORAGE_MODE == ModeDisk {
		absPath := toOSPath(path)
		info, err := os.Stat(absPath)
		if err != nil {
			writeError(w, 404, "文件不存在")
			return
		}
		t := "file"
		if info.IsDir() {
			t = "folder"
		}
		writeJSON(w, 200, map[string]interface{}{
			"type": t, "name": filepath.Base(absPath), "size": info.Size(),
			"created": info.ModTime().Format("2006/1/2 15:04:05"),
			"modified": info.ModTime().Format("2006/1/2 15:04:05"),
			"owner": "user", "perms": "读/写",
		})
		return
	}
	FS.mu.RLock()
	defer FS.mu.RUnlock()
	node, ok := FS.data[path]
	if !ok {
		writeError(w, 404, "文件不存在")
		return
	}
	writeJSON(w, 200, node)
}

func handleDownloadMeta(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")

	if STORAGE_MODE == ModeVFS {
		FS.mu.RLock()
		node, ok := FS.data[path]
		FS.mu.RUnlock()
		if !ok || node.Type != "file" {
			writeError(w, 404, "文件不存在")
			return
		}
		totalChunks := int64(0)
		if node.Size > 0 {
			totalChunks = (node.Size + CHUNK_SIZE - 1) / CHUNK_SIZE
		}
		if totalChunks == 0 {
			totalChunks = 1
		}
		writeJSON(w, 200, map[string]interface{}{
			"name": node.Name, "size": node.Size, "sha256": "",
			"chunkSize": CHUNK_SIZE, "totalChunks": totalChunks,
			"virtual": true,
		})
		return
	}

	absPath := toOSPath(path)
	info, err := os.Stat(absPath)
	if err != nil || info.IsDir() {
		writeError(w, 404, "文件不存在")
		return
	}
	sha, err := sha256File(absPath)
	if err != nil {
		sha = ""
	}
	size := info.Size()
	totalChunks := (size + CHUNK_SIZE - 1) / CHUNK_SIZE
	if totalChunks == 0 {
		totalChunks = 1
	}
	writeJSON(w, 200, map[string]interface{}{
		"name": filepath.Base(absPath), "size": size, "sha256": sha,
		"chunkSize": CHUNK_SIZE, "totalChunks": totalChunks,
		"virtual": false,
	})
}

func handleDownloadChunk(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	indexStr := r.URL.Query().Get("index")
	index, err := strconv.Atoi(indexStr)
	if err != nil || index < 0 {
		writeError(w, 400, "无效分片索引")
		return
	}

	if STORAGE_MODE == ModeVFS {
		FS.mu.RLock()
		node, ok := FS.data[path]
		FS.mu.RUnlock()
		if !ok || node.Type != "file" {
			writeError(w, 404, "文件不存在")
			return
		}
		offset := int64(index) * CHUNK_SIZE
		remaining := node.Size - offset
		if remaining <= 0 {
			writeError(w, 416, "分片越界")
			return
		}
		n := int64(CHUNK_SIZE)
		if n > remaining {
			n = remaining
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", n))
		chunk := make([]byte, 32*1024)
		for i := range chunk {
			chunk[i] = byte('A' + (i % 26))
		}
		for n > 0 {
			wc := int64(len(chunk))
			if wc > n {
				wc = n
			}
			if _, err := w.Write(chunk[:int(wc)]); err != nil {
				return
			}
			n -= wc
		}
		return
	}

	absPath := toOSPath(path)
	f, err := os.Open(absPath)
	if err != nil {
		writeError(w, 404, "无法打开文件")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	offset := int64(index) * CHUNK_SIZE
	if offset >= info.Size() {
		writeError(w, 416, "分片越界")
		return
	}
	n := int64(CHUNK_SIZE)
	if offset+n > info.Size() {
		n = info.Size() - offset
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", n))
	_, _ = io.CopyN(w, f, n)
}

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

func main() {
	if STORAGE_MODE == ModeVFS {
		initVFS()
	} else {
		fmt.Printf("[DISK] 物理磁盘模式，回收站目录: %s\n", DISK_TRASH_DIR)
		os.MkdirAll(DISK_TRASH_DIR, 0755)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/mode", handleMode)
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
	mux.HandleFunc("/api/download/meta", handleDownloadMeta)
	mux.HandleFunc("/api/download/chunk", handleDownloadChunk)
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
	fmt.Printf("🚀 服务端启动: http://localhost%s  [模式=%s]\n", addr, STORAGE_MODE)
	if err := http.ListenAndServe(addr, corsMiddleware(mux)); err != nil {
		fmt.Println("服务端启动失败:", err)
	}
}