package col1688

import (
	"bytes"
	"encoding/csv"
	"os"
	"path/filepath"
	"time"
)

// toCSV 把供应商记录序列化为 CSV（带 UTF-8 BOM，便于 Excel 正确识别中文）。
func toCSV(items []Supplier) []byte {
	var buf bytes.Buffer
	buf.WriteString("\ufeff")
	w := csv.NewWriter(&buf)
	_ = w.Write([]string{"名称", "电话", "地址", "主营业务", "实力等级", "来源链接", "来源", "采集时间"})
	for _, s := range items {
		_ = w.Write([]string{s.Name, s.Phone, s.Address, s.Main, s.Level, s.URL, s.Source, s.Time})
	}
	w.Flush()
	return buf.Bytes()
}

// SaveCSV 把结果写入指定目录下的带时间戳 CSV 文件，返回绝对路径。
func SaveCSV(items []Supplier, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := "1688_" + time.Now().Format("20060102_150405") + ".csv"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, toCSV(items), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
