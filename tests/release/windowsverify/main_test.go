package main

import (
	"bytes"
	"crypto/sha256"
	"debug/pe"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"
)

func TestPERejectsMalformedWrongArchitectureAndDLL(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		machine, characteristics uint16
		valid                    bool
	}{
		{"amd64", pe.IMAGE_FILE_MACHINE_AMD64, pe.IMAGE_FILE_EXECUTABLE_IMAGE, true},
		{"arm64", pe.IMAGE_FILE_MACHINE_ARM64, pe.IMAGE_FILE_EXECUTABLE_IMAGE, false},
		{"i386", pe.IMAGE_FILE_MACHINE_I386, pe.IMAGE_FILE_EXECUTABLE_IMAGE, false},
		{"dll", pe.IMAGE_FILE_MACHINE_AMD64, pe.IMAGE_FILE_EXECUTABLE_IMAGE | pe.IMAGE_FILE_DLL, false},
		{"not_executable", pe.IMAGE_FILE_MACHINE_AMD64, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var content bytes.Buffer
			dos := make([]byte, 128)
			copy(dos, "MZ")
			binary.LittleEndian.PutUint32(dos[60:], 128)
			content.Write(dos)
			content.WriteString("PE\x00\x00")
			header := pe.FileHeader{Machine: tc.machine, SizeOfOptionalHeader: uint16(binary.Size(pe.OptionalHeader64{})), Characteristics: tc.characteristics}
			if err := binary.Write(&content, binary.LittleEndian, header); err != nil {
				t.Fatal(err)
			}
			if err := binary.Write(&content, binary.LittleEndian, pe.OptionalHeader64{Magic: 0x20b, NumberOfRvaAndSizes: 16}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "candidate.exe")
			if err := os.WriteFile(path, content.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			if err := verifyPE(path); (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
	path := filepath.Join(t.TempDir(), "malformed.exe")
	if err := os.WriteFile(path, []byte("not PE"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyPE(path); err == nil {
		t.Fatal("accepted malformed PE")
	}
}

func TestProductReportBindsVersionDigestAndBothModes(t *testing.T) {
	commit := strings.Repeat("a", 40)
	digest := strings.Repeat("b", 64)
	baseline := productReport{Commit: commit, ServerSHA256: digest, Version: "v0.1.0-ci." + commit, OS: "windows", Arch: "amd64", Modes: []string{"foreground", "scm"}}
	for _, tc := range []struct {
		name   string
		change func(*productReport)
		valid  bool
	}{
		{"valid", func(*productReport) {}, true},
		{"wrong_commit", func(r *productReport) { r.Commit = strings.Repeat("c", 40) }, false},
		{"wrong_digest", func(r *productReport) { r.ServerSHA256 = strings.Repeat("c", 64) }, false},
		{"wrong_version", func(r *productReport) { r.Version = "(devel)" }, false},
		{"missing_version", func(r *productReport) { r.Version = "" }, false},
		{"wrong_arch", func(r *productReport) { r.Arch = "arm64" }, false},
		{"wrong_os", func(r *productReport) { r.OS = "linux" }, false},
		{"missing_scm", func(r *productReport) { r.Modes = []string{"foreground"} }, false},
		{"duplicate_mode", func(r *productReport) { r.Modes = []string{"scm", "scm"} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := baseline
			tc.change(&r)
			content, err := json.Marshal(r)
			if err != nil {
				t.Fatal(err)
			}
			if err := verifyProductReport(content, commit, digest); (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
	if err := verifyProductReport([]byte(`{} {}`), commit, digest); err == nil {
		t.Fatal("accepted trailing JSON")
	}
}

func TestCandidateSecretScanAndDigest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "candidate.exe")
	content := []byte("safe candidate")
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	entry, err := scanAndHash(path)
	if err != nil {
		t.Fatal(err)
	}
	if entry.SHA256 != "b4eae859e053cd37755529fe9d5a440144b08c7f84a537ab991e78d9e0d9b451" || entry.Size != int64(len(content)) {
		t.Fatalf("invalid digest record: %#v", entry)
	}
	if err := os.WriteFile(path, []byte("xta_"+strings.Repeat("A", 24)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := scanAndHash(path); err == nil {
		t.Fatal("accepted forbidden token shape")
	}
}

func TestManifestRejectsTamperingAndUnscannedFiles(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "xtunnel-server-windows-amd64.exe")
	for _, name := range []string{"manifest.json", "artifact-sha256.txt", "product-report.json"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, []byte("candidate"), 0600); err != nil {
		t.Fatal(err)
	}
	entry, err := scanAndHash(path)
	if err != nil {
		t.Fatal(err)
	}
	report, err := scanAndHash(filepath.Join(directory, "product-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	result := manifest{Artifacts: []artifact{entry}, ProductReportSHA256: report.SHA256}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	checksum := fmt.Sprintf("%s  %s\n%s  product-report.json\n%x  manifest.json\n", entry.SHA256, entry.Name, report.SHA256, sha256.Sum256(encoded))
	for name, content := range map[string][]byte{"manifest.json": encoded, "artifact-sha256.txt": []byte(checksum)} {
		if err := os.WriteFile(filepath.Join(directory, name), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := verifyManifest(directory, result); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"manifest.json", "artifact-sha256.txt", "product-report.json"} {
		metadata := filepath.Join(directory, name)
		original, err := os.ReadFile(metadata)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(metadata, []byte("tampered"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := verifyManifest(directory, result); err == nil {
			t.Fatalf("accepted modified %s", name)
		}
		if err := os.WriteFile(metadata, original, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"../escape.exe", "unknown.exe", "xtunnel-agent-windows-amd64.exe", ""} {
		bad := result
		bad.Artifacts = []artifact{{Name: name}}
		if err := verifyManifest(directory, bad); err == nil {
			t.Fatalf("accepted invalid name %q", name)
		}
	}
	bad := result
	bad.Artifacts = append([]artifact{entry}, entry)
	if err := verifyManifest(directory, bad); err == nil {
		t.Fatal("accepted duplicate entry")
	}
	if err := os.WriteFile(path, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyManifest(directory, result); err == nil {
		t.Fatal("accepted candidate digest mismatch")
	}
	if err := os.WriteFile(path, []byte("candidate"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "unscanned"), []byte("extra"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyManifest(directory, result); err == nil {
		t.Fatal("accepted extra artifact")
	}
}

func TestBuildInfoRequiresExactCleanCommitWithTrimpath(t *testing.T) {
	commit := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name, key, value string
		valid            bool
	}{
		{"trimpath_without_ldflags", "", "", true},
		{"dirty", "vcs.modified", "true", false},
		{"revision", "vcs.revision", strings.Repeat("b", 40), false},
		{"arch", "GOARCH", "arm64", false},
		{"os", "GOOS", "linux", false},
		{"go_minor", "go", "go1.28.1", false},
		{"go_prerelease", "go", "go1.27rc1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			settings := map[string]string{"GOOS": "windows", "GOARCH": "amd64", "vcs.revision": commit, "vcs.modified": "false", "-trimpath": "true"}
			info := &debug.BuildInfo{GoVersion: "go1.27.1"}
			if tc.key == "go" {
				info.GoVersion = tc.value
			} else if tc.key != "" {
				settings[tc.key] = tc.value
			}
			for key, value := range settings {
				info.Settings = append(info.Settings, debug.BuildSetting{Key: key, Value: value})
			}
			if err := verifyBuildInfo(info, commit); (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}
