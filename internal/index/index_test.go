package index

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWalkAndCollect_excludesGit(t *testing.T) {
	tmp := t.TempDir()

	// Create a normal txt file
	err := os.WriteFile(filepath.Join(tmp, "normal.txt"), []byte("hello"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	// Create a .git directory with a file
	gitDir := filepath.Join(tmp, ".git")
	err = os.MkdirAll(gitDir, 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(gitDir, "config"), []byte("test"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	// Create node_modules
	nodeDir := filepath.Join(tmp, "node_modules")
	err = os.MkdirAll(nodeDir, 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(nodeDir, "pkg.js"), []byte("test"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	files, err := walkAndCollect(tmp)
	if err != nil {
		t.Fatal(err)
	}

	// Should only have normal.txt
	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d: %v", len(files), files)
	}
	if !strings.HasSuffix(files[0], "normal.txt") {
		t.Fatalf("expected normal.txt, got %s", files[0])
	}
}

func TestWalkAndCollect_excludesBinaryExts(t *testing.T) {
	tmp := t.TempDir()

	// Create various binary files
	binaryFiles := []string{"app.exe", "lib.so", "image.jpg", "video.mp4", "archive.zip"}
	for _, name := range binaryFiles {
		err := os.WriteFile(filepath.Join(tmp, name), []byte("binary"), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}

	// Create a text file
	err := os.WriteFile(filepath.Join(tmp, "readme.txt"), []byte("hello"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	files, err := walkAndCollect(tmp)
	if err != nil {
		t.Fatal(err)
	}

	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d: %v", len(files), files)
	}
}

func TestWalkAndCollect_supportsNewExtensions(t *testing.T) {
	tmp := t.TempDir()

	extensions := []string{
		".md", ".html", ".htm", ".csv", ".tsv",
		".json", ".yaml", ".yml", ".eml",
	}
	for _, ext := range extensions {
		err := os.WriteFile(filepath.Join(tmp, "file"+ext), []byte("content"), 0644)
		if err != nil {
			t.Fatal(err)
		}
	}

	files, err := walkAndCollect(tmp)
	if err != nil {
		t.Fatal(err)
	}

	if len(files) != len(extensions) {
		t.Fatalf("expected %d files, got %d: %v", len(extensions), len(files), files)
	}
}

func TestWalkAndCollect_excludesLargeFiles(t *testing.T) {
	tmp := t.TempDir()

	// Create a file larger than default 10MB
	largeFile := filepath.Join(tmp, "large.txt")
	f, err := os.Create(largeFile)
	if err != nil {
		t.Fatal(err)
	}
	// Write 11MB
	data := make([]byte, 11*1024*1024)
	_, err = f.Write(data)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}

	// Create a small file
	err = os.WriteFile(filepath.Join(tmp, "small.txt"), []byte("hello"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	files, err := walkAndCollect(tmp)
	if err != nil {
		t.Fatal(err)
	}

	if len(files) != 1 {
		t.Fatalf("expected 1 file (small.txt), got %d: %v", len(files), files)
	}
}

func TestWalkAndCollect_noExtensionUtf8(t *testing.T) {
	tmp := t.TempDir()

	// Create a file with no extension but valid UTF-8
	err := os.WriteFile(filepath.Join(tmp, "README"), []byte("this is text"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	// Create a file with no extension but invalid UTF-8
	err = os.WriteFile(filepath.Join(tmp, "binary-no-ext"), []byte{0x00, 0x01, 0x02}, 0644)
	if err != nil {
		t.Fatal(err)
	}

	files, err := walkAndCollect(tmp)
	if err != nil {
		t.Fatal(err)
	}

	// Both files should be collected (extension check happens at read time)
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %v", len(files), files)
	}
}

func TestWalkAndCollect_nestedExcludedDirs(t *testing.T) {
	tmp := t.TempDir()

	// Create nested .git in a subdirectory
	subDir := filepath.Join(tmp, "subdir", ".git", "objects")
	err := os.MkdirAll(subDir, 0755)
	if err != nil {
		t.Fatal(err)
	}
	err = os.WriteFile(filepath.Join(subDir, "pack"), []byte("test"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	// Normal file at root
	err = os.WriteFile(filepath.Join(tmp, "root.txt"), []byte("root"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	// Normal file in subdir (not in .git)
	err = os.WriteFile(filepath.Join(tmp, "subdir", "normal.txt"), []byte("normal"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	files, err := walkAndCollect(tmp)
	if err != nil {
		t.Fatal(err)
	}

	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %v", len(files), files)
	}
}

func TestWalkAndCollect_csvTsv(t *testing.T) {
	tmp := t.TempDir()

	// CSV file
	err := os.WriteFile(filepath.Join(tmp, "data.csv"), []byte("a,b,c\n1,2,3\n4,5,6"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	// TSV file
	err = os.WriteFile(filepath.Join(tmp, "data.tsv"), []byte("a\tb\tc\n1\t2\t3"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	files, err := walkAndCollect(tmp)
	if err != nil {
		t.Fatal(err)
	}

	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d: %v", len(files), files)
	}
}

func TestWalkAndCollect_ymlExtension(t *testing.T) {
	tmp := t.TempDir()

	err := os.WriteFile(filepath.Join(tmp, "config.yml"), []byte("key: value"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	files, err := walkAndCollect(tmp)
	if err != nil {
		t.Fatal(err)
	}

	if len(files) != 1 {
		t.Fatalf("expected 1 file, got %d: %v", len(files), files)
	}
}

func TestStripHTML(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{
			"<p>Hello <b>world</b></p>",
			"Hello  world",
		},
		{
			"<html><head><title>Test</title></head><body><p>Content</p></body></html>",
			"Test    Content",
		},
		{
			"<div>\n  <span>Line 1</span>\n  <span>Line 2</span>\n</div>",
			"Line 1 \n   Line 2",
		},
		{
			"plain text without tags",
			"plain text without tags",
		},
	}

	for _, tt := range tests {
		result := stripHTML(tt.input)
		if result != tt.expected {
			t.Errorf("stripHTML(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}

func TestCsvToText(t *testing.T) {
	data := []byte("name,age,city\nAlice,30,NYC\nBob,25,SF")
	result, err := csvToText(data, ".csv")
	if err != nil {
		t.Fatal(err)
	}

	expected := "name age city\nAlice 30 NYC\nBob 25 SF"
	if result != expected {
		t.Errorf("csvToText = %q, want %q", result, expected)
	}
}

func TestCsvToText_tsv(t *testing.T) {
	data := []byte("name\tage\tcity\nAlice\t30\tNYC")
	result, err := csvToText(data, ".tsv")
	if err != nil {
		t.Fatal(err)
	}

	expected := "name age city\nAlice 30 NYC"
	if result != expected {
		t.Errorf("csvToText tsv = %q, want %q", result, expected)
	}
}

func TestSupportedExts(t *testing.T) {
	expectedExts := []string{
		".txt", ".md", ".pdf", ".html", ".htm", ".docx",
		".xlsx", ".csv", ".tsv", ".json", ".yaml", ".yml", ".eml",
	}
	for _, ext := range expectedExts {
		if !supportedExts[ext] {
			t.Errorf("expected %s to be supported", ext)
		}
	}
}

func TestExcludedBinaryExts(t *testing.T) {
	expectedBinary := []string{
		".so", ".dll", ".exe", ".bin", ".dat",
		".jpg", ".png", ".mp3", ".mp4", ".zip",
	}
	for _, ext := range expectedBinary {
		if !excludedBinaryExts[ext] {
			t.Errorf("expected %s to be excluded as binary", ext)
		}
	}
}

func TestExcludedDirs(t *testing.T) {
	expectedExcluded := []string{
		".git", "node_modules", "__pycache__",
		"venv", ".venv", "env", ".cache",
		"target", "dist", "build",
	}
	for _, dir := range expectedExcluded {
		if !excludedDirs[dir] {
			t.Errorf("expected %s to be excluded", dir)
		}
	}
}
