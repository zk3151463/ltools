package col1688

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

// store 负责把最近一次采集结果与导出记录持久化到 dataDir/col1688/。
type store struct {
	dir string
}

// NewStore 创建/打开结果存储目录。
func NewStore(dataDir string) *store {
	dir := filepath.Join(dataDir, "col1688")
	_ = os.MkdirAll(dir, 0o755)
	return &store{dir: dir}
}

type savedResult struct {
	Keyword string     `json:"keyword"`
	Items   []Supplier `json:"items"`
	SavedAt string     `json:"savedAt"`
}

// Save 保存最近一次结果。
func (s *store) Save(keyword string, items []Supplier) error {
	data := savedResult{Keyword: keyword, Items: items, SavedAt: nowLocal()}
	b, _ := json.MarshalIndent(data, "", "  ")
	return os.WriteFile(filepath.Join(s.dir, "results.json"), b, 0o644)
}

// Load 加载最近一次结果。
func (s *store) Load() []Supplier {
	b, err := os.ReadFile(filepath.Join(s.dir, "results.json"))
	if err != nil {
		return nil
	}
	var data savedResult
	if err := json.Unmarshal(b, &data); err != nil {
		return nil
	}
	return data.Items
}

// ExportDir 返回导出 CSV 所在目录。
func (s *store) ExportDir() string {
	return filepath.Join(s.dir, "export")
}

// ListExports 列出已导出的 CSV 文件（按名称排序）。
func (s *store) ListExports() []string {
	entries, err := os.ReadDir(filepath.Join(s.dir, "export"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}
