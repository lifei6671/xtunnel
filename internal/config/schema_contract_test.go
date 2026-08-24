package config_test

import (
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"testing"

	configschemas "github.com/lifei6671/xtunnel/configs"
	agentconfig "github.com/lifei6671/xtunnel/internal/agent/config"
	baseconfig "github.com/lifei6671/xtunnel/internal/config"
	serverconfig "github.com/lifei6671/xtunnel/internal/server/config"
)

func TestSchemaAndGoStructFieldsMatch(t *testing.T) {
	tests := []struct {
		name       string
		schemaData []byte
		configType reflect.Type
	}{
		{name: "server", schemaData: configschemas.ServerSchema(), configType: reflect.TypeFor[serverconfig.Config]()},
		{name: "agent", schemaData: configschemas.AgentSchema(), configType: reflect.TypeFor[agentconfig.Config]()},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			schemaFields := schemaLeafTypes(t, test.schemaData)
			structFields := structLeafTypes(test.configType, "")
			if !maps.Equal(schemaFields, structFields) {
				t.Fatalf("Schema fields = %#v\nGo struct fields = %#v", schemaFields, structFields)
			}
		})
	}
}

func schemaLeafTypes(t *testing.T, data []byte) map[string]string {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("json.Unmarshal(schema) error = %v", err)
	}

	result := make(map[string]string)
	var walk func(map[string]any, string)
	walk = func(current map[string]any, parent string) {
		properties, ok := current["properties"].(map[string]any)
		if !ok {
			t.Fatalf("object %q has no properties", parent)
		}
		for name, raw := range properties {
			property := raw.(map[string]any)
			path := name
			if parent != "" {
				path = parent + "." + name
			}
			typeName, _ := property["type"].(string)
			if typeName == "object" {
				walk(property, path)
				continue
			}
			if _, ok := property["x-secret"].(bool); !ok {
				t.Errorf("Schema field %s has no boolean x-secret", path)
			}
			if reloadable, ok := property["x-reloadable"].(bool); !ok || reloadable {
				t.Errorf("Schema field %s must declare x-reloadable=false", path)
			}
			if property["format"] == "go-duration" {
				typeName = "duration"
			}
			result[path] = typeName
		}
	}
	walk(root, "")
	return result
}

func structLeafTypes(current reflect.Type, parent string) map[string]string {
	result := make(map[string]string)
	for index := range current.NumField() {
		field := current.Field(index)
		name := field.Tag.Get("json")
		path := name
		if parent != "" {
			path = parent + "." + name
		}

		typeName, object := goTypeName(field.Type)
		if object {
			maps.Copy(result, structLeafTypes(field.Type, path))
			continue
		}
		result[path] = typeName
	}
	return result
}

func goTypeName(current reflect.Type) (string, bool) {
	if current == reflect.TypeFor[baseconfig.Duration]() {
		return "duration", false
	}
	switch current.Kind() {
	case reflect.Struct:
		return "", true
	case reflect.String:
		return "string", false
	case reflect.Int:
		return "integer", false
	case reflect.Float64:
		return "number", false
	case reflect.Slice:
		return "array", false
	default:
		return fmt.Sprintf("unsupported:%s", current), false
	}
}
