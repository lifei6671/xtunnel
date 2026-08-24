package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"go.yaml.in/yaml/v3"
)

const environmentPrefix = "XTUNNEL_"

// Options 包含四层配置源。Environment 使用 KEY=VALUE；CLI Key 使用
// "management.public_url" 这类点分 Schema 路径。
type Options struct {
	YAML        []byte
	Environment []string
	CLI         map[string]string
}

// Load 按 Schema Default、YAML、Environment、CLI 的顺序覆盖，随后用 Schema
// 校验最终对象并解码为 T。
func Load[T any](schemaData []byte, options Options, validate func(*T) error) (T, error) {
	var zero T

	schemaMap, compiled, err := compileSchema(schemaData)
	if err != nil {
		return zero, err
	}

	merged := defaultsFromSchema(schemaMap)
	yamlValues, err := decodeYAML(options.YAML)
	if err != nil {
		return zero, err
	}
	mergeMaps(merged, yamlValues)

	fields, err := collectFields(schemaMap)
	if err != nil {
		return zero, fmt.Errorf("index config schema fields: %w", err)
	}
	if err := applyEnvironment(merged, fields, options.Environment); err != nil {
		return zero, err
	}
	if err := applyCLI(merged, fields, options.CLI); err != nil {
		return zero, err
	}

	encoded, err := json.Marshal(merged)
	if err != nil {
		return zero, fmt.Errorf("encode merged config: %w", err)
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return zero, fmt.Errorf("decode merged config for schema validation: %w", err)
	}
	if err := compiled.Validate(instance); err != nil {
		return zero, fmt.Errorf("validate merged config: %w", err)
	}

	var result T
	if err := json.Unmarshal(encoded, &result); err != nil {
		return zero, fmt.Errorf("decode validated config: %w", err)
	}
	if validate != nil {
		if err := validate(&result); err != nil {
			return zero, fmt.Errorf("validate config relationships: %w", err)
		}
	}
	return result, nil
}

func compileSchema(data []byte) (map[string]any, *jsonschema.Schema, error) {
	var schemaMap map[string]any
	if err := json.Unmarshal(data, &schemaMap); err != nil {
		return nil, nil, fmt.Errorf("decode config schema metadata: %w", err)
	}

	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, nil, fmt.Errorf("decode config schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	compiler.RegisterFormat(&jsonschema.Format{
		Name: "go-duration",
		Validate: func(value any) error {
			text, ok := value.(string)
			if !ok {
				return nil
			}
			if _, err := time.ParseDuration(text); err != nil {
				return jsonschema.LocalizableError("invalid Go duration")
			}
			return nil
		},
	})
	const schemaURL = "schema.json"
	if err := compiler.AddResource(schemaURL, document); err != nil {
		return nil, nil, fmt.Errorf("register config schema: %w", err)
	}
	compiled, err := compiler.Compile(schemaURL)
	if err != nil {
		return nil, nil, fmt.Errorf("compile config schema: %w", err)
	}
	return schemaMap, compiled, nil
}

func decodeYAML(data []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, nil
	}

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var values map[string]any
	if err := decoder.Decode(&values); err != nil {
		return nil, fmt.Errorf("decode YAML config: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("decode YAML config: multiple documents are not allowed")
		}
		return nil, fmt.Errorf("decode trailing YAML document: %w", err)
	}
	if values == nil {
		return map[string]any{}, nil
	}
	return values, nil
}

func defaultsFromSchema(schema map[string]any) map[string]any {
	result := make(map[string]any)
	properties, _ := schema["properties"].(map[string]any)
	for name, rawProperty := range properties {
		property, _ := rawProperty.(map[string]any)
		if value, ok := property["default"]; ok {
			result[name] = cloneJSONValue(value)
			continue
		}
		if property["type"] == "object" {
			children := defaultsFromSchema(property)
			if len(children) > 0 {
				result[name] = children
			}
		}
	}
	return result
}

func cloneJSONValue(value any) any {
	switch current := value.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(current))
		for key, child := range current {
			cloned[key] = cloneJSONValue(child)
		}
		return cloned
	case []any:
		cloned := make([]any, len(current))
		for index, child := range current {
			cloned[index] = cloneJSONValue(child)
		}
		return cloned
	default:
		return value
	}
}

func mergeMaps(destination, source map[string]any) {
	for key, sourceValue := range source {
		sourceMap, sourceIsMap := sourceValue.(map[string]any)
		destinationMap, destinationIsMap := destination[key].(map[string]any)
		if sourceIsMap && destinationIsMap {
			mergeMaps(destinationMap, sourceMap)
			continue
		}
		destination[key] = sourceValue
	}
}

type field struct {
	path       []string
	schemaType string
}

func collectFields(schema map[string]any) (map[string]field, error) {
	fields := make(map[string]field)
	var walk func(map[string]any, []string) error
	walk = func(current map[string]any, parent []string) error {
		properties, ok := current["properties"].(map[string]any)
		if !ok {
			return errors.New("object schema has no properties")
		}
		for name, rawProperty := range properties {
			property, ok := rawProperty.(map[string]any)
			if !ok {
				return fmt.Errorf("property %q is not an object", name)
			}
			path := append(append([]string(nil), parent...), name)
			typeName, _ := property["type"].(string)
			if typeName == "object" {
				if err := walk(property, path); err != nil {
					return err
				}
				continue
			}
			if typeName == "" {
				return fmt.Errorf("property %q has no scalar type", strings.Join(path, "."))
			}
			fields[strings.Join(path, ".")] = field{path: path, schemaType: typeName}
		}
		return nil
	}
	if err := walk(schema, nil); err != nil {
		return nil, err
	}
	return fields, nil
}

func applyEnvironment(values map[string]any, fields map[string]field, environ []string) error {
	environmentFields := make(map[string]field, len(fields))
	for _, item := range fields {
		name := environmentPrefix + strings.ToUpper(strings.Join(item.path, "__"))
		environmentFields[name] = item
	}

	for _, entry := range environ {
		name, value, found := strings.Cut(entry, "=")
		if !found || !strings.HasPrefix(name, environmentPrefix) {
			continue
		}
		item, ok := environmentFields[name]
		if !ok {
			return fmt.Errorf("unknown XTUNNEL environment variable %q", name)
		}
		parsed, err := parseOverride(value, item.schemaType)
		if err != nil {
			return fmt.Errorf("parse environment variable %s: %w", name, err)
		}
		setPath(values, item.path, parsed)
	}
	return nil
}

func applyCLI(values map[string]any, fields map[string]field, overrides map[string]string) error {
	for name, value := range overrides {
		item, ok := fields[name]
		if !ok {
			return fmt.Errorf("unknown CLI config override %q", name)
		}
		parsed, err := parseOverride(value, item.schemaType)
		if err != nil {
			return fmt.Errorf("parse CLI config override %s: %w", name, err)
		}
		setPath(values, item.path, parsed)
	}
	return nil
}

func parseOverride(value, schemaType string) (any, error) {
	switch schemaType {
	case "string":
		return value, nil
	case "integer":
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("expected integer: %w", err)
		}
		return parsed, nil
	case "number":
		parsed, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return nil, fmt.Errorf("expected number: %w", err)
		}
		return parsed, nil
	case "boolean":
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return nil, fmt.Errorf("expected boolean: %w", err)
		}
		return parsed, nil
	case "array":
		var parsed []any
		if err := json.Unmarshal([]byte(value), &parsed); err != nil {
			return nil, fmt.Errorf("expected JSON array: %w", err)
		}
		return parsed, nil
	default:
		return nil, fmt.Errorf("unsupported schema type %q", schemaType)
	}
}

func setPath(values map[string]any, path []string, value any) {
	current := values
	for _, segment := range path[:len(path)-1] {
		next, ok := current[segment].(map[string]any)
		if !ok {
			next = make(map[string]any)
			current[segment] = next
		}
		current = next
	}
	current[path[len(path)-1]] = value
}
