package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	compress "github.com/cervantesh/cervo-compress"
)

const (
	toolCompressionProtocol = "calvoproxy.tool-compression.v1"
	defaultToolResultLimit  = 4096
	maxToolResultLimit      = 1 << 20
	maxToolCompressionInput = 64 << 20
)

type toolCompressionRequest struct {
	Version         string           `json:"version"`
	Messages        []map[string]any `json:"messages"`
	ToolResultLimit int              `json:"tool_result_limit,omitempty"`
}

type toolCompressionSaving struct {
	Name       string `json:"name"`
	SavedBytes int    `json:"saved_bytes"`
}

type toolCompressionReport struct {
	OriginalBytes int                     `json:"original_bytes"`
	SavedBytes    int                     `json:"saved_bytes"`
	Engines       []string                `json:"engines"`
	ByEngine      []toolCompressionSaving `json:"by_engine"`
}

type toolCompressionResponse struct {
	Version  string                `json:"version"`
	Messages []map[string]any      `json:"messages"`
	Report   toolCompressionReport `json:"report"`
}

// runCompactTools is a local bridge for conversation owners such as Hermes.
// It does not call the proxy, a model, or the network: JSON enters on stdin,
// cervo-compress rewrites tool-result payloads in this process, and JSON leaves
// on stdout. This keeps the router stateless while non-Go clients use the
// canonical compression implementation.
func runCompactTools(args []string) int {
	return runCompactToolsWith(args, os.Stdin, os.Stdout, os.Stderr)
}

func runCompactToolsWith(args []string, in io.Reader, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("compact-tools", flag.ContinueOnError)
	fs.SetOutput(errOut)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(errOut, "usage: calvoproxy compact-tools < request.json")
		return 2
	}

	var request toolCompressionRequest
	decoder := json.NewDecoder(io.LimitReader(in, maxToolCompressionInput+1))
	decoder.UseNumber()
	if err := decoder.Decode(&request); err != nil {
		fmt.Fprintf(errOut, "invalid compact-tools request: %v\n", err)
		return 2
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		fmt.Fprintln(errOut, "invalid compact-tools request: expected one JSON object")
		return 2
	}
	if request.Version != toolCompressionProtocol {
		fmt.Fprintf(errOut, "unsupported compact-tools version %q\n", request.Version)
		return 2
	}
	limit := request.ToolResultLimit
	if limit == 0 {
		limit = defaultToolResultLimit
	}
	if limit < 0 || limit > maxToolResultLimit {
		fmt.Fprintf(errOut, "tool_result_limit must be between 1 and %d bytes\n", maxToolResultLimit)
		return 2
	}

	messages := make([]compress.Message, len(request.Messages))
	for i, raw := range request.Messages {
		messages[i] = compressionMessageFromMap(raw)
	}
	compressed, report := compress.Pipeline(messages, compress.Recommended(limit)...)
	response := toolCompressionResponse{
		Version:  toolCompressionProtocol,
		Messages: make([]map[string]any, len(compressed)),
		Report: toolCompressionReport{
			OriginalBytes: report.OriginalBytes,
			SavedBytes:    report.SavedBytes,
			Engines:       report.Engines,
			ByEngine:      make([]toolCompressionSaving, len(report.ByEngine)),
		},
	}
	for i, message := range compressed {
		response.Messages[i] = compressionMessageToMap(message)
	}
	for i, saving := range report.ByEngine {
		response.Report.ByEngine[i] = toolCompressionSaving{Name: saving.Name, SavedBytes: saving.SavedBytes}
	}
	if response.Report.Engines == nil {
		response.Report.Engines = []string{}
	}

	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(response); err != nil {
		fmt.Fprintf(errOut, "could not encode compact-tools response: %v\n", err)
		return 1
	}
	return 0
}

func compressionMessageFromMap(raw map[string]any) compress.Message {
	extra := make(map[string]any, len(raw))
	for key, value := range raw {
		if key != "role" && key != "content" {
			extra[key] = value
		}
	}
	role, _ := raw["role"].(string)
	return compress.Message{Role: role, Content: raw["content"], Extra: extra}
}

func compressionMessageToMap(message compress.Message) map[string]any {
	out := make(map[string]any, len(message.Extra)+2)
	for key, value := range message.Extra {
		out[key] = value
	}
	out["role"] = message.Role
	out["content"] = message.Content
	return out
}
