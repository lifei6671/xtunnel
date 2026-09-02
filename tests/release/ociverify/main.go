package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/lifei6671/xtunnel/tests/release/internal/secretcheck"
)

type descriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Size        int64             `json:"size"`
	Platform    *platform         `json:"platform,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type platform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
}

type index struct {
	SchemaVersion int          `json:"schemaVersion"`
	Manifests     []descriptor `json:"manifests"`
}

type manifest struct {
	SchemaVersion int          `json:"schemaVersion"`
	Config        descriptor   `json:"config"`
	Layers        []descriptor `json:"layers"`
}

type imageConfig struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
	Config       struct {
		User       string              `json:"User"`
		Entrypoint []string            `json:"Entrypoint"`
		Cmd        []string            `json:"Cmd"`
		Env        []string            `json:"Env"`
		Volumes    map[string]struct{} `json:"Volumes"`
	} `json:"config"`
}

type layoutFiles struct {
	content    map[string][]byte
	blobSizes  map[string]int64
	referenced map[string]bool
}

func main() {
	archivePath := flag.String("archive", "", "OCI layout tar archive")
	target := flag.String("target", "", "expected XTunnel target: server or agent")
	flag.Parse()
	if *archivePath == "" || (*target != "server" && *target != "agent") {
		fmt.Fprintln(os.Stderr, "usage: ociverify -archive FILE -target server|agent")
		os.Exit(2)
	}
	if err := verifyArchive(*archivePath, *target); err != nil {
		fmt.Fprintf(os.Stderr, "OCI release verification failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("OCI release verification passed: target=%s platforms=linux/amd64,linux/arm64\n", *target)
}

func verifyArchive(path, target string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat archive: %w", err)
	}
	if stat.Size() <= 0 || stat.Size() > 2<<30 {
		return fmt.Errorf("archive size %d is outside 1 byte through 2 GiB", stat.Size())
	}
	files, err := readTarMetadata(file)
	if err != nil {
		return fmt.Errorf("read OCI layout: %w", err)
	}
	var layout struct {
		Version string `json:"imageLayoutVersion"`
	}
	if err := scanSecrets("oci-layout", files.content["oci-layout"]); err != nil {
		return err
	}
	if err := json.Unmarshal(files.content["oci-layout"], &layout); err != nil || layout.Version != "1.0.0" {
		return fmt.Errorf("invalid oci-layout version")
	}
	var root index
	if err := scanSecrets("index.json", files.content["index.json"]); err != nil {
		return err
	}
	if err := json.Unmarshal(files.content["index.json"], &root); err != nil {
		return fmt.Errorf("decode index.json: %w", err)
	}
	platformManifests, err := resolvePlatformManifests(files, root)
	if err != nil {
		return err
	}
	want := map[string]bool{"amd64": false, "arm64": false}
	for _, item := range platformManifests {
		if item.MediaType != "application/vnd.oci.image.manifest.v1+json" {
			return fmt.Errorf("unexpected root descriptor media type %q", item.MediaType)
		}
		if item.Platform == nil || item.Platform.OS != "linux" {
			return fmt.Errorf("root descriptor is missing a Linux platform")
		}
		if _, expected := want[item.Platform.Architecture]; !expected {
			return fmt.Errorf("unexpected Linux platform %q", item.Platform.Architecture)
		}
		if want[item.Platform.Architecture] {
			return fmt.Errorf("duplicate linux/%s manifest", item.Platform.Architecture)
		}
		manifestBytes, err := descriptorBytes(files, item)
		if err != nil {
			return fmt.Errorf("read linux/%s manifest: %w", item.Platform.Architecture, err)
		}
		var imageManifest manifest
		if err := scanSecrets("image manifest", manifestBytes); err != nil {
			return err
		}
		if err := json.Unmarshal(manifestBytes, &imageManifest); err != nil {
			return fmt.Errorf("decode linux/%s manifest: %w", item.Platform.Architecture, err)
		}
		if imageManifest.SchemaVersion != 2 || imageManifest.Config.MediaType != "application/vnd.oci.image.config.v1+json" {
			return fmt.Errorf("linux/%s manifest has an invalid schema or config media type", item.Platform.Architecture)
		}
		configBytes, err := descriptorBytes(files, imageManifest.Config)
		if err != nil {
			return fmt.Errorf("read linux/%s config: %w", item.Platform.Architecture, err)
		}
		if err := scanSecrets("image config", configBytes); err != nil {
			return err
		}
		var config imageConfig
		if err := json.Unmarshal(configBytes, &config); err != nil {
			return fmt.Errorf("decode linux/%s config: %w", item.Platform.Architecture, err)
		}
		if err := verifyConfig(config, item.Platform.Architecture, target); err != nil {
			return err
		}
		for layerIndex, layer := range imageManifest.Layers {
			if layer.MediaType != "application/vnd.oci.image.layer.v1.tar+gzip" && layer.MediaType != "application/vnd.oci.image.layer.v1.tar" {
				return fmt.Errorf("linux/%s layer %d has unsupported media type %q", item.Platform.Architecture, layerIndex, layer.MediaType)
			}
			if _, err := markDescriptor(files, layer); err != nil {
				return fmt.Errorf("validate linux/%s layer %d: %w", item.Platform.Architecture, layerIndex, err)
			}
			if err := scanArchivedLayer(file, layer); err != nil {
				return fmt.Errorf("scan linux/%s layer %d: %w", item.Platform.Architecture, layerIndex, err)
			}
		}
		want[item.Platform.Architecture] = true
	}
	for architecture, found := range want {
		if !found {
			return fmt.Errorf("missing linux/%s manifest", architecture)
		}
	}
	for path := range files.blobSizes {
		if !files.referenced[path] {
			return fmt.Errorf("unreferenced OCI blob %s", path)
		}
	}
	return nil
}

// resolvePlatformManifests 接受 OCI Layout 的两种等价结构：平台 Manifest
// 直接挂在根索引下，或由 Buildx 先生成一个带名称注解的单层子索引。只允许这一层
// 间接结构，避免递归索引掩盖额外平台、证明清单或未受验证的对象。
func resolvePlatformManifests(files *layoutFiles, root index) ([]descriptor, error) {
	if root.SchemaVersion != 2 {
		return nil, fmt.Errorf("root index must use schemaVersion 2")
	}
	if len(root.Manifests) == 2 {
		return root.Manifests, nil
	}
	if len(root.Manifests) != 1 || root.Manifests[0].MediaType != "application/vnd.oci.image.index.v1+json" {
		return nil, fmt.Errorf("root index must contain two platform manifests or one nested image index")
	}
	nestedBytes, err := descriptorBytes(files, root.Manifests[0])
	if err != nil {
		return nil, fmt.Errorf("read nested image index: %w", err)
	}
	if err := scanSecrets("nested image index", nestedBytes); err != nil {
		return nil, err
	}
	var nested index
	if err := json.Unmarshal(nestedBytes, &nested); err != nil {
		return nil, fmt.Errorf("decode nested image index: %w", err)
	}
	if nested.SchemaVersion != 2 || len(nested.Manifests) != 2 {
		return nil, fmt.Errorf("nested image index must contain schemaVersion 2 and exactly two manifests")
	}
	return nested.Manifests, nil
}

func readTarMetadata(file *os.File) (*layoutFiles, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	result := &layoutFiles{
		content: make(map[string][]byte), blobSizes: make(map[string]int64), referenced: make(map[string]bool),
	}
	seen := make(map[string]struct{})
	archive := tar.NewReader(file)
	entries := 0
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return result, nil
		}
		if err != nil {
			return nil, err
		}
		entries++
		if entries > 1_000_000 {
			return nil, fmt.Errorf("OCI archive contains more than 1000000 entries")
		}
		if len(header.Name) > 4096 {
			return nil, fmt.Errorf("OCI archive entry name exceeds 4096 bytes")
		}
		if _, exists := seen[header.Name]; exists {
			return nil, fmt.Errorf("duplicate OCI archive entry %s", header.Name)
		}
		seen[header.Name] = struct{}{}
		if err := secretcheck.Bytes("OCI archive entry name", []byte(header.Name)); err != nil {
			return nil, err
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return nil, fmt.Errorf("unsupported OCI archive object %s type %d", header.Name, header.Typeflag)
		}
		if strings.HasPrefix(header.Name, "blobs/sha256/") {
			expected := strings.TrimPrefix(header.Name, "blobs/sha256/")
			if len(expected) != sha256.Size*2 {
				return nil, fmt.Errorf("invalid blob path %s", header.Name)
			}
			hash := sha256.New()
			var capture bytes.Buffer
			writer := io.Writer(hash)
			if header.Size <= 8<<20 {
				writer = io.MultiWriter(hash, &capture)
			}
			written, err := io.Copy(writer, archive)
			if err != nil || written != header.Size {
				return nil, fmt.Errorf("read %s: bytes=%d want=%d error=%w", header.Name, written, header.Size, err)
			}
			if hex.EncodeToString(hash.Sum(nil)) != expected {
				return nil, fmt.Errorf("blob path digest mismatch: %s", header.Name)
			}
			if header.Size <= 8<<20 {
				result.content[header.Name] = capture.Bytes()
			}
			result.blobSizes[header.Name] = header.Size
			continue
		}
		if header.Name != "index.json" && header.Name != "oci-layout" {
			return nil, fmt.Errorf("unexpected OCI archive file %s", header.Name)
		}
		if header.Size > 8<<20 {
			return nil, fmt.Errorf("metadata %s exceeds 8 MiB", header.Name)
		}
		content, err := io.ReadAll(archive)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", header.Name, err)
		}
		result.content[header.Name] = content
	}
}

func descriptorBytes(files *layoutFiles, item descriptor) ([]byte, error) {
	path, err := markDescriptor(files, item)
	if err != nil {
		return nil, err
	}
	content, exists := files.content[path]
	if !exists {
		return nil, fmt.Errorf("blob %s exceeds the 8 MiB metadata limit", item.Digest)
	}
	digest := sha256.Sum256(content)
	_, encoded, _ := strings.Cut(item.Digest, ":")
	if hex.EncodeToString(digest[:]) != encoded {
		return nil, fmt.Errorf("blob %s content digest mismatch", item.Digest)
	}
	return content, nil
}

func markDescriptor(files *layoutFiles, item descriptor) (string, error) {
	algorithm, encoded, ok := strings.Cut(item.Digest, ":")
	if !ok || algorithm != "sha256" || len(encoded) != sha256.Size*2 {
		return "", fmt.Errorf("unsupported digest %q", item.Digest)
	}
	path := "blobs/sha256/" + encoded
	size, exists := files.blobSizes[path]
	if !exists {
		return "", fmt.Errorf("missing blob %s", item.Digest)
	}
	if size != item.Size {
		return "", fmt.Errorf("blob %s size = %d, want %d", item.Digest, size, item.Size)
	}
	files.referenced[path] = true
	return path, nil
}

func verifyConfig(config imageConfig, architecture, target string) error {
	if config.OS != "linux" || config.Architecture != architecture {
		return fmt.Errorf("config platform = %s/%s, want linux/%s", config.OS, config.Architecture, architecture)
	}
	if config.Config.User != "65532:65532" {
		return fmt.Errorf("linux/%s user = %q", architecture, config.Config.User)
	}
	wantEntrypoint := "/usr/local/bin/xtunnel-" + target
	if len(config.Config.Entrypoint) != 1 || config.Config.Entrypoint[0] != wantEntrypoint {
		return fmt.Errorf("linux/%s entrypoint = %v, want [%s]", architecture, config.Config.Entrypoint, wantEntrypoint)
	}
	if target == "agent" {
		if len(config.Config.Cmd) != 1 || config.Config.Cmd[0] != "run" {
			return fmt.Errorf("linux/%s Agent command = %v, want [run]", architecture, config.Config.Cmd)
		}
		if len(config.Config.Volumes) != 0 {
			return fmt.Errorf("linux/%s Agent image declares volumes", architecture)
		}
	} else if _, exists := config.Config.Volumes["/var/lib/xtunnel"]; !exists {
		return fmt.Errorf("linux/%s Server image is missing /var/lib/xtunnel volume", architecture)
	}
	for _, environment := range config.Config.Env {
		name, value, _ := strings.Cut(environment, "=")
		upperName := strings.ToUpper(name)
		if value != "" && (strings.Contains(upperName, "TOKEN") || strings.Contains(upperName, "PASSWORD") || strings.Contains(upperName, "COOKIE") || strings.Contains(upperName, "PRIVATE_KEY")) {
			return fmt.Errorf("linux/%s image embeds secret-like environment %q", architecture, name)
		}
	}
	return nil
}

func scanArchivedLayer(file *os.File, item descriptor) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	path := "blobs/sha256/" + strings.TrimPrefix(item.Digest, "sha256:")
	archive := tar.NewReader(file)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("missing layer %s", item.Digest)
		}
		if err != nil {
			return err
		}
		if header.Name != path {
			continue
		}
		if header.Size != item.Size {
			return fmt.Errorf("layer %s size = %d, want %d", item.Digest, header.Size, item.Size)
		}
		return scanLayer(item.MediaType, archive)
	}
}

func scanLayer(mediaType string, reader io.Reader) error {
	if strings.HasSuffix(mediaType, "+gzip") {
		compressed, err := gzip.NewReader(reader)
		if err != nil {
			return fmt.Errorf("open gzip layer: %w", err)
		}
		defer compressed.Close()
		reader = compressed
	} else if !strings.HasSuffix(mediaType, ".tar") {
		return fmt.Errorf("unsupported layer media type %q", mediaType)
	}
	// Tar Header、PAX 与文件填充同样计入展开预算，避免大量非普通条目绕过
	// 逐文件限制形成压缩炸弹。额外 64 MiB 只覆盖合法 Header/填充开销。
	limited := &io.LimitedReader{R: reader, N: (2 << 30) + (64 << 20) + 1}
	layer := tar.NewReader(limited)
	var expanded int64
	var entries int
	seen := make(map[string]struct{})
	for {
		header, err := layer.Next()
		if errors.Is(err, io.EOF) {
			// tar 的两个零块只结束归档语义，不保证外层解压流也结束。继续扫描
			// 尾随字节，同时迫使 gzip 校验 Footer 和后续 Member。
			if err := secretcheck.Reader("image layer trailing data", limited); err != nil {
				return err
			}
			if limited.N == 0 {
				return fmt.Errorf("expanded layer stream exceeds 2112 MiB")
			}
			return nil
		}
		if err != nil {
			return err
		}
		entries++
		if entries > 1_000_000 {
			return fmt.Errorf("layer contains more than 1000000 entries")
		}
		if len(header.Name) > 4096 || len(header.Linkname) > 4096 {
			return fmt.Errorf("layer entry metadata path exceeds 4096 bytes")
		}
		if _, exists := seen[header.Name]; exists {
			return fmt.Errorf("duplicate layer entry %s", header.Name)
		}
		seen[header.Name] = struct{}{}
		metadata := header.Name + "\n" + header.Linkname
		for key, value := range header.PAXRecords {
			metadata += "\n" + key + "=" + value
			if len(metadata) > 1<<20 {
				return fmt.Errorf("layer entry metadata exceeds 1 MiB")
			}
		}
		if err := secretcheck.Bytes("image layer metadata", []byte(metadata)); err != nil {
			return err
		}
		if header.Size < 0 || header.Size > 512<<20 {
			return fmt.Errorf("layer file %s size %d exceeds 512 MiB", header.Name, header.Size)
		}
		expanded += header.Size
		if expanded > 2<<30 {
			return fmt.Errorf("expanded layer exceeds 2 GiB")
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			continue
		}
		if err := secretcheck.Reader("image file "+header.Name, layer); err != nil {
			return err
		}
	}
}

func scanSecrets(source string, content []byte) error {
	return secretcheck.Bytes(source, content)
}
