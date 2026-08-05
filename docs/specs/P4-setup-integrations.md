# P4 — `calvoproxy setup <herramienta>`

Arquitectura de referencia: [ARCHITECTURE-6.md](../ARCHITECTURE-6.md).

## 1. Problema

`doctor` sabe comprobar que Hermes está bien cableado, pero **solo comprueba**: cuando falla,
imprime el bloque y te deja pegarlo a mano. Y solo sabe de Hermes. La misma lógica —detectar
la instalación, saber cuál es el bloque correcto, verificar que tomó efecto— vale para Claude
Code y Codex, que son los otros dos clientes que este proxy sirve a diario.

## 2. Contrato

```go
type Integration interface {
    Name() string
    ConfigPath() string                       // "" si no se encuentra la herramienta
    Render(baseURL string) string             // el bloque que debe existir
    Current(path, baseURL string) state       // missing | stale | configured
    Apply(path, baseURL string) (backup string, err error)
    Verify(baseURL string) checkResult        // round-trip real contra el proxy
}
```

`Apply` devuelve la ruta del backup para que `--revert` sepa qué restaurar.

```
calvoproxy setup <hermes|claude-code|codex> [--apply] [--revert] [--url URL]
calvoproxy setup --list
```

**`--check` es el modo por defecto y no escribe nada.** Escribir en el fichero de otro
programa es la única operación destructiva de todo el plan, así que el defecto informa y solo
`--apply` toca el disco.

## 3. Reglas duras de escritura

1. **Backup siempre antes de tocar**, en `<config-dir>/calvoproxy/backups/<tool>-<ts>.bak`.
   `--revert` restaura el más reciente.
2. **Nunca round-trip de un parser sobre formatos con comentarios.** El TOML de Codex se
   parchea por bloque delimitado con marcadores; solo el JSON de Claude Code —que no tiene
   comentarios— se lee, se modifica y se reescribe, y aun así preservando el resto de claves.
3. **Idempotencia.** Aplicar dos veces no duplica nada y la segunda vez informa de que ya
   estaba configurado.
4. **Hermes se queda en solo-lectura.** Su YAML se inspecciona con una heurística line-wise
   ([doctor.go:101](../../cmd/doctor.go)) y *una heurística que lee no debe escribir*:
   `Apply` imprime el bloque y devuelve `errApplyNotSupported`. Es una decisión, no una
   carencia — está en la interfaz para que se vea.

## 4. Bloques por herramienta

**Claude Code** (`~/.claude/settings.json`) — habla el dialecto Anthropic contra
`/v1/messages`, que el proxy ya sirve:

```json
{"env": {"ANTHROPIC_BASE_URL": "http://127.0.0.1:8080", "ANTHROPIC_AUTH_TOKEN": "dummy"}}
```

**Codex** (`~/.codex/config.toml`) — proveedor OpenAI-compatible:

```toml
# >>> calvoproxy >>>
model_provider = "calvoproxy"
[model_providers.calvoproxy]
name = "CalvoProxy"
base_url = "http://127.0.0.1:8080/v1"
wire_api = "chat"
# <<< calvoproxy <<<
```

**Hermes** (`config.yaml`) — el bloque que ya conoce `hermesConfigBlock`
([doctor.go:75](../../cmd/doctor.go)), impreso para pegar.

## 5. Invariantes verificables

| # | Invariante | Cómo se prueba |
|---|---|---|
| 1 | `--check` no escribe nunca, ni con el fichero presente ni ausente | mtime y contenido intactos |
| 2 | `--apply` deja backup restaurable | el backup existe y es byte a byte el original |
| 3 | `--apply` conserva las claves ajenas del JSON | settings con otras claves; se afirma que siguen ahí |
| 4 | `--apply` es idempotente | dos pasadas; el resultado es idéntico y la segunda dice "ya configurado" |
| 5 | El TOML conserva comentarios y contenido previo | config con comentarios; se afirma que sobreviven |
| 6 | `--revert` restaura el original byte a byte | aplicar y revertir; comparación exacta |
| 7 | Hermes nunca escribe, ni con `--apply` | fichero intacto y salida con el bloque |
| 8 | Herramienta desconocida → error claro, código 2, sin pánico | `setup inexistente` |
| 9 | Sin config detectada, informa y no crea el fichero a ciegas | HOME vacío |

## 6. Fuera de alcance

Cursor, Cline y Aider. La interfaz existe para que sean adapters, pero el valor de este corte
es validar el contrato con tres formatos distintos (YAML solo-lectura, JSON, TOML), no cubrir
catálogo.
