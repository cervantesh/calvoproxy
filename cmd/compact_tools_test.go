package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestCompactToolsUsesCervoCompressAndPreservesEnvelope(t *testing.T) {
	content := "HEAD\n" + strings.Repeat("progress 10%\rprogress 20%\r", 500) + "\nTAIL"
	request := toolCompressionRequest{
		Version: toolCompressionProtocol,
		Messages: []map[string]any{
			{"role": "user", "content": "keep my prompt", "name": "human"},
			{"role": "tool", "content": content, "tool_call_id": "call-1"},
		},
		ToolResultLimit: 1024,
	}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runCompactToolsWith(nil, bytes.NewReader(raw), &stdout, &stderr); code != 0 {
		t.Fatalf("compact-tools exit=%d stderr=%s", code, stderr.String())
	}

	var response toolCompressionResponse
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("invalid response: %v\n%s", err, stdout.String())
	}
	if response.Version != toolCompressionProtocol {
		t.Fatalf("version=%q", response.Version)
	}
	if response.Report.SavedBytes <= 0 || len(response.Report.Engines) == 0 {
		t.Fatalf("cervo-compress did not report a saving: %+v", response.Report)
	}
	if got := response.Messages[0]["content"]; got != "keep my prompt" {
		t.Fatalf("user content changed: %v", got)
	}
	if got := response.Messages[0]["name"]; got != "human" {
		t.Fatalf("user envelope changed: %v", got)
	}
	if got := response.Messages[1]["tool_call_id"]; got != "call-1" {
		t.Fatalf("tool envelope changed: %v", got)
	}
	compressed, _ := response.Messages[1]["content"].(string)
	if !strings.Contains(compressed, "HEAD") || !strings.Contains(compressed, "TAIL") {
		t.Fatalf("tool result lost an edge: %q", compressed)
	}
}

func TestCompactToolsReturnsOriginalWhenNothingCanBeCompressed(t *testing.T) {
	raw := `{"version":"calvoproxy.tool-compression.v1","messages":[{"role":"user","content":"hello"}]}`
	var stdout, stderr bytes.Buffer
	if code := runCompactToolsWith(nil, strings.NewReader(raw), &stdout, &stderr); code != 0 {
		t.Fatalf("compact-tools exit=%d stderr=%s", code, stderr.String())
	}
	var response toolCompressionResponse
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Report.SavedBytes != 0 || len(response.Report.Engines) != 0 {
		t.Fatalf("unexpected compression: %+v", response.Report)
	}
	if got := response.Messages[0]["content"]; got != "hello" {
		t.Fatalf("content changed: %v", got)
	}
}

func TestCompactToolsRejectsUnknownProtocolAndTrailingJSON(t *testing.T) {
	for _, input := range []string{
		`{"version":"future","messages":[]}`,
		`{"version":"calvoproxy.tool-compression.v1","messages":[]} {}`,
	} {
		var stdout, stderr bytes.Buffer
		if code := runCompactToolsWith(nil, strings.NewReader(input), &stdout, &stderr); code != 2 {
			t.Fatalf("input=%q exit=%d, want 2", input, code)
		}
		if stdout.Len() != 0 {
			t.Fatalf("invalid input wrote stdout: %s", stdout.String())
		}
	}
}

func TestCompactToolsPreservesStructuredToolResultShape(t *testing.T) {
	request := toolCompressionRequest{
		Version: toolCompressionProtocol,
		Messages: []map[string]any{{
			"role": "user",
			"content": []any{map[string]any{
				"type":        "tool_result",
				"tool_use_id": "toolu-1",
				"content":     "HEAD" + strings.Repeat("x", 9000) + "TAIL",
				"is_error":    false,
			}},
		}},
		ToolResultLimit: 1024,
	}
	raw, _ := json.Marshal(request)
	var stdout, stderr bytes.Buffer
	if code := runCompactToolsWith(nil, bytes.NewReader(raw), &stdout, &stderr); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	var response toolCompressionResponse
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	blocks, ok := response.Messages[0]["content"].([]any)
	if !ok || len(blocks) != 1 {
		t.Fatalf("structured content shape changed: %#v", response.Messages[0]["content"])
	}
	block := blocks[0].(map[string]any)
	if block["type"] != "tool_result" || block["tool_use_id"] != "toolu-1" || block["is_error"] != false {
		t.Fatalf("tool_result envelope changed: %#v", block)
	}
}
