package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/pe"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/lifei6671/xtunnel/tests/release/internal/secretcheck"
)

var stableGo = regexp.MustCompile(`^go1\.27\.[1-9][0-9]*$`)
var fullCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

type artifact struct {
	Name      string `json:"name"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	GoVersion string `json:"go_version"`
	Version   string `json:"version"`
}
type manifest struct {
	Commit              string     `json:"commit"`
	Platform            string     `json:"platform"`
	ProductReportSHA256 string     `json:"product_report_sha256"`
	Artifacts           []artifact `json:"artifacts"`
}

func main() {
	server := flag.String("server", "", "prebuilt Server executable")
	commit := flag.String("commit", "", "full Git commit SHA")
	output := flag.String("output", "", "new directory outside repository")
	report := flag.String("product-report", "", "successful commit-bound foreground and SCM report")
	flag.Parse()
	if err := run(*server, *commit, *output, *report); err != nil {
		fmt.Fprintf(os.Stderr, "Windows candidate verification failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("Windows amd64 candidate verification passed")
}

func run(server, commit, output, report string) error {
	if runtime.GOOS != "windows" || runtime.GOARCH != "amd64" || !stableGo.MatchString(runtime.Version()) || os.Getenv("GOTOOLCHAIN") != "local" {
		return errors.New("requires native Windows amd64 Go 1.27.1+ with GOTOOLCHAIN=local")
	}
	if server == "" || output == "" || report == "" || !fullCommit.MatchString(commit) {
		return errors.New("server, full commit, output and product-report are required")
	}
	root, err := commandOutput("git", "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	head, err := commandOutput("git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	status, err := commandOutput("git", "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return err
	}
	if head != commit || status != "" {
		return errors.New("candidate requires a clean checkout at the exact full commit")
	}
	output, err = filepath.Abs(output)
	if err != nil {
		return err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(output))
	if err != nil {
		return err
	}
	output = filepath.Join(parent, filepath.Base(output))
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(root, output)
	if err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("output must be outside the repository")
	}
	if err := os.Mkdir(output, 0700); err != nil {
		return fmt.Errorf("create fresh candidate directory: %w", err)
	}
	result := manifest{Commit: commit, Platform: "windows/amd64"}
	path := filepath.Join(output, "xtunnel-server-windows-amd64.exe")
	if err := copyCandidate(server, path); err != nil {
		return err
	}
	entry, err := verifyBinary(path, commit)
	if err != nil {
		return fmt.Errorf("verify Server: %w", err)
	}
	result.Artifacts = []artifact{entry}
	reportContent, err := os.ReadFile(report)
	if err != nil {
		return err
	}
	if err := verifyProductReport(reportContent, commit, result.Artifacts[0].SHA256); err != nil {
		return err
	}
	// 双模式报告已核验版本与本次复制后的字节，才将版本写入产物元数据。
	result.Artifacts[0].Version = "v0.1.0-ci." + commit
	if err := secretcheck.Bytes("product-report.json", reportContent); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(output, "product-report.json"), reportContent, 0600); err != nil {
		return err
	}
	reportDigest := sha256.Sum256(reportContent)
	result.ProductReportSHA256 = hex.EncodeToString(reportDigest[:])
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if err := secretcheck.Bytes("manifest.json", encoded); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(output, "manifest.json"), encoded, 0600); err != nil {
		return err
	}
	var lines strings.Builder
	for _, entry := range result.Artifacts {
		fmt.Fprintf(&lines, "%s  %s\n", entry.SHA256, entry.Name)
	}
	fmt.Fprintf(&lines, "%s  product-report.json\n", result.ProductReportSHA256)
	digest := sha256.Sum256(encoded)
	fmt.Fprintf(&lines, "%x  manifest.json\n", digest)
	if err := os.WriteFile(filepath.Join(output, "artifact-sha256.txt"), []byte(lines.String()), 0600); err != nil {
		return err
	}
	return verifyManifest(output, result)
}

func commandOutput(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	content, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return "", fmt.Errorf("%s command failed: %w", name, err)
	}
	return strings.TrimSpace(string(content)), nil
}

func copyCandidate(source, destination string) (resultErr error) {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("candidate input is not a regular file")
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, input.Close()) }()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, output.Close()) }()
	if _, err := io.Copy(output, input); err != nil {
		return err
	}
	return output.Sync()
}

func verifyPE(path string) (resultErr error) {
	file, err := pe.Open(path)
	if err != nil {
		return fmt.Errorf("parse PE: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	if file.Machine != pe.IMAGE_FILE_MACHINE_AMD64 || file.Characteristics&pe.IMAGE_FILE_EXECUTABLE_IMAGE == 0 || file.Characteristics&pe.IMAGE_FILE_DLL != 0 {
		return errors.New("candidate is not an amd64 executable PE")
	}
	if _, ok := file.OptionalHeader.(*pe.OptionalHeader64); !ok {
		return errors.New("candidate is not a PE32+ executable")
	}
	return nil
}

func verifyBinary(path, commit string) (artifact, error) {
	var entry artifact
	if err := verifyPE(path); err != nil {
		return entry, err
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return entry, fmt.Errorf("read Go build metadata: %w", err)
	}
	if err := verifyBuildInfo(info, commit); err != nil {
		return entry, err
	}
	entry, err = scanAndHash(path, secretcheck.WindowsServerBinaryReader)
	if err != nil {
		return entry, err
	}
	entry.GoVersion = info.GoVersion
	return entry, nil
}

// verifyBuildInfo 校验编译来源；trimpath 构建的版本由绑定文件摘要的 SystemInfo 报告验证。
func verifyBuildInfo(info *debug.BuildInfo, commit string) error {
	if !stableGo.MatchString(info.GoVersion) {
		return errors.New("candidate uses an unsupported Go version")
	}
	settings := make(map[string]string, len(info.Settings))
	for _, setting := range info.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["GOOS"] != "windows" || settings["GOARCH"] != "amd64" || settings["vcs.revision"] != commit || settings["vcs.modified"] != "false" {
		return errors.New("candidate Go platform or clean VCS identity does not match commit")
	}
	return nil
}

// productReport 是真实产品双模式测试写出的只读验收输入。
type productReport struct {
	Commit       string   `json:"commit"`
	ServerSHA256 string   `json:"server_sha256"`
	Version      string   `json:"version"`
	OS           string   `json:"os"`
	Arch         string   `json:"arch"`
	Modes        []string `json:"modes"`
}

func verifyProductReport(content []byte, commit, digest string) error {
	var report productReport
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&report); err != nil {
		return fmt.Errorf("parse product report: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("product report has trailing content")
	}
	if report.Commit != commit || report.ServerSHA256 != digest || report.Version != "v0.1.0-ci."+commit || report.OS != "windows" || report.Arch != "amd64" {
		return errors.New("product report does not match candidate identity")
	}
	if len(report.Modes) != 2 || !((report.Modes[0] == "foreground" && report.Modes[1] == "scm") || (report.Modes[0] == "scm" && report.Modes[1] == "foreground")) {
		return errors.New("product report requires both foreground and SCM modes")
	}
	return nil
}
func scanAndHash(path string, scan func(string, io.Reader) error) (entry artifact, resultErr error) {
	before, err := os.Lstat(path)
	if err != nil {
		return entry, err
	}
	if !before.Mode().IsRegular() {
		return entry, errors.New("candidate is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return entry, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return entry, err
	}
	if !info.Mode().IsRegular() || !os.SameFile(before, info) {
		return entry, errors.New("candidate is not a regular file")
	}
	hash := sha256.New()
	if err := scan(filepath.Base(path), io.TeeReader(file, hash)); err != nil {
		return entry, err
	}
	return artifact{Name: filepath.Base(path), SHA256: hex.EncodeToString(hash.Sum(nil)), Size: info.Size()}, nil
}

// verifyManifest 拒绝内容漂移或额外未扫描文件；摘要绑定真实产品报告验证的最终副本。
func verifyManifest(directory string, result manifest) error {
	allowed := map[string]bool{"manifest.json": true, "artifact-sha256.txt": true, "product-report.json": true}
	for _, entry := range result.Artifacts {
		if entry.Name != "xtunnel-server-windows-amd64.exe" || allowed[entry.Name] {
			return errors.New("duplicate or invalid manifest artifact")
		}
		allowed[entry.Name] = true
		actual, err := scanAndHash(filepath.Join(directory, entry.Name), secretcheck.WindowsServerBinaryReader)
		if err != nil {
			return err
		}
		if actual.SHA256 != entry.SHA256 || actual.Size != entry.Size {
			return errors.New("candidate manifest digest or size mismatch")
		}
	}
	if !allowed["xtunnel-server-windows-amd64.exe"] {
		return errors.New("Server artifact is missing")
	}
	report, err := scanAndHash(filepath.Join(directory, "product-report.json"), secretcheck.Reader)
	if err != nil {
		return err
	}
	if report.SHA256 != result.ProductReportSHA256 {
		return errors.New("product report digest mismatch")
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	var checksums strings.Builder
	for _, entry := range result.Artifacts {
		fmt.Fprintf(&checksums, "%s  %s\n", entry.SHA256, entry.Name)
	}
	fmt.Fprintf(&checksums, "%s  product-report.json\n", result.ProductReportSHA256)
	fmt.Fprintf(&checksums, "%x  manifest.json\n", sha256.Sum256(encoded))
	for name, expected := range map[string][]byte{"manifest.json": encoded, "artifact-sha256.txt": []byte(checksums.String())} {
		actual, err := scanAndHash(filepath.Join(directory, name), secretcheck.Reader)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(expected)
		if actual.SHA256 != hex.EncodeToString(digest[:]) {
			return errors.New("candidate metadata readback mismatch")
		}
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	if len(entries) != len(allowed) {
		return errors.New("candidate contains missing or extra files")
	}
	for _, entry := range entries {
		if entry.IsDir() || !allowed[entry.Name()] {
			return errors.New("candidate contains an unverified file")
		}
	}
	return nil
}
