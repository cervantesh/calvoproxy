// Reemplazo sin ventana de ensure-calvoproxy.ps1 para la tarea programada
// CalvoProxy_KeepAlive. Compilado con -ldflags="-H=windowsgui" no asigna
// consola nunca, así que no hay parpadeo de ventana posible (a diferencia de
// -WindowStyle Hidden en powershell.exe, que crea la consola visible y luego
// la oculta).
//
// Sirve a los DOS llamadores, de modo que ensure-calvoproxy.ps1 ya no hace
// falta: el disparador de Task Scheduler (que nunca manda stdin) y el hook
// on_session_start de Hermes (que envia su JSON por stdin y se queda esperando
// si nadie lo lee). El drenaje es condicional por la misma razon que en el .ps1:
// leer stdin incondicionalmente bloquea para siempre bajo Task Scheduler, que
// fue como la tarea acabo atascada en "Running" con el proxy caido.
//
// Tener dos lanzadores con la misma tabla de variables copiada a mano ya costo
// una: PROXY_MAX_COMPLETION_TOKENS se subio en el .ps1 y el proceso siguio
// arrancando con el valor viejo, porque quien lo lanza es este binario.
package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const (
	calvoproxyExe = `C:\dev\calvoproxy\calvoproxy.exe`
	healthURL     = "http://127.0.0.1:8080/health"
	port          = "8080"
)

func portOpen(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func buildChildEnv() []string {
	base := os.Environ()
	env := make(map[string]string, len(base)+8)
	for _, kv := range base {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				env[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	delete(env, "OPENROUTER_API_KEY")
	env["PORT"] = port
	env["GRPC_PORT"] = "19090"
	env["OTEL_ENABLED"] = "false"
	// 8192, no 1024. Con 1024 la compactacion de contexto de Hermes no cabia en
	// su propio resumen: el resumen salia truncado con finish_reason=length, y sin
	// resumen completo no podia reducir nada, asi que la sesion moria por
	// desbordamiento tras tres intentos. Medido 2026-09-08: los tres perfiles
	// cortaban en exactamente 1024 tokens de salida pidiera lo que pidiera el
	// cliente. Sigue por debajo del OutputReserveTokens de 16384 que declaran los
	// modelos principales de la cadena.
	env["PROXY_MAX_COMPLETION_TOKENS"] = "8192"
	env["PROXY_TOOL_RESULT_LIMIT"] = "8192"
	env["PROXY_OLLAMA_URL"] = "http://127.0.0.1:11434"
	env["PROXY_REQUEST_TIMEOUT_SECONDS"] = "90"
	env["PROXY_TOTAL_TIMEOUT_SECONDS"] = "180"
	env["PROXY_STREAM_IDLE_TIMEOUT"] = "20"
	env["PROXY_STREAM_FIRST_BYTE_TIMEOUT"] = "3"

	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// drainStdinIfRedirected consume el payload del hook y nada mas. Solo cuando
// stdin viene redirigido: bajo Task Scheduler no hay nadie escribiendo y una
// lectura incondicional no termina nunca.
func drainStdinIfRedirected() {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return
	}
	if fi.Mode()&os.ModeCharDevice != 0 {
		return // consola: nadie va a escribir
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func main() {
	drainStdinIfRedirected()

	if portOpen("127.0.0.1:" + port) {
		return // ya está arriba, nada que hacer
	}

	cmd := exec.Command(calvoproxyExe)
	cmd.Env = buildChildEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | 0x00000008, // DETACHED_PROCESS
	}
	if err := cmd.Start(); err != nil {
		return
	}

	adminToken := os.Getenv("PROXY_ADMIN_TOKEN")
	client := &http.Client{Timeout: 2 * time.Second}
	for i := 0; i < 16; i++ {
		time.Sleep(500 * time.Millisecond)
		req, err := http.NewRequest(http.MethodGet, healthURL, nil)
		if err != nil {
			continue
		}
		if adminToken != "" {
			req.Header.Set("Authorization", "Bearer "+adminToken)
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		var body struct {
			Ready bool `json:"ready"`
		}
		decErr := json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if decErr == nil && body.Ready {
			break
		}
	}
}
