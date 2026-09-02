package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyArchive(t *testing.T) {
	tests := []struct {
		name      string
		platforms []string
		secret    string
		unused    bool
		nested    bool
		wantErr   bool
	}{
		{name: "valid", platforms: []string{"amd64", "arm64"}},
		{name: "valid buildx nested index", platforms: []string{"amd64", "arm64"}, nested: true},
		{name: "missing arm64", platforms: []string{"amd64"}, wantErr: true},
		{name: "secret in layer", platforms: []string{"amd64", "arm64"}, secret: "xta_0123456789abcdefghijklmnop", wantErr: true},
		{name: "unreferenced blob", platforms: []string{"amd64", "arm64"}, unused: true, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "layout.tar")
			writeOCIArchive(t, path, test.platforms, test.secret, test.unused, test.nested)
			err := verifyArchive(path, "agent")
			if (err != nil) != test.wantErr {
				t.Fatalf("verifyArchive() error = %v, want error %t", err, test.wantErr)
			}
		})
	}
}

func writeOCIArchive(t *testing.T, path string, platforms []string, secret string, unused, nested bool) {
	t.Helper()
	blobs := make(map[string][]byte)
	root := index{SchemaVersion: 2}
	for _, architecture := range platforms {
		config := imageConfig{Architecture: architecture, OS: "linux"}
		config.Config.User = "65532:65532"
		config.Config.Entrypoint = []string{"/usr/local/bin/xtunnel-agent"}
		config.Config.Cmd = []string{"run"}
		configBytes := mustJSON(t, config)
		configDescriptor := addBlob(blobs, "application/vnd.oci.image.config.v1+json", configBytes)
		layerDescriptor := addBlob(blobs, "application/vnd.oci.image.layer.v1.tar+gzip", gzipLayer(t, secret))
		manifestBytes := mustJSON(t, manifest{SchemaVersion: 2, Config: configDescriptor, Layers: []descriptor{layerDescriptor}})
		manifestDescriptor := addBlob(blobs, "application/vnd.oci.image.manifest.v1+json", manifestBytes)
		manifestDescriptor.Platform = &platform{OS: "linux", Architecture: architecture}
		root.Manifests = append(root.Manifests, manifestDescriptor)
	}
	if unused {
		addBlob(blobs, "application/vnd.oci.image.config.v1+json", []byte(`{"unused":true}`))
	}
	if nested {
		nestedDescriptor := addBlob(blobs, "application/vnd.oci.image.index.v1+json", mustJSON(t, root))
		root.Manifests = []descriptor{nestedDescriptor}
	}

	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	archive := tar.NewWriter(file)
	writeTarFile(t, archive, "oci-layout", []byte(`{"imageLayoutVersion":"1.0.0"}`))
	writeTarFile(t, archive, "index.json", mustJSON(t, root))
	for blobPath, content := range blobs {
		writeTarFile(t, archive, blobPath, content)
	}
	if err := archive.Close(); err != nil {
		t.Fatalf("close fixture tar: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close fixture file: %v", err)
	}
}

func TestScanLayerRejectsSecretMetadataAndDuplicateEntries(t *testing.T) {
	tests := []struct {
		name    string
		headers []*tar.Header
	}{
		{name: "secret metadata", headers: []*tar.Header{{Name: "xta_0123456789abcdefghijklmnop", Mode: 0o600}}},
		{name: "duplicate entries", headers: []*tar.Header{{Name: "same", Mode: 0o600}, {Name: "same", Mode: 0o600}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var encoded bytes.Buffer
			writer := tar.NewWriter(&encoded)
			for _, header := range test.headers {
				if err := writer.WriteHeader(header); err != nil {
					t.Fatalf("write layer header: %v", err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatalf("close layer: %v", err)
			}
			if err := scanLayer("application/vnd.oci.image.layer.v1.tar", bytes.NewReader(encoded.Bytes())); err == nil {
				t.Fatal("scanLayer() passed unsafe metadata")
			}
		})
	}
}

func TestScanLayerRejectsTrailingSecret(t *testing.T) {
	var encoded bytes.Buffer
	layer := tar.NewWriter(&encoded)
	writeTarFile(t, layer, "safe", []byte("safe"))
	if err := layer.Close(); err != nil {
		t.Fatalf("close layer: %v", err)
	}
	encoded.WriteString("xta_0123456789abcdefghijklmnop")
	if err := scanLayer("application/vnd.oci.image.layer.v1.tar", bytes.NewReader(encoded.Bytes())); err == nil {
		t.Fatal("scanLayer() accepted a secret after the tar end marker")
	}
}

func addBlob(blobs map[string][]byte, mediaType string, content []byte) descriptor {
	digest := sha256.Sum256(content)
	encoded := hex.EncodeToString(digest[:])
	blobs["blobs/sha256/"+encoded] = content
	return descriptor{MediaType: mediaType, Digest: "sha256:" + encoded, Size: int64(len(content))}
}

func gzipLayer(t *testing.T, content string) []byte {
	t.Helper()
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	layer := tar.NewWriter(gzipWriter)
	writeTarFile(t, layer, "usr/local/bin/xtunnel-agent", []byte(content))
	if err := layer.Close(); err != nil {
		t.Fatalf("close layer tar: %v", err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatalf("close layer gzip: %v", err)
	}
	return compressed.Bytes()
}

func writeTarFile(t *testing.T, archive *tar.Writer, name string, content []byte) {
	t.Helper()
	if err := archive.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(content))}); err != nil {
		t.Fatalf("write %s header: %v", name, err)
	}
	if _, err := archive.Write(content); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	content, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return content
}
