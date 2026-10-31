// Package contract embeds the JSON Schemas of the panel↔node contract and
// of @remnawave/node-plugins, both exported from the TS sources' zod schemas
// (see contract_test.go for how to regenerate them). Request bodies and
// plugin configs are validated against them, so validation follows the
// same rules the TS node's ZodValidationPipe applies.
package contract

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed schemas.json
var schemasJSON []byte

//go:embed node-plugins.schema.json
var pluginSchemaJSON []byte

var (
	once     sync.Once
	requests map[string]*jsonschema.Schema // by command name
	plugin   *jsonschema.Schema
	loadErr  error
)

func load() {
	var raw map[string]struct {
		Request json.RawMessage `json:"request"`
	}
	if loadErr = json.Unmarshal(schemasJSON, &raw); loadErr != nil {
		return
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	requests = map[string]*jsonschema.Schema{}
	for name, cmd := range raw {
		if len(cmd.Request) == 0 || string(cmd.Request) == "null" {
			continue
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(cmd.Request))
		if err != nil {
			loadErr = err
			return
		}
		url := "mem://" + name + ".json"
		if err := c.AddResource(url, doc); err != nil {
			loadErr = err
			return
		}
		if requests[name], err = c.Compile(url); err != nil {
			loadErr = fmt.Errorf("%s: %w", name, err)
			return
		}
	}
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

// Issue is one validation problem, shaped like a zod issue.
type Issue struct {
	Path    string `json:"path"`
	Message string `json:"message"`
}

func validate(s *jsonschema.Schema, body []byte) []Issue {
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte("{}")
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(body))
	if err != nil {
		return []Issue{{"", err.Error()}}
	}
	err = s.Validate(v)
	if err == nil {
		return nil
	}
	ve, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return []Issue{{"", err.Error()}}
	}
	var out []Issue
	var walk func(e *jsonschema.ValidationError)
	walk = func(e *jsonschema.ValidationError) {
		if len(e.Causes) == 0 {
			out = append(out, Issue{Path: strings.Join(e.InstanceLocation, "."), Message: e.Error()})
		}
		for _, c := range e.Causes {
			walk(c)
		}
	}
	walk(ve)
	return out
}

// ValidateRequest checks a request body against command's request schema.
// Commands without a request schema accept anything.
func ValidateRequest(command string, body []byte) []Issue {
	once.Do(load)
	if loadErr != nil {
		return []Issue{{"", "contract schemas failed to load: " + loadErr.Error()}}
	}
	s, ok := requests[command]
	if !ok {
		return nil
	}
	return validate(s, body)
}

// ValidatePlugin checks a plugin config against NodePluginSchema.
func ValidatePlugin(config []byte) []Issue {
	once.Do(load)
	if loadErr != nil {
		return []Issue{{"", "plugin schema failed to load: " + loadErr.Error()}}
	}
	return validate(plugin, config)
}
