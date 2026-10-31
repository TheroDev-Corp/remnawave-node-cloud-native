// Package contract embeds the JSON Schemas of the panel↔node contract and
// of @remnawave/node-plugins, both exported from the TS sources' zod schemas
// (see contract_test.go for how to regenerate them). Request bodies are
// validated by a zod-faithful walker (zod.go); plugin configs by a JSON
// Schema validator.
package contract

import (
	"bytes"
	_ "embed"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed schemas.json
var schemasJSON []byte

//go:embed node-plugins.schema.json
var pluginSchemaJSON []byte

var (
	once    sync.Once
	plugin  *jsonschema.Schema
	loadErr error
)

func load() {
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(pluginSchemaJSON))
	if err != nil {
		loadErr = err
		return
	}
	if err := c.AddResource("mem://node-plugins.json", doc); err != nil {
		loadErr = err
		return
	}
	plugin, loadErr = c.Compile("mem://node-plugins.json")
}

// PluginIssue is one plugin-config validation problem (logged, never sent).
type PluginIssue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func validate(s *jsonschema.Schema, body []byte) []PluginIssue {
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		return []PluginIssue{{"", err.Error()}}
	}
	err = s.Validate(v)
	if err == nil {
		return nil
	}
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return []PluginIssue{{"", err.Error()}}
	}
	var out []PluginIssue
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			out = append(out, PluginIssue{Path: strings.Join(e.InstanceLocation, "."), Message: e.Error()})
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	return out
}

// ValidatePlugin checks a plugin config against NodePluginSchema.
func ValidatePlugin(config []byte) []PluginIssue {
	once.Do(load)
	if loadErr != nil {
		return []PluginIssue{{"", "plugin schema failed to load: " + loadErr.Error()}}
	}
	return validate(plugin, config)
}
