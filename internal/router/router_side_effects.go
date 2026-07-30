package router

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

const (
	defaultWorkspaceRoot = "/workspace/cervoclaw"
	defaultOpenCodeDB    = "/workspace/opencode_state/share/opencode.db"
)

type WorkspaceSideEffectExtractor struct {
	WorkspaceRoot string
	OpenCodeDB    string
}

func NewWorkspaceSideEffectExtractor() WorkspaceSideEffectExtractor {
	return WorkspaceSideEffectExtractor{
		WorkspaceRoot: defaultWorkspaceRoot,
		OpenCodeDB:    defaultOpenCodeDB,
	}
}

func (e WorkspaceSideEffectExtractor) Extract(ctx context.Context, content string) map[string]any {
	// 1. Detectar archivos modificados recientemente en el disco (ultimos 60s)
	// Como no sabemos que archivos toco el modelo exactamente (si no uso herramientas explicitas),
	// miramos que ha cambiado en el workspace.
	workspaceRoot := e.WorkspaceRoot
	if workspaceRoot == "" {
		workspaceRoot = defaultWorkspaceRoot
	}

	// Capturamos el snapshot actual
	_ = exec.Command("git", "-C", workspaceRoot, "add", ".").Run()
	gitHashCmd := exec.Command("git", "-C", workspaceRoot, "rev-parse", "HEAD")
	postSnapshot := ""
	if out, err := gitHashCmd.Output(); err == nil {
		postSnapshot = strings.TrimSpace(string(out))
	}

	// Obtenemos el snapshot anterior (HEAD~1 o similar)
	preSnapshot := ""
	if out, err := exec.Command("git", "-C", workspaceRoot, "rev-parse", "HEAD~1").Output(); err == nil {
		preSnapshot = strings.TrimSpace(string(out))
	}

	// Listamos archivos cambiados entre snapshots
	files := []string{}
	if preSnapshot != "" && postSnapshot != "" {
		gitDiffCmd := exec.Command("git", "-C", workspaceRoot, "diff", "--name-only", preSnapshot, postSnapshot)
		if out, err := gitDiffCmd.Output(); err == nil {
			for _, f := range strings.Split(string(out), "\n") {
				if f = strings.TrimSpace(f); f != "" {
					files = append(files, f)
				}
			}
		}
	}

	if len(files) == 0 {
		return nil
	}

	filesMetadata := []map[string]any{}
	for _, f := range files {
		fullPath := workspaceRoot + "/" + f
		fileContent := ""
		if data, err := os.ReadFile(fullPath); err == nil {
			fileContent = string(data)
		}
		oldContent := ""
		if preSnapshot != "" {
			if out, err := exec.Command("git", "-C", workspaceRoot, "show", preSnapshot+":"+f).Output(); err == nil {
				oldContent = string(out)
			}
		}

		filesMetadata = append(filesMetadata, map[string]any{
			"path":        f,
			"action":      "modified",
			"description": "Updated by AI Agent",
			"content":     fileContent,
			"original":    oldContent,
		})
	}

	// SINCRONIZACIÓN SQL PARA OPENCODE
	dbPath := e.OpenCodeDB
	if dbPath == "" {
		dbPath = defaultOpenCodeDB
	}
	if _, err := os.Stat(dbPath); err == nil {
		sqlCmd := fmt.Sprintf(`
			sqlite3 %s "
			INSERT INTO part (id, message_id, session_id, time_created, time_updated, data)
			SELECT 'proxy-start-' || hex(randomblob(4)), message.id, message.session_id, message.time_created, message.time_created, '{\"type\":\"step-start\",\"snapshot\":\"%s\"}'
			FROM message ORDER BY time_created DESC LIMIT 1;

			INSERT INTO part (id, message_id, session_id, time_created, time_updated, data)
			SELECT 'proxy-finish-' || hex(randomblob(4)), message.id, message.session_id, message.time_created + 100, message.time_created + 100, '{\"type\":\"step-finish\",\"reason\":\"stop\",\"snapshot\":\"%s\"}'
			FROM message ORDER BY time_created DESC LIMIT 1;
			"
		`, dbPath, preSnapshot, postSnapshot)
		_ = exec.Command("sh", "-c", sqlCmd).Run()
	}

	return map[string]any{
		"side_effects": map[string]any{
			"files":   filesMetadata,
			"summary": "Workspace state change detected",
		},
	}
}
