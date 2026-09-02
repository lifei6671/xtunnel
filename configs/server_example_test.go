package configs_test

import (
	"encoding/json"
	"maps"
	"os"
	"path"
	"strings"
	"testing"

	configschemas "github.com/lifei6671/xtunnel/configs"
	baseconfig "github.com/lifei6671/xtunnel/internal/config"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
	"go.yaml.in/yaml/v3"
)

func TestServerExampleCoversSchemaAndLoads(t *testing.T) {
	data, err := os.ReadFile("server.example.yaml")
	if err != nil {
		t.Fatalf("read Server example: %v", err)
	}

	var example map[string]any
	if err := yaml.Unmarshal(data, &example); err != nil {
		t.Fatalf("decode Server example: %v", err)
	}
	serverSection, ok := example["server"].(map[string]any)
	if !ok {
		t.Fatal("Server example has no server object")
	}
	dataDir, ok := serverSection["data_dir"].(string)
	if !ok || !path.IsAbs(dataDir) {
		t.Fatalf("Server example server.data_dir = %v, want an absolute Linux path", serverSection["data_dir"])
	}

	// 示例面向 Linux；Windows 测试宿主使用临时绝对路径覆盖，但先独立验证原始 POSIX 路径。
	if _, err := serverconfig.Load(baseconfig.Options{
		YAML: data,
		CLI:  map[string]string{"server.data_dir": t.TempDir()},
	}); err != nil {
		t.Fatalf("load Server example: %v", err)
	}
	exampleFields := yamlLeafPaths(example, "")
	schemaFields := schemaLeafPaths(t, configschemas.ServerSchema())

	// pinned 模式禁止同时出现 public TLS 文件字段；示例以注释展示切换方法。
	delete(schemaFields, "agent_gateway.tls.cert_file")
	delete(schemaFields, "agent_gateway.tls.key_file")
	if !maps.Equal(exampleFields, schemaFields) {
		t.Fatalf("Server example fields = %#v\nSchema fields valid for pinned mode = %#v", exampleFields, schemaFields)
	}
	for _, optional := range []string{"# cert_file:", "# key_file:"} {
		if !strings.Contains(string(data), optional) {
			t.Fatalf("Server example does not document public TLS field %q", optional)
		}
	}
}

func TestAgentBootstrapExampleDeclaresOnlyTokenInput(t *testing.T) {
	data, err := os.ReadFile("agent-bootstrap.env.example")
	if err != nil {
		t.Fatalf("read Agent Bootstrap example: %v", err)
	}

	var assignments []string
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, _, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("Agent Bootstrap example line is not an environment assignment: %q", line)
		}
		assignments = append(assignments, name)
	}
	if len(assignments) != 1 || assignments[0] != "XTUNNEL_TOKEN" {
		t.Fatalf("Agent Bootstrap example assignments = %v, want only XTUNNEL_TOKEN", assignments)
	}
	if !strings.Contains(string(data), "不会自动读取本文件") {
		t.Fatal("Agent Bootstrap example must state that Agent does not automatically load it")
	}
}

func schemaLeafPaths(t *testing.T, data []byte) map[string]struct{} {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("decode Server Schema: %v", err)
	}
	result := make(map[string]struct{})
	var walk func(map[string]any, string)
	walk = func(current map[string]any, parent string) {
		properties, ok := current["properties"].(map[string]any)
		if !ok {
			t.Fatalf("Schema object %q has no properties", parent)
		}
		for name, raw := range properties {
			property, ok := raw.(map[string]any)
			if !ok {
				t.Fatalf("Schema field %q is not an object", name)
			}
			path := joinPath(parent, name)
			if property["type"] == "object" {
				walk(property, path)
				continue
			}
			result[path] = struct{}{}
		}
	}
	walk(root, "")
	return result
}

func yamlLeafPaths(current map[string]any, parent string) map[string]struct{} {
	result := make(map[string]struct{})
	for name, value := range current {
		path := joinPath(parent, name)
		if child, ok := value.(map[string]any); ok {
			maps.Copy(result, yamlLeafPaths(child, path))
			continue
		}
		result[path] = struct{}{}
	}
	return result
}

func joinPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "." + name
}
