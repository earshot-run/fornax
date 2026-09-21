package main

// Structured output for `ask`: --json asks for any JSON object, --schema
// pins the reply to a JSON Schema read from a file ('-' = stdin). The reply
// is the payload — it alone goes to stdout so the command pipes clean.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// jsonFlag non-empty means "any JSON object"; schemaPath points at a JSON
// Schema file ("-" reads it from stdin). They are alternatives.
func runAskStructured(ctx context.Context, spec *modelSpec, eng *engineSpec, prompt, jsonFlag, schemaPath string) error {
	extra, err := structuredExtra(jsonFlag, schemaPath)
	if err != nil {
		return err
	}
	text, err := askStructured(ctx, spec, eng, prompt, extra)
	if err != nil {
		return err
	}
	fmt.Println(text)
	return nil
}

func structuredExtra(jsonFlag, schemaPath string) (map[string]any, error) {
	if jsonFlag != "" && schemaPath != "" {
		return nil, fmt.Errorf("--json and --schema are alternatives — pick one")
	}
	if schemaPath == "" {
		if jsonFlag == "" {
			return nil, nil
		}
		return map[string]any{
			"response_format": map[string]any{"type": "json_object"},
		}, nil
	}
	var data []byte
	var err error
	if schemaPath == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(schemaPath)
	}
	if err != nil {
		return nil, fmt.Errorf("could not read the schema: %w", err)
	}
	var schema any
	if err := json.Unmarshal(data, &schema); err != nil {
		return nil, fmt.Errorf("the schema is not valid JSON: %w", err)
	}
	return map[string]any{
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   "fornax",
				"schema": schema,
				"strict": true,
			},
		},
	}, nil
}

func askStructured(ctx context.Context, spec *modelSpec, eng *engineSpec, prompt string, extra map[string]any) (string, error) {
	var text string
	err := withServer(ctx, spec, eng, func(url, key string) error {
		reply, err := chatOnceFull(ctx, url, key, spec.id,
			[]message{textMessage("user", prompt)}, -1, extra)
		if err != nil {
			return err
		}
		text = strings.TrimSpace(reply.Text)
		return nil
	})
	if err != nil {
		return "", err
	}
	if text == "" {
		return "", fmt.Errorf("the model returned an empty reply")
	}
	return text, nil
}
