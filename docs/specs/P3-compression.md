# P3 — Compresión de peticiones

Arquitectura de referencia: [ARCHITECTURE-6.md](../ARCHITECTURE-6.md).

## 1. Problema, y por qué este es el punto peligroso

Las cargas de agente reenvían el historial entero en cada turno, y lo que más lo infla son los
resultados de herramientas: un `cat` de un fichero grande viaja otra vez en cada turno
posterior, para siempre. Contra un free tier con cupo por peticiones, eso no cuesta dinero
pero sí contexto útil.

**Este es el único punto del plan que modifica la petición del usuario**, y por tanto el único
que puede degradar respuestas *en silencio*. Todo el diseño está subordinado a eso.

## 2. Qué NO se hace

- **Dedup de sesión entre turnos: descartado.** El upstream es stateless. No reenviar el
  contexto no lo comprime: hace que el modelo deje de verlo. Eso es amnesia, no compresión, y
  un LRU con hash de prefijo no lo arregla — el problema no es recordar el prefijo, es que hay
  que mandarlo igual.
- **Poda semántica de prosa: descartada en v1.** "Semántico, determinista y sin ML" es una
  contradicción práctica: preservar código byte a byte mientras se poda texto exige delimitar
  código en Markdown arbitrario, y un fence mal cerrado por el modelo convierte la poda en
  corrupción.

## 3. Los dos motores que quedan

**`toolcap`** — recorta el contenido de mensajes `role: "tool"` que superen el límite,
conservando el principio y el final con un marcador explícito en medio. Los dos extremos
porque un resultado de herramienta puede llevar la información al principio (un fichero) o al
final (el error de un comando), y quedarse con uno solo elige mal la mitad de las veces.

**Nunca toca un resultado que sea JSON válido.** Recortar JSON produce JSON inválido, y un
resultado corrupto es peor que uno largo.

**`dedup`** — dentro del historial **de esta misma petición**, las copias repetidas de un
bloque idéntico se sustituyen por una referencia a la copia que sí viaja. Determinista por
hash de contenido, sin estado y sin persistencia.

**La última aparición siempre sobrevive intacta.** Es la que el modelo está mirando; sustituir
esa por una referencia dejaría al modelo sin el contenido justo cuando lo necesita.

## 4. Enganche

Una sola pasada en `dispatchChain`, antes de `executeFallbacks` — **nunca dentro del bucle**,
que ya re-serializa por intento y multiplicaría el coste por el número de modelos.

```go
type Compressor interface {
    Name() string
    Apply(body map[string]any) (map[string]any, compressionStat)
}
```

**Devuelve un mapa nuevo, nunca muta el de entrada**, porque `execution.RequestBody["model"]`
([router_fallback.go:107](../../internal/router/router_fallback.go)) escribe sobre el mapa
compartido.

## 5. Interruptores, todos conservadores

| Variable | Defecto | Qué hace |
|---|---|---|
| `PROXY_COMPRESS_PROFILES` | *(vacío)* | perfiles con compresión. **Vacío = apagado del todo** |
| `PROXY_COMPRESS_DRYRUN` | `false` | calcula el ahorro y **no aplica nada** |
| `PROXY_COMPRESS_TOOL_LIMIT` | `4096` | bytes por encima de los cuales se recorta un tool result |

**Apagado por defecto** es deliberado: nadie debería descubrir que su proxy comprime porque
una respuesta salió peor.

Ante **cualquier** error de un motor, o si el ahorro queda por debajo del umbral, se reenvía
el cuerpo original intacto.

## 6. Invariantes verificables

| # | Invariante | Cómo se prueba |
|---|---|---|
| 1 | Sin perfiles configurados, el cuerpo sale idéntico | comparación byte a byte |
| 2 | Nunca muta el mapa de entrada | se guarda copia y se compara tras aplicar |
| 3 | `toolcap` no toca un tool result que sea JSON válido | resultado JSON largo |
| 4 | `toolcap` conserva principio y final, y marca el recorte | contenido largo no-JSON |
| 5 | `toolcap` no toca mensajes que no sean `role: tool` | mensaje de usuario largo |
| 6 | `dedup` deja intacta la **última** aparición | tres copias; la tercera sobrevive |
| 7 | `dedup` no toca bloques que solo aparecen una vez | historial sin repeticiones |
| 8 | `dry-run` reporta ahorro y no aplica nada | cuerpo idéntico, stat > 0 |
| 9 | Un cuerpo sin `messages` o con formas raras no revienta | mensajes nulos, tipos mezclados |
| 10 | El ahorro llega a la traza como `cmp=` | header tras comprimir |

## 7. Fuera de alcance

Comprimir la respuesta (el cliente la necesita entera), contar tokens reales en vez de bytes
(exigiría un tokenizador vendorizado por modelo), y cualquier motor que requiera un modelo.
